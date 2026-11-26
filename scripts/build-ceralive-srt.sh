#!/usr/bin/env bash
# Build the CERALIVE SRT fork (https://github.com/CERALIVE/srt) from a pinned
# source commit into a private, self-contained bundle that the CtrlIRL-Coder
# binary links against in place of the stock Haivision libsrt.
#
# The bundle contains the shared library, the matching C headers, pkg-config
# files and a build.json metadata file (source SHA + library SHA-256). Nothing
# is installed system-wide; the app is built with -L/-I pointing at this bundle
# and a $ORIGIN rpath so it loads the bundled library at runtime.
#
# Usage:
#   scripts/build-ceralive-srt.sh [--arch amd64|arm64] [--out DIR] [--sha SHA]
#
#   --arch   Target architecture. Defaults to the host arch (amd64|arm64).
#            Cross-building requires the matching cross toolchain + gnuTLS dev.
#   --out    Output bundle directory. Default: .build/ceralive-srt/<arch>.
#   --sha    Source commit to build. Default: resolve the current master HEAD.
#
# Output: prints the bundle directory path on the last line. Emits
# "CERALIVE_LIB_SHA256=<sha>" and "CERALIVE_SOURCE_SHA=<sha>" on stderr so
# callers can capture them.
set -euo pipefail

REPO="https://github.com/CERALIVE/srt"
ARCH="$(uname -m)"            # x86_64 | aarch64
OUT=""
SRC_SHA=""
CACHE_DIR="${CERALIVE_CACHE:-$PWD/.build/ceralive-cache}"

while [ $# -gt 0 ]; do
  case "$1" in
    --arch) ARCH="$2"; shift 2 ;;
    --out)  OUT="$2"; shift 2 ;;
    --sha)  SRC_SHA="$2"; shift 2 ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

# Normalise + resolve arch.
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 2 ;;
esac

# Resolve source SHA (current master HEAD) when not pinned.
if [ -z "$SRC_SHA" ]; then
  SRC_SHA="$(git ls-remote "$REPO" refs/heads/master | awk '{print $1}')"
  [ -n "$SRC_SHA" ] || { echo "failed to resolve master SHA" >&2; exit 1; }
fi

if [ -z "$OUT" ]; then OUT="$PWD/.build/ceralive-srt/$ARCH"; fi
mkdir -p "$CACHE_DIR"

SRC_DIR="$CACHE_DIR/ceralive-srt-src"
BUILDDIR="$CACHE_DIR/build-$SRC_SHA-$ARCH"
HOST_ARCH="$(uname -m)"
CROSS=0

# Select compilers (native, or aarch64 cross when target != host).
CC="${CC:-cc}"
CXX="${CXX:-c++}"
if [ "$ARCH" = "arm64" ] && [ "$HOST_ARCH" != "aarch64" ]; then
  CROSS=1
  CC=aarch64-linux-gnu-gcc
  CXX=aarch64-linux-gnu-g++
  command -v "$CXX" >/dev/null 2>&1 || {
    echo "arm64 cross build requires $CXX (install gcc-aarch64-linux-gnu + arm64 gnuTLS dev)" >&2
    exit 1; }
  export PKG_CONFIG_PATH="${PKG_CONFIG_CROSS:-/usr/lib/aarch64-linux-gnu/pkgconfig}${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
fi

# Fetch (and reuse) the source at the exact commit.
if [ -d "$SRC_DIR/.git" ]; then
  # Reuse the cached clone; make sure it can reach the requested SHA.
  if ! git -C "$SRC_DIR" cat-file -e "$SRC_SHA" 2>/dev/null; then
    git -C "$SRC_DIR" fetch --filter=blob:none "$REPO" "$SRC_SHA"
  fi
  git -C "$SRC_DIR" checkout -q "$SRC_SHA"
else
  rm -rf "$SRC_DIR"
  git clone --filter=blob:none --no-checkout "$REPO" "$SRC_DIR"
  git -C "$SRC_DIR" checkout -q "$SRC_SHA"
fi

rm -rf "$BUILDDIR"
CMAKE_CMAKE_ARGS=(-S "$SRC_DIR" -B "$BUILDDIR")
[ "$CROSS" = 1 ] && CMAKE_CMAKE_ARGS+=(-DCMAKE_SYSTEM_PROCESSOR=aarch64)
cmake "${CMAKE_CMAKE_ARGS[@]}" \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_C_COMPILER="$CC" -DCMAKE_CXX_COMPILER="$CXX" \
  -DCMAKE_INSTALL_PREFIX=/usr \
  -DENABLE_APPS=OFF \
  -DENABLE_SHARED=ON \
  -DENABLE_STATIC=OFF \
  -DENABLE_TESTING=OFF \
  -DENABLE_UNITTESTS=OFF \
  -DUSE_ENCLIB=gnutls

cmake --build "$BUILDDIR" --parallel "$(nproc)"

STAGE="$CACHE_DIR/stage-$SRC_SHA-$ARCH"
rm -rf "$STAGE"
DESTDIR="$STAGE" cmake --install "$BUILDDIR" >/dev/null
rm -rf "$STAGE/usr/bin"

# Determine the vendor libdir (multiarch) that CMake installed into.
SRC_LIBDIR="$(cd "$STAGE" && find . -type f -name 'libsrt.so.1.5.*' | head -n1 | sed 's|/libsrt\.so.*||')"
[ -n "$SRC_LIBDIR" ] || { echo "build produced no libsrt.so.1.5.*" >&2; exit 1; }
SRC_LIBDIR="${SRC_LIBDIR#./}"   # strip leading ./
REAL_LIB="$STAGE/$SRC_LIBDIR"
# Canonical basename may be libsrt.so.1.5.<minor>.
REAL_BASENAME="$(cd "$REAL_LIB" && ls libsrt.so.1.5.* | head -n1)"

# Assemble the bundle: lib (real + symlinks), headers, pkg-config, metadata.
rm -rf "$OUT"
mkdir -p "$OUT/lib" "$OUT/include" "$OUT/pkgconfig"
cp -P "$STAGE/$SRC_LIBDIR"/*.so* "$OUT/lib/"
# Guarantee the two SONAME/dev symlinks exist.
ln -sf "$REAL_BASENAME" "$OUT/lib/libsrt.so.1.5"
ln -sf "libsrt.so.1.5"  "$OUT/lib/libsrt.so"
cp -r "$STAGE/usr/include/srt" "$OUT/include/"
if [ -d "$STAGE/$SRC_LIBDIR/pkgconfig" ]; then
  cp "$STAGE/$SRC_LIBDIR/pkgconfig"/*.pc "$OUT/pkgconfig/"
fi

LIB_SHA="$(sha256sum "$OUT/lib/$REAL_BASENAME" | awk '{print $1}')"
BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
cat > "$OUT/build.json" <<EOF
{
  "repo": "$REPO",
  "source_sha": "$SRC_SHA",
  "arch": "$ARCH",
  "backend": "gnutls",
  "soname": "libsrt.so.1.5",
  "lib_file": "lib/$REAL_BASENAME",
  "lib_sha256": "$LIB_SHA",
  "built_at": "$BUILT_AT"
}
EOF

# Drop the intermediate stage; keep the source cache for reuse.
rm -rf "$STAGE" "$BUILDDIR"

echo "CERALIVE_LIB_SHA256=$LIB_SHA" >&2
echo "CERALIVE_SOURCE_SHA=$SRC_SHA" >&2
echo "$OUT"
