#!/usr/bin/env bash
#
# Integration smoke test for CtrlIRL-Coder.
#
# Starts a local SRT listener, runs both pipeline templates against it, and
# asserts that each reaches PLAYING, SRT connects, with no GStreamer
# CRITICAL/ERROR output.
#
# Requires: bin/ctrlirl-coder-linux-amd64 (make dist-amd64), gst-launch-1.0 with srtsrc.
set -euo pipefail

cd "$(dirname "$0")/.."

BINARY=./bin/ctrlirl-coder-linux-amd64
CONF=config.toml
PORT=9101
DURATION=8

if [[ ! -x "$BINARY" ]]; then
  echo "error: $BINARY not found — run 'make dist-amd64' first" >&2
  exit 1
fi
if ! command -v gst-launch-1.0 >/dev/null 2>&1; then
  echo "error: gst-launch-1.0 not found (install gstreamer1.0-tools)" >&2
  exit 1
fi

# Pick a free port if the default is taken.
if ss -ulnp 2>/dev/null | grep -q ":$PORT"; then
  PORT=9102
fi

TMPDIR_SMOKE=$(mktemp -d)
trap 'rm -rf "$TMPDIR_SMOKE"; [[ -n "${LISTENER_PID:-}" ]] && kill "$LISTENER_PID" 2>/dev/null || true' EXIT

# Point the authoritative [srt] uri at the smoke port.
CONF_TMP="$TMPDIR_SMOKE/config.toml"
if grep -qE '^\s*uri\s*=' "$CONF"; then
  sed -E "s|(^([[:space:]]*uri[[:space:]]*=[[:space:]]*)\"?srt://[^:\"]*:)[0-9]+\??(\"?).*|\1$PORT\3|g" \
    "$CONF" >"$CONF_TMP"
else
  cp "$CONF" "$CONF_TMP"
fi

start_listener() {
  # Raw MPEG-TS capture: the payload is a TS mux, so no reframe/parse step —
  # srtsrc passes the bytes through and we just discard them.
  gst-launch-1.0 -q srtsrc "uri=srt://:$PORT?mode=listener" \
    ! fakesink > "$TMPDIR_SMOKE/listener.log" 2>&1 &
  LISTENER_PID=$!
  for _ in $(seq 1 20); do
    if ss -ulnp 2>/dev/null | grep -q ":$PORT"; then
      return 0
    fi
    sleep 0.5
  done
  echo "error: SRT listener did not come up on :$PORT" >&2
  cat "$TMPDIR_SMOKE/listener.log" >&2
  return 1
}

wait_free() {
  for _ in $(seq 1 40); do
    if ! ss -ulnp 2>/dev/null | grep -q ":$PORT"; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}

run_case() {
  local name="$1"; shift
  local pipeline="$1"; shift
  local scene="${1:-}"

  echo "=== [$name] starting listener ==="
  start_listener

  local logfile="$TMPDIR_SMOKE/$name.log"
  local args=(--config "$CONF_TMP" --pipeline "$pipeline")
  [[ -n "$scene" ]] && args+=(--scene "$scene")

  echo "=== [$name] running $BINARY ${args[*]} ==="
  set +e
  timeout --signal=INT "$DURATION" "$BINARY" "${args[@]}" > "$logfile" 2>&1
  local rc=$?
  set -e

  if [[ $rc -ne 124 && $rc -ne 0 ]]; then
    echo "FAIL [$name]: exited with $rc (expected 124=SIGINT)" >&2
    cat "$logfile" >&2
    return 1
  fi

  if ! grep -q "pipeline playing" "$logfile"; then
    echo "FAIL [$name]: never reached 'pipeline playing'" >&2
    cat "$logfile" >&2
    return 1
  fi

  if ! grep -q "srt: connected" "$logfile"; then
    echo "FAIL [$name]: SRT never connected" >&2
    cat "$logfile" >&2
    return 1
  fi

  if grep -qiE "CRITICAL|ERROR" "$logfile"; then
    echo "FAIL [$name]: GStreamer CRITICAL/ERROR present" >&2
    grep -iE "CRITICAL|ERROR" "$logfile" >&2
    return 1
  fi

  echo "PASS [$name]"
}

# Scene for the layered case (two layers: video test pattern + text title).
cat > "$TMPDIR_SMOKE/scene.json" <<'JSON'
{
  "name": "smoke",
  "canvas": { "width": 1280, "height": 720 },
  "layers": [
    { "id": "main", "type": "video", "source": "",
      "properties": { "zorder": 1, "xpos": 0, "ypos": 0,
                      "width": 1280, "height": 720, "alpha": 1.0 } },
    { "id": "title", "type": "text", "text": "CtrlIRL",
      "properties": { "zorder": 5, "xpos": 32, "ypos": 640,
                      "width": 400, "height": 80, "alpha": 1.0 } }
  ]
}
JSON

echo "### Layered pipeline (compositor + 2 scene layers) ###"
run_case "layered" pipeline/example-layered.pipeline "$TMPDIR_SMOKE/scene.json"
wait_free

echo "### Full pipeline (single source, no compositor) ###"
run_case "full" pipeline/example-full.pipeline ""
wait_free

echo
echo "All integration smoke cases passed."
