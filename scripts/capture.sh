#!/bin/sh
# Capture an encoder's SRT output to a file.
#   usage: capture.sh PORT OUT DUR ENCODER [args...]
# Starts a GStreamer SRT listener on $PORT writing to $OUT, runs the encoder
# (SRT caller) as the last args, lets it stream for DUR seconds, then stops
# both. Prints the captured byte count.
set -u
PORT="$1"; OUT="$2"; DUR="${3:-8}"; shift 3
KILL=/bin/kill
rm -f "$OUT"

gst-launch-1.0 -q srtsrc uri=srt://:"$PORT" mode=listener latency=200 ! filesink location="$OUT" 2>/dev/null &
LISTEN=$!
sleep 1

"$@" >/dev/null 2>&1 &
ENC=$!

sleep "$DUR"
"$KILL" "$ENC" 2>/dev/null
sleep 0.6
"$KILL" "$LISTEN" 2>/dev/null
wait 2>/dev/null
echo "captured $(stat -c%s "$OUT" 2>/dev/null || echo 0) bytes -> $OUT"
