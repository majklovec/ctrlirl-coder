# CtrlIRL-Coder — build, test, and packaging.
#
# Single build (cgo): GStreamer + the CERALIVE SRT fork. There is no "no-GStreamer"
# or "no-SRT" variant — the app owns the SRT socket and pulls bytes from the
# pipeline's terminal appsink, so the binary always needs GStreamer (core + the
# app library, libgstapp) AND a linked SRT lib.
#
# SRT is the CERALIVE fork (https://github.com/CERALIVE/srt), built from a pinned
# source commit into a private bundle (.build/ceralive-srt/<arch>) by
# scripts/build-ceralive-srt.sh. The binary links that bundle (-L/-I + a $ORIGIN
# rpath) and embeds the library's SHA-256 + source commit so it refuses to run
# over a stock/different libsrt at startup. Nothing is installed system-wide.
#
# amd64 builds natively (needs the local GStreamer dev headers + the CERALIVE
# build deps: cmake, a C++ compiler, gnuTLS dev). arm64 is produced by the Docker
# build (make docker / docker-arm64). A bare `go build` is NOT a valid way to build
# this binary: it would resolve -lsrt to a stock system libsrt and the runtime
# guard would reject it.

GO       ?= go
PKG      := ./cmd/ctrlirl-coder
IMG      ?= ctrlirl-coder

# CERALIVE SRT bundle (built by the helper). The Go binary links against it and
# embeds its identity, so any `go build`/`go test` in this repo must point cgo at
# it via CGO_CFLAGS/CGO_LDFLAGS (set per-target below).
BUNDLE_DIR  := .build/ceralive-srt/amd64
BUNDLE_JSON := $(BUNDLE_DIR)/build.json
SRT_PKG     := ctrlirl-coder/internal/sinks/srt
# CGO flags that bind cgo to the bundled fork + its headers, and load it next to
# the binary ($ORIGIN) rather than from a system path.
CERALIVE_CGO_LDFLAGS := -L$(BUNDLE_DIR)/lib -lsrt -Wl,-rpath,\$$ORIGIN -Wl,--enable-new-dtags
CERALIVE_CGO_CFLAGS  := -I$(BUNDLE_DIR)/include
# Embed the built fork's identity (CERALIVE_LIB_SHA / CERALIVE_SRC_SHA are set in
# each recipe from the fresh bundle before the go build runs).

# Native amd64 prerequisites (Debian/Ubuntu):
#   sudo apt install -y gcc pkg-config cmake c++ libgnutls28-dev nettle-dev \
#       libglib2.0-dev gstreamer1.0-dev \
#       libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev
#
# Scene filters (runtime, not build) live across three plugin sets —
# base (videoscale, volume), good (videocrop, videobalance, videobox,
# videoflip, effectv, equalizer-nbands, audiodynamic), bad (rotate =
# "videorotate" kind, pitch):
#   sudo apt install -y gstreamer1.0-plugins-base gstreamer1.0-plugins-good \
#       gstreamer1.0-plugins-bad
#
# The `browser` layer additionally needs (runtime, not build):
#   sudo apt install -y chromium gstreamer1.0-plugins-pulse \
#       pipewire-pulse libpulse0 pulseaudio-utils
#   (Chrome/Chromium binary via $CTRLIRL_CHROME_BIN or on PATH; a
#    Pulse/PipeWire user session so page audio has a sink to live on.)

.PHONY: all build \
        dist dist-amd64 dist-arm64 bundle-amd64 \
        test vet test-integration test-stream test-obs \
        docker docker-arm64 deb deb-amd64 deb-arm64 clean

all: dist-amd64

## Dry-run: print what `make dist` would execute, build nothing.
build:
	$(MAKE) -n dist

## Build the CERALIVE SRT fork bundle for the host arch (cached source per commit).
bundle-amd64:
	bash scripts/build-ceralive-srt.sh --arch amd64 --out $(BUNDLE_DIR)

## All dist binaries (amd64 native; arm64 via Docker). All outputs land in bin/.
dist: dist-amd64 dist-arm64

## Native amd64 binary: links the CERALIVE bundle, embeds its identity, and ships
## the runtime lib next to the binary ($ORIGIN rpath). cgo: needs GStreamer dev
## headers + the CERALIVE build deps (cmake, c++, gnuTLS dev).
dist-amd64: | bundle-amd64
	@sha=$$(sed -n 's/.*"lib_sha256": "\([0-9a-f]*\)".*/\1/p' $(BUNDLE_JSON)); \
	src=$$(sed -n 's/.*"source_sha": "\([0-9a-f]*\)".*/\1/p' $(BUNDLE_JSON)); \
	if [ -z "$$sha" ] || [ -z "$$src" ]; then echo "CERALIVE bundle identity missing; run: make bundle-amd64" >&2; exit 1; fi; \
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
	  CGO_CFLAGS="$(CERALIVE_CGO_CFLAGS)" CGO_LDFLAGS="$(CERALIVE_CGO_LDFLAGS)" \
	  $(GO) build -ldflags "-X $(SRT_PKG).ceraliveLibSHA256=$$sha -X $(SRT_PKG).ceraliveSourceSHA=$$src" \
	  -o bin/ctrlirl-coder-linux-amd64 $(PKG); \
	libfile=$$(sed -n 's/.*"lib_file": "\([^\"]*\)".*/\1/p' $(BUNDLE_JSON)); \
	cp -f $(BUNDLE_DIR)/$$libfile bin/libsrt.so.1.5; \
	cp -f $(BUNDLE_JSON) bin/

## arm64 image via buildx (emulated on non-arm64 hosts). The Dockerfile builds the
## arm64 CERALIVE bundle in-band. Extracts the binary to bin/ctrlirl-coder-linux-arm64.
dist-arm64: docker-arm64

## Host arch normalized to the dist target name (dist-amd64 / dist-arm64).
ifneq ($(filter aarch64 arm64,$(shell uname -m)),)
HOST_DEB_ARCH := arm64
else
HOST_DEB_ARCH := amd64
endif

## Build a .deb for the HOST arch. Builds the host dist first (CERALIVE bundle
## + binary in bin/), then packages it. Needs dpkg-dev. Output:
## dist/ctrlirl-coder_<version>_<arch>.deb. Bundles the CERALIVE libsrt next to
## the binary and declares the GStreamer/gnutls/PipeWire deps from
## packaging/deps.txt.
deb: dist-$(HOST_DEB_ARCH)
	bash scripts/build-deb.sh

## amd64 .deb (builds the dist binary first).
deb-amd64: dist-amd64
	bash scripts/build-deb.sh --arch amd64

## arm64 .deb (on a non-arm64 host the script builds the arm64 Docker image
## via buildx and extracts the binary + CERALIVE libsrt from it).
deb-arm64:
	bash scripts/build-deb.sh --arch arm64

## Run the full Go test suite (cgo: GStreamer dev headers + the CERALIVE bundle,
## since the srt package links the fork). The test binary embeds the same
## CERALIVE identity as the dist binary so the runtime backend guard passes.
test: | bundle-amd64
	@sha=$$(sed -n 's/.*"lib_sha256": "\([0-9a-f]*\)".*/\1/p' $(BUNDLE_JSON)); \
	src=$$(sed -n 's/.*"source_sha": "\([0-9a-f]*\)".*/\1/p' $(BUNDLE_JSON)); \
	if [ -z "$$sha" ] || [ -z "$$src" ]; then echo "CERALIVE bundle identity missing; run: make bundle-amd64" >&2; exit 1; fi; \
	CGO_ENABLED=1 CGO_CFLAGS="$(CERALIVE_CGO_CFLAGS)" \
	CGO_LDFLAGS="$(CERALIVE_CGO_LDFLAGS) -Wl,-rpath,$(abspath $(BUNDLE_DIR)/lib)" \
	  $(GO) test -ldflags "-X $(SRT_PKG).ceraliveLibSHA256=$$sha -X $(SRT_PKG).ceraliveSourceSHA=$$src" ./...

## Vet (cgo: same prerequisites as test).
vet: | bundle-amd64
	CGO_ENABLED=1 CGO_CFLAGS="$(CERALIVE_CGO_CFLAGS)" CGO_LDFLAGS="$(CERALIVE_CGO_LDFLAGS)" \
	  $(GO) vet ./...

## Integration smoke: layered + full pipelines against a local srtsrc listener.
## Requires the gstreamer binary and gst-launch-1.0 with srtsrc.
test-integration: dist-amd64
	@bash scripts/integration-smoke.sh

## SRT streaming test: full pipeline -> local listener on 127.0.0.1:7000.
## Env overrides: PORT, DURATION, MIN_BYTES, KEEP_TMP=1 (see the script).
test-stream: dist-amd64
	@bash scripts/test-stream.sh

## Stream the amd64 binary into a running OBS SRT Input on 127.0.0.1:7000
test-obs: dist-amd64
	@bash scripts/test-obs.sh

## Build the Docker image (GStreamer + libsrt dev), amd64.
docker:
	docker build -t $(IMG) .

## Build the arm64 Docker image via buildx. On an amd64 host the go
## binary comes from the native `cross` stage (qemu cgo is dead: it hangs
## on go-glib); on an arm64 host it builds natively.
docker-arm64:
	@case "$$(uname -m)" in \
		aarch64|arm64) \
			docker buildx build --platform linux/arm64 -t $(IMG):arm64 --load . ;; \
		*) \
			docker buildx build --platform linux/arm64 --build-arg SRC=cross -t $(IMG):arm64 --load . ;; \
	esac

## Clean build artifacts (binaries + the CERALIVE bundle/cache).
clean:
	rm -rf bin .build
