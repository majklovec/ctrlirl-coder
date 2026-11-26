# syntax=docker/dockerfile:1
#
# CtrlIRL-Coder — multi-stage build + runtime image.
#
# glib headers must declare GPathBuf (go-gst/go-glib cgo preamble uses it).
# libglib2.0-dev only ships gpathbuf.h from glib >= 2.78 — bookworm's glib
# 2.74 lacks it, so EVERY go-glib cgo build in a bookworm container fails
# (native amd64 AND cross). Hence golang:1.26-trixie (glib 2.84).
#
# SRC selects the source stage for the runtime image: `build` (native,
# TARGETARCH == host) or `cross` (amd64 host building arm64). Must be before
# the first FROM to be in scope for a FROM instruction.
ARG SRC=build

# ---- build dependencies (shared by `build` and `cross`) ----
# $BUILDPLATFORM = host arch: the toolchain stages always run natively
# (qemu cgo is dead); only the runtime stage may be emulated (pure apt).
FROM --platform=$BUILDPLATFORM golang:1.26-trixie AS builddeps

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        g++ \
        ca-certificates \
        cmake \
        git \
        libglib2.0-dev \
        libgstreamer1.0-dev \
        libgstreamer-plugins-base1.0-dev \
        libgnutls28-dev \
        nettle-dev && \
    rm -rf /var/lib/apt/lists/*

# ---- native build (use when TARGETARCH matches the build host; on an
#      amd64 host prefer the `cross` target — qemu-emulated arm64 cgo in an
#      image is dead: 10+ min hangs, then "could not determine what
#      C.<sym> refers to". Never `--platform linux/arm64` from amd64.) ----
FROM builddeps AS build

WORKDIR /src
COPY . /src

ARG TARGETARCH
# CERALIVE SRT bundle in its own step: cached per platform, re-runs only
# when scripts/build-ceralive-srt.sh or vendor/SRT change.
RUN set -eux; \
    bash scripts/build-ceralive-srt.sh --arch "${TARGETARCH}" --out /src/.build/ceralive-srt/${TARGETARCH}

RUN set -eux; \
    BUNDLE=/src/.build/ceralive-srt/${TARGETARCH}; \
    sha=$(sed -n 's/.*"lib_sha256": "\([0-9a-f]*\)".*/\1/p' "${BUNDLE}/build.json"); \
    src=$(sed -n 's/.*"source_sha": "\([0-9a-f]*\)".*/\1/p' "${BUNDLE}/build.json"); \
    [ -n "${sha}" ] && [ -n "${src}" ]; \
    # $ORIGIN must reach the linker literally. BuildKit pre-expands `$` in
    # RUN (turns "\$ORIGIN" into "$ORIGIN" for /bin/sh -> dies under set -u,
    # and turns "$$ORIGIN" into "1ORIGIN"), so build the dollar via command
    # substitution — `$(printf ...)` is left for the shell: ORIGIN ends the
    # step as literal "$ORIGIN".
    ORIGIN="$(printf '%s' '$')ORIGIN"; \
    CGO_ENABLED=1 \
      CGO_CFLAGS="-I${BUNDLE}/include" \
      CGO_LDFLAGS="-L${BUNDLE}/lib -lsrt -Wl,-rpath,${ORIGIN} -Wl,--enable-new-dtags" \
      go build -ldflags "-X ctrlirl-coder/internal/sinks/srt.ceraliveLibSHA256=${sha} -X ctrlirl-coder/internal/sinks/srt.ceraliveSourceSHA=${src}" \
      -o /out/ctrlirl-coder ./cmd/ctrlirl-coder; \
    libfile=$(sed -n 's/.*"lib_file": "\([^"]*\)".*/\1/p' "${BUNDLE}/build.json"); \
    cp "${BUNDLE}/${libfile}" /out/libsrt.so.1.5; \
    cp "${BUNDLE}/build.json" /out/build.json

# ---- cross build from an amd64 host -> arm64 go binary (native compiler,
#      no qemu) ----
FROM builddeps AS cross

RUN set -eux; \
    dpkg --add-architecture arm64; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        g++-aarch64-linux-gnu \
        libgnutls28-dev:arm64 \
        libglib2.0-dev:arm64 \
        libgstreamer1.0-dev:arm64 \
        libgstreamer-plugins-base1.0-dev:arm64 \
        nettle-dev:arm64; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY . /src

RUN set -eux; \
    bash scripts/build-ceralive-srt.sh --arch arm64 --out /src/.build/ceralive-srt/arm64

RUN set -eux; \
    BUNDLE=/src/.build/ceralive-srt/arm64; \
    sha=$(sed -n 's/.*"lib_sha256": "\([0-9a-f]*\)".*/\1/p' "${BUNDLE}/build.json"); \
    src=$(sed -n 's/.*"source_sha": "\([0-9a-f]*\)".*/\1/p' "${BUNDLE}/build.json"); \
    [ -n "${sha}" ] && [ -n "${src}" ]; \
    ORIGIN="$(printf '%s' '$')ORIGIN"; \
    # PKG_CONFIG_PATH: cgo runs `pkg-config gstreamer-1.0/glib-2.0` per
    # directive — pointed at the arm64 .pc files. Explicit CGO_CFLAGS/
    # LDFLAGS add the arch include/lib dirs on top (public headers are
    # arch-neutral under /usr/include, config headers under
    # /usr/lib/aarch64-linux-gnu).
    PKG_CONFIG_PATH=/usr/lib/aarch64-linux-gnu/pkgconfig \
    CGO_ENABLED=1 \
      GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
      CGO_CFLAGS="-I${BUNDLE}/include -I/usr/include/glib-2.0 -I/usr/lib/aarch64-linux-gnu/glib-2.0/include" \
      CGO_LDFLAGS="-L${BUNDLE}/lib -L/usr/lib/aarch64-linux-gnu -lsrt -Wl,-rpath,${ORIGIN} -Wl,--enable-new-dtags" \
      go build -ldflags "-X ctrlirl-coder/internal/sinks/srt.ceraliveLibSHA256=${sha} -X ctrlirl-coder/internal/sinks/srt.ceraliveSourceSHA=${src}" \
      -o /out/ctrlirl-coder ./cmd/ctrlirl-coder; \
    libfile=$(sed -n 's/.*"lib_file": "\([^"]*\)".*/\1/p' "${BUNDLE}/build.json"); \
    cp "${BUNDLE}/${libfile}" /out/libsrt.so.1.5; \
    cp "${BUNDLE}/build.json" /out/build.json

# ---- runtime ----
# FROM expansion of a global ARG is the supported indirection (COPY --from
# does not expand variables): SRC=build (native) or SRC=cross (arm64 from an
# amd64 host).
FROM ${SRC} AS built

# The runtime stage defaults to $TARGETPLATFORM, so it matches the binary's
# arch even when built on a foreign-arch host (only its apt/useradd RUN
# emulates).
FROM debian:trixie-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        gstreamer1.0-plugins-base \
        gstreamer1.0-plugins-good \
        gstreamer1.0-plugins-ugly \
        gstreamer1.0-plugins-bad \
        gstreamer1.0-pulseaudio \
        # textoverlay (text layers) lives in gstreamer1.0-x (libgstpango)
        gstreamer1.0-x \
        gstreamer1.0-libav && \
    rm -rf /var/lib/apt/lists/*

RUN useradd --create-home --shell /usr/sbin/nologin ctrlirl

# The binary links libsrt.so.1.5 by SONAME with RUNPATH $ORIGIN, so the
# CERALIVE fork lib + build.json must sit NEXT TO the binary; the SHA pair
# is baked in at build time and verified at startup, so the loader is never
# offered a gnutls-based system libsrt.
COPY --from=built /out/ctrlirl-coder /usr/local/bin/ctrlirl-coder
COPY --from=built /out/libsrt.so.1.5 /usr/local/bin/libsrt.so.1.5
COPY --from=built /out/build.json /usr/local/bin/build.json

USER ctrlirl
ENTRYPOINT ["/usr/local/bin/ctrlirl-coder"]
