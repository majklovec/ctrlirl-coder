#!/usr/bin/env bash
# Build a Debian package for the CtrlIRL-Coder live SRT encoder.
#
# The package self-contains everything the binary needs at runtime:
#   * the Go binary            -> /usr/local/bin/ctrlirl-coder
#   * the CERALIVE libsrt fork -> /usr/local/bin/libsrt.so.1.5   (bundled;
#     the binary carries an $ORIGIN rpath, so no ldconfig/system libsrt)
#   * example config/scene     -> /etc/ctrlirl-coder/
#   * pipeline templates       -> /usr/share/ctrlirl-coder/pipeline/
#
# System dependencies (GStreamer plugins, gnutls/nettle for the bundled SRT,
# Pulse/PipeWire for the browser layer) are declared in packaging/deps.txt and
# written into the package Depends.
#
# Prereqs (Debian/Ubuntu, host arch):
#   sudo apt install -y dpkg-dev
#   make dist-<arch>         # binary + libsrt + build.json in bin/
#
# Prereqs (host amd64, packaging for arm64) — native cross-compile, no docker:
#   sudo dpkg --add-architecture arm64 && sudo apt update
#   sudo apt install -y g++-aarch64-linux-gnu libgnutls28-dev:arm64 \
#       libglib2.0-dev:arm64 libgstreamer1.0-dev:arm64 \
#       libgstreamer-plugins-base1.0-dev:arm64 nettle-dev:arm64
#
# Fallback for arm64 (no native cross toolchain): docker + buildx
# (emulated, slow).
#
# Usage:
#   scripts/build-deb.sh [--arch amd64|arm64] [--version V] [--maintainer S]
#
# Output: dist/ctrlirl-coder_<version>_<arch>.deb
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Normalise the host arch to a Debian name (used to pick the artifact source).
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
x86_64) HOST_ARCH=amd64 ;;
aarch64) HOST_ARCH=arm64 ;;
esac

ARCH="$HOST_ARCH"
VERSION=""
MAINTAINER="${MAINTAINER:-CtrlIRL Developers <dev@ctrlirl-coder.local>}"

while [ $# -gt 0 ]; do
	case "$1" in
	--arch)
		ARCH="$2"
		shift 2
		;;
	--version)
		VERSION="$2"
		shift 2
		;;
	--maintainer)
		MAINTAINER="$2"
		shift 2
		;;
	-h | --help)
		sed -n '2,30p' "$0"
		exit 0
		;;
	*)
		echo "unknown arg: $1" >&2
		exit 2
		;;
	esac
done
case "$ARCH" in amd64 | arm64) : ;; *)
	echo "unsupported arch: $ARCH" >&2
	exit 2
	;;
esac

# Artifact source. Host arch: the bin/ dist outputs (make dist-<arch>).
# Cross arch: native cross-compile on the host when the toolchain is present
# (fast, deterministic); otherwise fall back to the Docker buildx image,
# which carries the binary + CERALIVE libsrt + build.json under /out/
# (emulated on non-arm64 hosts — slow).
if [ "$ARCH" = "$HOST_ARCH" ]; then
	BIN="$ROOT/bin/ctrlirl-coder-linux-$ARCH"
	SRTLIB="$ROOT/bin/libsrt.so.1.5"
	BJSON="$ROOT/bin/build.json"
	for f in "$BIN" "$SRTLIB" "$BJSON"; do
		[ -f "$f" ] || {
			echo "missing $f — run: make dist-$ARCH" >&2
			exit 1
		}
	done
elif [ "$ARCH" = "arm64" ] && command -v aarch64-linux-gnu-g++ >/dev/null 2>&1; then
	echo "cross-compiling natively for arm64 (aarch64-linux-gnu-gcc)…"
	BUNDLE="$ROOT/.build/ceralive-srt/arm64"
	bash "$ROOT/scripts/build-ceralive-srt.sh" --arch arm64 --out "$BUNDLE"
	sha="$(sed -n 's/.*"lib_sha256": "\([0-9a-f]*\)".*/\1/p' "$BUNDLE/build.json")"
	src="$(sed -n 's/.*"source_sha": "\([0-9a-f]*\)".*/\1/p' "$BUNDLE/build.json")"
	{ [ -n "$sha" ] && [ -n "$src" ]; } || {
		echo "CERALIVE bundle identity missing; the source build failed" >&2
		exit 1
	}
	EXTRACT="$(mktemp -d)"
	trap 'rm -rf "$EXTRACT"' EXIT
	PKG_CONFIG_PATH=/usr/lib/aarch64-linux-gnu/pkgconfig \
		CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
		CGO_CFLAGS="-I$BUNDLE/include" \
		CGO_LDFLAGS="-L$BUNDLE/lib -lsrt -Wl,-rpath,\$ORIGIN -Wl,--enable-new-dtags" \
		go build -ldflags "-X ctrlirl-coder/internal/sinks/srt.ceraliveLibSHA256=$sha -X ctrlirl-coder/internal/sinks/srt.ceraliveSourceSHA=$src" \
		-o "$EXTRACT/ctrlirl-coder" ./cmd/ctrlirl-coder
	libfile="$(sed -n 's/.*"lib_file": "\([^"]*\)".*/\1/p' "$BUNDLE/build.json")"
	cp "$BUNDLE/$libfile" "$EXTRACT/libsrt.so.1.5"
	cp "$BUNDLE/build.json" "$EXTRACT/build.json"
	BIN="$EXTRACT/ctrlirl-coder"
	SRTLIB="$EXTRACT/libsrt.so.1.5"
	BJSON="$EXTRACT/build.json"
else
	# Docker fallback (e.g. arm64 target from a host without the cross
	# toolchain). Never emulates: on a non-arm64 host it builds the
	# amd64-native -> arm64 `cross` stage (BuildKit skips the unused
	# native `build` stage); on an arm64 host it builds natively.
	command -v docker >/dev/null 2>&1 ||
		{
			echo "cross-arch packaging needs the cross toolchain (gcc-aarch64-linux-gnu + arm64 GStreamer/gnutls/nettle dev) or docker (host is $HOST_ARCH, target $ARCH)" >&2
			exit 1
		}
	CTRIMG="ctrlirl-coder:$ARCH-cross"
	if [ "$HOST_ARCH" = "arm64" ]; then
		echo "building $CTRIMG (native arm64)…"
		docker build --target cross -t "$CTRIMG" "$ROOT"
	else
		echo "building $CTRIMG (amd64 host -> arm64 cross stage)…"
		docker build --target cross -t "$CTRIMG" "$ROOT"
	fi
	CTR="$(docker create "$CTRIMG")"
	EXTRACT="$(mktemp -d)"
	trap 'docker rm -f "$CTR" >/dev/null 2>&1; rm -rf "$EXTRACT"' EXIT
	docker cp "$CTR:/out/ctrlirl-coder" "$EXTRACT/ctrlirl-coder"
	docker cp "$CTR:/out/libsrt.so.1.5" "$EXTRACT/libsrt.so.1.5"
	docker cp "$CTR:/out/build.json" "$EXTRACT/build.json"
	BIN="$EXTRACT/ctrlirl-coder"
	SRTLIB="$EXTRACT/libsrt.so.1.5"
	BJSON="$EXTRACT/build.json"
fi

# Version: explicit > git describe (tag or short head) > 0.1.0. Sanitised to
# Debian-version-legal characters (alphanumerics, '.', '+', '-').
if [ -z "$VERSION" ]; then
	if git -C "$ROOT" describe --tags --always 2>/dev/null | grep -q .; then
		VERSION="$(git -C "$ROOT" describe --tags --always | tr '/' '-' | tr -cd 'A-Za-z0-9.+-')"
	else
		VERSION="0.1.0"
	fi
fi
# Strip a leading "v" (git tags are v-prefixed; dpkg versions are not).
VERSION="${VERSION#v}"
# dpkg requires the version to start with a digit; a bare git sha (untagged
# repo) does not — prefix it.
case "$VERSION" in
[0-9]*) : ;;
*) VERSION="0.$VERSION" ;;
esac
# Debian wants a -revision; append one when absent.
case "$VERSION" in
*-*) : ;;
*) VERSION="${VERSION}-1" ;;
esac

# Debian package name is lowercase, no arch suffix.
PKG="ctrlirl-coder"
STAGE="$ROOT/.build/deb/${PKG}_${VERSION}_${ARCH}"
rm -rf "$STAGE"
mkdir -p "$STAGE/DEBIAN" \
	"$STAGE/usr/local/bin" \
	"$STAGE/etc/$PKG" \
	"$STAGE/usr/share/$PKG/pipeline" \
	"$STAGE/usr/lib/systemd/system"

# --- payload ---
install -m 0755 "$BIN" "$STAGE/usr/local/bin/ctrlirl-coder"
# Host strip cannot handle a cross-arch ELF; use the target binutils strip.
STRIP_BIN=strip
[ "$ARCH" = "arm64" ] && STRIP_BIN=aarch64-linux-gnu-strip
[ "$ARCH" = "amd64" ] && [ "$HOST_ARCH" = "arm64" ] && STRIP_BIN=x86_64-linux-gnu-strip
$STRIP_BIN --strip-unneeded "$STAGE/usr/local/bin/ctrlirl-coder" 2>/dev/null ||
	echo "warn: strip failed (non-fatal)" >&2
install -m 0644 "$SRTLIB" "$STAGE/usr/local/bin/libsrt.so.1.5" # $ORIGIN rpath
install -m 0644 "$BJSON" "$STAGE/usr/local/bin/build.json"     # CERALIVE identity
install -m 0644 "$ROOT/config.toml.example" "$STAGE/etc/$PKG/config.toml"
install -m 0644 "$ROOT/scene.example.json" "$STAGE/etc/$PKG/scene.example.json"
install -m 0644 "$ROOT"/pipeline/*.pipeline "$STAGE/usr/share/$PKG/pipeline/"

# Declare the config as a dpkg conffile (preserved / prompted-on conflict).
echo "/etc/$PKG/config.toml" >"$STAGE/DEBIAN/conffiles"

# --- control ---
DEP_PKGS="$(tr -d ' \t' <"$ROOT/packaging/deps.txt" | sed '/^#/d; /^\s*$/d' | tr '\n' ' ')"

# Runtime shared-lib deps, derived from the binary's NEEDEDs + the bundled
# libsrt's NEEDEDs. libsrt is bundled (dropped). 'libglib2.0-0 |
# libglib2.0-0t64' covers both Debian (libglib2.0-0) and Ubuntu >= 24.04
# (libglib2.0-0t64). The gstapp library ships in libgstapp-1.0-0 on
# bookworm but was merged into gstreamer1.0-plugins-base on newer
# releases — a parenthesized alternative keeps both resolvable. libm.so.6
# and libgcc_s.so.1 are satisfied by the glibc and libstdc++6 packages
# listed.
# (No parens: dpkg 1.23 chokes on group syntax in control; a bare 'a | b'
# relation is valid and sorts as one token through the comma joiner.)
LIB_DEPS="libc6,libglib2.0-0 | libglib2.0-0t64,libgobject-2.0-0,libgstreamer1.0-0,libgstapp-1.0-0|gstreamer1.0-plugins-base,libgnutls30,libnettle8,libstdc++6"

# Join into a Depends line. Split LIB_DEPS on commas (the `|` alternative group
# has no internal comma, so it stays one token), append the runtime package
# list, trim, de-dupe, and join with ", ".
DEP_ALL="$(
	{
		printf '%s\n' "$LIB_DEPS" | tr ',' '\n'
		printf '%s\n' "$DEP_PKGS" | tr ' ' '\n'
	} |
		sed 's/^[ \t]*//; s/[ \t]*$//' | sed '/^$/d' | sort -u |
		awk 'NR>1{printf ", "}{printf "%s",$0}END{print ""}'
)"

SHORTDESC="CtrlIRL live SRT encoder"
# Single-paragraph description: this host's dpkg (1.23.7) rejects even a
# single-space "blank" line inside Description, so keep it one paragraph.
LONGDESC="Go rewrite of the C ctrlirl_coder live encoder. GStreamer pipeline with a Go-owned SRT socket (the CERALIVE SRT fork, bundled), adaptive bitrate, multi-layer scenes, JSON-RPC control over HTTPS plus /metrics. The package ships the binary and the CERALIVE libsrt together under /usr/local/bin (the binary uses an \$ORIGIN rpath, so no system libsrt is needed). GStreamer plugin sets, gnutls/nettle, and PipeWire/Pulse for the browser layer are declared as dependencies. Configure in /etc/ctrlirl-coder/config.toml (SIGHUP to live-reload); scene example at /etc/ctrlirl-coder/scene.example.json; pipeline templates in /usr/share/ctrlirl-coder/pipeline. Start via 'systemctl start ctrlirl-coder' or run 'ctrlirl-coder --config /etc/ctrlirl-coder/config.toml'."

cat >"$STAGE/DEBIAN/control" <<EOF
Package: $PKG
Version: $VERSION
Architecture: $ARCH
Maintainer: $MAINTAINER
Installed-Size: $(du -sk "$STAGE" | awk '{print $1}')
Depends: $DEP_ALL
Section: video
Priority: optional
Description: $SHORTDESC
$(printf '%s\n' "$LONGDESC" | fold -s -w 76 | sed 's/^/ /')
EOF

echo "$PKG — version $VERSION ($ARCH)"
echo "  bundled libsrt: $(sed -n 's/.*"lib_sha256": "\([0-9a-f]*\)".*/\1/p' "$BJSON")"
echo "  source sha:     $(sed -n 's/.*"source_sha": "\([0-9a-f]*\)".*/\1/p' "$BJSON")"
echo "  depends: $DEP_ALL"

OUT="$ROOT/dist/ctrlirl-coder_${VERSION}_${ARCH}.deb"
mkdir -p "$ROOT/dist"
dpkg-deb --build --root-owner-group -Zgzip "$STAGE" "$OUT" 2>&1 | sed 's/^/  /'

rm -rf "$STAGE"
echo "$OUT"
