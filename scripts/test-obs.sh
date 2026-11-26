#!/usr/bin/env bash
#
# test-obs.sh — stream ctrlirl-coder to an already-running OBS instance.
#
# Unlike test-stream.sh (which spins up its own gst-launch listener), this
# script expects OBS to be running with an *SRT Input* source listening on
# 127.0.0.1:$PORT (default 7000). It then streams the bundled MPEG-TS pipeline
# into it using the prebuilt binary: the pipeline's byte-tap hands the TS
# bytes to the Go SRT sender, which connects to OBS as the SRT caller. It
# asserts the app side stayed clean and SRT connected.
#
# OBS setup (one-time):
#   Sources → + → "SRT Input" → Transport URL: srt://localhost:7000
#   (OBS listens on UDP 7000; the app connects as the SRT caller.)
#
# Usage:
#   scripts/test-obs.sh                        # defaults: 60 s
#   DURATION=120 scripts/test-obs.sh           # stream for 2 minutes
#
# Env overrides:
#   PORT       OBS SRT Input port (default 7000)
#   DURATION   streaming seconds before SIGINT (default 60)
#   BINARY     amd64 binary (default bin/ctrlirl-coder-linux-amd64, rebuilt if missing or stale)
#   CONFIG     TOML config (default config.toml)
#   PIPELINE   pipeline template (default pipeline/example-layered.pipeline;
#              must contain a compositor element when a scene is used)
#   SCENE      scene JSON (default: generated test scene — test video + HLS
#              video source + 1 browser layer (headless Chrome overlay of a
#              generated local page) + 2 pango text overlays; the text layers
#              exercise textoverlay options: font-desc, color, outline-color,
#              draw-outline/draw-shadow, shaded-background/shading-value,
#              halignment/valignment, xpad/ypad, deltax, wrap-mode)
#
# While streaming, a background loop pushes the 'hls' layer 10 px right,
# once per second, over the app's shared HTTP API (POST /rpc, --api-addr).
# Asserts ~1/s live layer.set ops applied.

set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${PORT:-7000}"
DURATION="${DURATION:-10}"
BINARY="${BINARY:-bin/ctrlirl-coder-linux-amd64}"
CONFIG="${CONFIG:-config.toml}"
PIPELINE="${PIPELINE:-pipeline/example-layered.pipeline}"
SCENE="${SCENE:-}"

PAGE_URL="https://seznam.cz"

TMPDIR_T="$(mktemp -d)"
cleanup() { rm -rf "$TMPDIR_T"; }
# Shared HTTP API listener (JSON-RPC /rpc + /metrics).
API_ADDR="${API_ADDR:-127.0.0.1:8899}"

#PAGE_URL="file://$TMPDIR_T/browser-page.html"
trap cleanup EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# Test scene: test video layers, an HLS video layer, a browser layer, and two
# pango text overlays (textoverlay painting each text into a transparent
# BGRA surface, composited as alpha surfaces). The text-options exercise the
# element's styling properties. The browser layer points at a generated local
# page rendered by the headless-Chrome overlay. Overwritten via SCENE env.
if [[ -z "$SCENE" ]]; then
	SCENE="$TMPDIR_T/test-scene.json"
	cat >"$TMPDIR_T/browser-page.html" <<'PAGE'
<!doctype html>
<html>
<head>
<meta charset="utf-8">
<style>
  body { margin: 0; width: 100vw; height: 100vh; box-sizing: border-box;
         font-family: sans-serif; color: #fff;
         display: flex; flex-direction: column; justify-content: space-between;
         background: linear-gradient(160deg, #08243a, #0d4b5c); }
  .clock { font-size: 72px; font-weight: bold; padding: 40px; }
  .label { font-size: 28px; opacity: 0.85; padding: 0 40px 40px; }
</style>
</head>
<body>
  <div class="clock" id="c"></div>
  <div class="label">CtrlIRL browser overlay — live page</div>
  <script>
    const c = document.getElementById('c');
    const tick = () => c.textContent = new Date().toLocaleTimeString();
    tick(); setInterval(tick, 250);
  </script>
</body>
</html>
PAGE
	cat >"$SCENE" <<'EOF'
{
  "name": "test-obs",
  "canvas": { "width": 1280, "height": 720 },
  "layers": [
    {
      "id": "main",
      "type": "video",
      "source": "",
      "properties": { "zorder": 1, "xpos": 0, "ypos": 0, "width": 1280, "height": 720, "alpha": 1.0 }
    },
    {
      "id": "secondary",
      "type": "video",
      "source": "",
      "properties": { "zorder": 2, "xpos": 0, "ypos": 0, "width": 640, "height": 360, "alpha": 1.0 },
	  "filters": [
		{ "kind": "videorotate", "params": { "angle": -45 } }
      ]
    },
    {
      "id": "hls",
      "type": "video",
      "source": "https://test-streams.mux.dev/x36xhzz/x36xhzz.m3u8",
      "properties": { "zorder": 3, "xpos": 640, "ypos": 360, "width": 640, "height": 360, "alpha": 1.0 },
	  "filters": [
        { "kind": "videocrop", "params": { "top": 100, "bottom": 100 } },
        { "kind": "videobalance", "params": { "brightness": 0.05, "saturation": 1.1 } },
		{ "kind": "videorotate", "params": { "angle": 45 } }
      ]
    },
    {
      "id": "browser",
      "type": "browser",
      "source": "__PAGE_URL__",
      "properties": { "zorder": 4, "xpos": 0, "ypos": 360, "width": 640, "height": 360, "alpha": 1.0, "framerate": 30 }
    },
    {
      "id": "title",
      "type": "text",
      "text": "CTRL IRL",
      "text-options": {
        "font-desc": "Sans Bold 56",
        "color": "#00E5FF",
        "outline-color": "#000000",
        "draw-outline": "true",
        "draw-shadow": "false",
        "halignment": "left",
        "valignment": "top",
        "xpad": "64",
        "ypad": "48"
      },
      "properties": { "zorder": 5, "xpos": 0, "ypos": 20, "width": 1280, "height": 700 },
	  	  "filters": [
		{ "kind": "videorotate", "params": { "angle": 30 } }
      ]
    },
    {
      "id": "subtitle",
      "type": "text",
      "text": "second overlay — shaded background",
      "text-options": {
        "font-desc": "Sans 28",
        "color": "#FFFFFF",
        "shaded-background": "true",
        "shading-value": "160",
        "halignment": "left",
        "valignment": "top",
        "xpad": "64",
        "ypad": "140",
        "deltax": "4",
        "wrap-mode": "word"
      },
      "properties": { "zorder": 6, "xpos": 0, "ypos": 0 }
    }
  ]
}
EOF
	sed -i "s|__PAGE_URL__|$PAGE_URL|" "$SCENE"
fi

# --- 0. Prerequisites -------------------------------------------------------
[[ -f "$CONFIG" ]] || fail "config not found: $CONFIG"
[[ -f "$PIPELINE" ]] || fail "pipeline template not found: $PIPELINE"
[[ -f "$SCENE" ]] || fail "scene JSON not found: $SCENE"

# Build via Makefile when the binary does not exist or is older than the
# newest source file (a stale binary silently runs old code — e.g. without
# the filter chain).
needs_build=1
if [[ -x "$BINARY" ]]; then
	newest_src=$(find cmd internal -name '*.go' -newer "$BINARY" -print -quit 2>/dev/null)
	[[ -z "$newest_src" ]] && needs_build=0
fi
if [[ $needs_build -eq 1 ]]; then
	echo "building $BINARY (make dist-amd64)..."
	if [[ "$BINARY" == "bin/ctrlirl-coder-linux-amd64" ]]; then
		make dist-amd64 || fail "make dist-amd64 failed"
	else
		go build -o "$BINARY" ./cmd/ctrlirl-coder || fail "build failed"
	fi
fi

# OBS (or any UDP listener) must already be listening on :PORT.
if ! ss -ulnp 2>/dev/null | grep -q ":$PORT "; then
	echo "No UDP listener on 127.0.0.1:$PORT detected." >&2
	echo "Start OBS and add an *SRT Input* source with" >&2
	echo "Transport URL: srt://localhost:$PORT  — then re-run." >&2
	exit 1
fi
echo "=== UDP listener present on :$PORT (assuming OBS) ==="

# The [srt] uri in the config is the authoritative destination; point it at OBS.
CONFIG_TMP="$TMPDIR_T/config.toml"
if grep -qE '^\s*uri\s*=' "$CONFIG"; then
	sed -E "s|(^([[:space:]]*uri[[:space:]]*=[[:space:]]*)\"?srt://[^:\"]*:)[0-9]+\??(\"?).*|\1$PORT\3|g" \
		"$CONFIG" >"$CONFIG_TMP"
else
	cp "$CONFIG" "$CONFIG_TMP"
fi
grep -qE "uri[[:space:]]*=[[:space:]]*\"?srt://[^:\"]*:$PORT" "$CONFIG_TMP" ||
	fail "config rewrite to port $PORT failed"

# hls layer slides right at 10 px/s over the shared HTTP API (one curl per
# request; the `for` bounds the loop to the run duration).
command -v curl >/dev/null || fail "curl not found (needed for the RPC move loop)"
(
	for x in $(seq 10 10 $((DURATION * 10))); do
		# width 1180 (not 1280): the slide reaches xpos=100, and the scene
		# canvas invariant rejects any layer rect sticking out of the canvas
		# (compositor canvas union would then dead-end the stream).
		curl -s --max-time 2 -X POST "http://$API_ADDR/rpc" \
			-H 'Content-Type: application/json' \
			-d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"layer.set\",\"params\":{\"id\":\"hls\",\"properties\":{\"zorder\":1,\"xpos\":$x,\"ypos\":0,\"width\":1180,\"height\":720,\"alpha\":1.0}}}" \
			>/dev/null 2>&1 || true
		sleep 1
	done
) &
MOVER_PID=$!

# --- 1. Stream into OBS ------------------------------------------------------
echo "=== streaming SRT MPEG-TS to OBS on 127.0.0.1:$PORT for ${DURATION}s (binary=$BINARY, scene=$SCENE) ==="
echo "    (watch the OBS source now; the app stops itself after ${DURATION}s)"
echo "    (hls layer slides right at 10px/s via the HTTP API at $API_ADDR)"
set +e
timeout --signal=INT "$DURATION" \
	"$BINARY" --config "$CONFIG_TMP" --pipeline "$PIPELINE" --scene "$SCENE" \
	--api-addr "$API_ADDR" \
	>"$TMPDIR_T/app.log" 2>&1
APP_RC=$?
set -e

kill "$MOVER_PID" 2>/dev/null || true
wait "$MOVER_PID" 2>/dev/null || true

[[ $APP_RC -eq 124 || $APP_RC -eq 0 ]] || {
	echo "--- app log ---" >&2
	cat "$TMPDIR_T/app.log" >&2
	fail "app exited with $APP_RC (expected 124=SIGINT or 0)"
}

# --- 2. Assertions -----------------------------------------------------------
grep -q "pipeline playing" "$TMPDIR_T/app.log" ||
	{
		echo "--- app log ---" >&2
		cat "$TMPDIR_T/app.log" >&2
		fail "never reached 'pipeline playing'"
	}
echo "PASS: pipeline reached PLAYING"

# The RPC move loop pushes one layer.set per second; require 3/4 of them
# (a stalled loop — e.g. a blocking client — fails this).
RPC_APPLIED=$(grep -c "rpc: applied" "$TMPDIR_T/app.log" || true)
RPC_MIN=$((DURATION * 3 / 4))
[[ "$RPC_APPLIED" -ge "$RPC_MIN" ]] ||
	{
		echo "--- app log ---" >&2
		cat "$TMPDIR_T/app.log" >&2
		fail "only $RPC_APPLIED live layer.set ops via RPC socket, want >= $RPC_MIN (move loop stalled?)"
	}
echo "PASS: $RPC_APPLIED live layer.set ops applied via RPC socket (hls layer moved, 1/s loop)"

# Liveness: the pipeline must have PRODUCED encoded frames, not merely
# reached PLAYING. A zero-frame pipeline (e.g. compositor canvas union
# mismatch dead-ending caps negotiation) passes every structural check yet
# streams black. The 1 Hz "appsink diag" line logs the cumulative frame
# counter; require at least 10 fps of production on average.
LAST_DIAG=$(grep '"appsink diag"' "$TMPDIR_T/app.log" | tail -1 || true)
DIAG_FRAMES=$(printf '%s' "$LAST_DIAG" | grep -oE 'frames=[0-9]+' | grep -oE '[0-9]+' || true)
DIAG_MIN=$((DURATION * 10))
[[ "${DIAG_FRAMES:-0}" -ge "$DIAG_MIN" ]] ||
	{
		echo "--- app log ---" >&2
		cat "$TMPDIR_T/app.log" >&2
		fail "appsink produced ${DIAG_FRAMES:-0} frames in ${DURATION}s, want >= $DIAG_MIN (pipeline dead/zero-frame? check compositor canvas vs trunk capsfilter)"
	}
echo "PASS: $DIAG_FRAMES encoded frames produced in ${DURATION}s (>= $DIAG_MIN, stream is not black)"

grep -q "srt: connected" "$TMPDIR_T/app.log" ||
	{
		echo "--- app log ---" >&2
		cat "$TMPDIR_T/app.log" >&2
		fail "SRT never connected"
	}
echo "PASS: SRT connected to OBS"

# Scene assertions: every layer attached (text layers ride the same surface
# path as images, so one check covers both).
LAYER_IDS=$(grep -oE '"id"[[:space:]]*:[[:space:]]*"[^"]+"' "$SCENE" | grep -oE '"[^"]+"$' | tr -d '"')
LAYER_COUNT=$(printf '%s\n' "$LAYER_IDS" | grep -c .)
for id in $LAYER_IDS; do
	grep -q "scene: layer attached.*id=$id" "$TMPDIR_T/app.log" ||
		{
			echo "--- app log ---" >&2
			cat "$TMPDIR_T/app.log" >&2
			fail "scene layer $id never attached"
		}
done
echo "PASS: all $LAYER_COUNT scene layers attached"

# Text layers must be built as pango textoverlay surfaces (options parsed),
# one "layer: text overlay" log line per text layer.
TEXT_LAYERS=$(grep -c '"type"[[:space:]]*:[[:space:]]*"text"' "$SCENE" || true)
BUILT_TEXT=$(grep -c "layer: text overlay" "$TMPDIR_T/app.log" || true)
[[ "$BUILT_TEXT" -eq "$TEXT_LAYERS" ]] ||
	fail "expected $TEXT_LAYERS textoverlay layers built, app log shows $BUILT_TEXT"
echo "PASS: $TEXT_LAYERS text layers built as pango textoverlay"

# Browser layers must launch the headless-Chrome overlay feeder, one
# "layer: browser overlay" log line per browser layer.
BROWSER_LAYERS=$(grep -c '"type"[[:space:]]*:[[:space:]]*"browser"' "$SCENE" || true)
BUILT_BROWSER=$(grep -c "layer: browser overlay" "$TMPDIR_T/app.log" || true)
[[ "$BUILT_BROWSER" -eq "$BROWSER_LAYERS" ]] ||
	fail "expected $BROWSER_LAYERS browser overlays running, app log shows $BUILT_BROWSER"
echo "PASS: $BROWSER_LAYERS browser layer(s) running (headless Chrome feeder)"

	CRIT_LINES="$(grep -iE 'CRITICAL|ERROR' "$TMPDIR_T/app.log" | grep -v 'gst_mini_object_unref' || true)"
	# The gst_mini_object_unref assert is a known benign shutdown race (go-gst
	# caps GC-finalizer vs element teardown, non-fatal) — everything else fails.
	[[ -z "$CRIT_LINES" ]] ||
	{
		echo "$CRIT_LINES" >&2
		fail "GStreamer CRITICAL/ERROR in app log (SRT handshake against OBS failed?)"
	}
	echo "PASS: zero CRITICAL/ERROR in app log"

grep -q "shutting down" "$TMPDIR_T/app.log" ||
	fail "no graceful-shutdown log line (expected 'shutting down')"
echo "PASS: graceful SIGINT shutdown"

echo
echo "=== PASS: streamed SRT MPEG-TS into OBS on 127.0.0.1:$PORT for ${DURATION}s ==="
echo "If OBS still shows black: check the SRT Input URL is srt://localhost:$PORT"
echo "and that OBS's *Network* settings allow UDP on that port."
