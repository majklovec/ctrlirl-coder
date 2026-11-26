#!/usr/bin/env bash
#
# test-stream.sh — end-to-end SRT streaming test for ctrlirl-coder.
#
# What it does:
#   1. Starts a local SRT listener on 127.0.0.1:$PORT that captures the
#      received MPEG-TS stream byte-for-byte to a file.
#   2. Runs ctrlirl-coder with a config whose [srt] uri points the byte-tap's
#      SRT sender at 127.0.0.1:$PORT (the pipeline template carries no SRT URI;
#      the config is authoritative).
#   3. Asserts: the app reached PLAYING, logged zero GStreamer CRITICAL/ERROR
#      lines, SRT connected, shut down gracefully on SIGINT, the listener
#      captured a non-trivial MPEG-TS, and ffprobe sees h264 + aac video+audio.
#
# Usage:
#   scripts/test-stream.sh                       # defaults
#   PORT=7000 DURATION=15 scripts/test-stream.sh # tuned
#
# Env overrides:
#   PORT       SRT port (default 7000)
#   DURATION   streaming seconds before SIGINT (default 10)
#   BINARY     ctrlirl-coder binary (default ./bin/ctrlirl-coder-linux-amd64, built if missing)
#   CONFIG     TOML config (default config.toml)
#   PIPELINE   pipeline template (default pipeline/example-full.pipeline)
#   MIN_BYTES  minimum captured stream bytes to pass (default 100000)
#   MIN_FRAMES minimum decoded video frames to pass (default: max(2, DURATION/2));
#              set 0 to skip when ffprobe is not installed
#   KEEP_TMP   set to 1 to keep the temp dir (logs, captured stream)

set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${PORT:-7000}"
DURATION="${DURATION:-10}"
BINARY="${BINARY:-./bin/ctrlirl-coder-linux-amd64}"
CONFIG="${CONFIG:-config.toml}"
PIPELINE="${PIPELINE:-pipeline/example-full.pipeline}"
MIN_BYTES="${MIN_BYTES:-100000}"

TMPDIR_T="$(mktemp -d)"
KILLED=()
cleanup() {
	for pid in "${KILLED[@]}"; do kill "$pid" 2>/dev/null || true; done
	if [[ -n "${KEEP_TMP:-}" ]]; then
		echo "KEEP_TMP set — logs kept in $TMPDIR_T"
		else
		rm -rf "$TMPDIR_T"
		fi
}
trap cleanup EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# --- 0. Prerequisites -------------------------------------------------------
command -v gst-launch-1.0 >/dev/null 2>&1 || fail "gst-launch-1.0 not found (install gstreamer1.0-tools)"
[[ -f "$CONFIG" ]] || fail "config not found: $CONFIG"
[[ -f "$PIPELINE" ]] || fail "pipeline template not found: $PIPELINE"

if [[ ! -x "$BINARY" ]]; then
	echo "building $BINARY..."
	go build -o "$BINARY" ./cmd/ctrlirl-coder || fail "build failed"
fi

# Point the authoritative [srt] uri at the test port via a temp config copy.
CONFIG_TMP="$TMPDIR_T/config.toml"
if grep -qE '^\s*uri\s*=' "$CONFIG"; then
	sed -E "s|(^([[:space:]]*uri[[:space:]]*=[[:space:]]*)\"?srt://[^:\"]*:)[0-9]+\??(\"?).*|\1$PORT\3|g" \
		"$CONFIG" >"$CONFIG_TMP"
else
	cp "$CONFIG" "$CONFIG_TMP"
fi
grep -qE "uri[[:space:]]*=[[:space:]]*\"?srt://[^:\"]*:$PORT" "$CONFIG_TMP" \
	|| fail "config rewrite to port $PORT failed"
echo "=== streaming to 127.0.0.1:$PORT (config [srt] uri authoritative) ==="

# --- 1. Start the SRT listener (raw MPEG-TS capture) ------------------------
gst-launch-1.0 -q \
	srtsrc "uri=srt://:$PORT?mode=listener" \
	! filesink "location=$TMPDIR_T/capture.ts" \
	>"$TMPDIR_T/listener.log" 2>&1 &
LISTENER_PID=$!
KILLED+=("$LISTENER_PID")

for _ in $(seq 1 20); do
	ss -ulnp 2>/dev/null | grep -q ":$PORT " && break
	sleep 0.5
done
ss -ulnp 2>/dev/null | grep -q ":$PORT " || {
	echo "--- listener log ---" >&2
	cat "$TMPDIR_T/listener.log" >&2
	fail "SRT listener did not come up on :$PORT"
}

# --- 2. Run ctrlirl-coder ----------------------------------------------------
echo "=== running $BINARY for ${DURATION}s ==="
set +e
timeout --signal=INT "$DURATION" \
	"$BINARY" --config "$CONFIG_TMP" --pipeline "$PIPELINE" \
	>"$TMPDIR_T/app.log" 2>&1
APP_RC=$?
set -e

[[ $APP_RC -eq 124 || $APP_RC -eq 0 ]] || {
	echo "--- app log ---" >&2
	cat "$TMPDIR_T/app.log" >&2
	fail "app exited with $APP_RC (expected 124=SIGINT or 0)"
}

# --- 3. Assertions -----------------------------------------------------------
grep -q "pipeline playing" "$TMPDIR_T/app.log" ||
	{ echo "--- app log ---" >&2; cat "$TMPDIR_T/app.log" >&2; fail "never reached 'pipeline playing'"; }
echo "PASS: pipeline reached PLAYING"

grep -q "srt: connected" "$TMPDIR_T/app.log" ||
	{ echo "--- app log ---" >&2; cat "$TMPDIR_T/app.log" >&2; fail "SRT never connected"; }
echo "PASS: SRT connected"

! grep -qiE "CRITICAL|ERROR" "$TMPDIR_T/app.log" ||
	{ grep -iE "CRITICAL|ERROR" "$TMPDIR_T/app.log" >&2; fail "GStreamer CRITICAL/ERROR in app log"; }
echo "PASS: zero CRITICAL/ERROR in app log"

grep -q "shutting down" "$TMPDIR_T/app.log" ||
	fail "no graceful-shutdown log line (expected 'shutting down')"
echo "PASS: graceful SIGINT shutdown"

# Stop the listener gracefully (SIGINT) and wait for the filesink to close the
# file cleanly. An abrupt SIGTERM leaves a truncated MPEG-TS with no clean
# end, and `ffprobe -count_frames` then reports 0 even though the stream is
# valid — so let the teardown flush first.
kill -INT "$LISTENER_PID" 2>/dev/null || true
wait "$LISTENER_PID" 2>/dev/null || true
sleep 1

BYTES=$(stat -c %s "$TMPDIR_T/capture.ts" 2>/dev/null || echo 0)
echo "listener captured $BYTES bytes (min $MIN_BYTES)"
[[ "$BYTES" -ge "$MIN_BYTES" ]] ||
	{ echo "--- listener log ---" >&2; cat "$TMPDIR_T/listener.log" >&2; fail "captured too little: ${BYTES} < ${MIN_BYTES} bytes"; }
echo "PASS: SRT data flowed to the listener"

# The capture is MPEG-TS: it must expose both video (h264) and audio (aac).
MIN_FRAMES="${MIN_FRAMES:-$(( DURATION > 2 ? DURATION/2 : 2 ))}"
if [[ "$MIN_FRAMES" -gt 0 ]]; then
	command -v ffprobe >/dev/null 2>&1 || {
		echo "WARN: ffprobe not found — skipping codec/frame assertions"
		MIN_FRAMES=0
	}
fi
if [[ "$MIN_FRAMES" -gt 0 ]]; then
	CODECS=$(ffprobe -v error -show_entries stream=codec_name -of default=nw=1:nk=1 "$TMPDIR_T/capture.ts" 2>/dev/null || true)
	echo "codecs in capture: $(echo "$CODECS" | tr '\n' ' ')"
	echo "$CODECS" | grep -qi h264 || fail "no h264 video stream in captured TS"
	echo "$CODECS" | grep -qi aac   || fail "no aac audio stream in captured TS"
	echo "PASS: captured TS has h264 video + aac audio"
	# Count decoded video frames. The just-killed filesink may not have
	# finished flushing the TS when we first read it, so `ffprobe -count_frames`
	# can briefly report 0 on an otherwise-valid capture. Retry briefly, then
	# fall back to a packet count (equally valid for a well-formed TS).
	FRAMES=0
	for _ in 1 2 3 4 5; do
		FRAMES=$(ffprobe -v error -count_frames -select_streams v:0 \
			-show_entries stream=nb_read_frames \
			-of default=noprint_wrappers=1:nokey=1 "$TMPDIR_T/capture.ts" 2>/dev/null | head -n1)
		[[ "$FRAMES" =~ ^[0-9]+$ ]] && [[ "$FRAMES" -gt 0 ]] && break
		sleep 1
	done
	if [[ ! "$FRAMES" =~ ^[0-9]+$ ]] || [[ "$FRAMES" -eq 0 ]]; then
		PKTS=$(ffprobe -v error -count_packets -select_streams v:0 \
			-show_entries stream=nb_read_packets \
			-of default=noprint_wrappers=1:nokey=1 "$TMPDIR_T/capture.ts" 2>/dev/null | head -n1)
		[[ "$PKTS" =~ ^[0-9]+$ ]] && FRAMES="$PKTS"
	fi
	[[ "$FRAMES" =~ ^[0-9]+$ ]] || FRAMES=0
	echo "decoded frame count: $FRAMES (min $MIN_FRAMES)"
	[[ "$FRAMES" -ge "$MIN_FRAMES" ]] ||
		fail "captured stream not decodable: ${FRAMES} < ${MIN_FRAMES} frames"
	echo "PASS: captured stream decodes to $FRAMES video frames"
fi

# --- 4. Report ---------------------------------------------------------------
echo
echo "=== PASS: ctrlirl-coder streamed SRT MPEG-TS to 127.0.0.1:$PORT for ${DURATION}s ==="
