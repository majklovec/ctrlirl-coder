#!/bin/sh
# Same as capture.sh but tees the encoder's stderr to LOG for inspection.
#   usage: capturelog.sh PORT OUT DUR LOG ENCODER [args...]
set -u
PORT="$1"; OUT="$2"; DUR="${3:-8}"; LOG="$4"; shift 4
KILL=/bin/kill
rm -f "$OUT" "$LOG"

gst-launch-1.0 -q srtsrc uri=srt://:"$PORT" mode=listener latency=200 ! filesink location="$OUT" 2>/dev/null &
LISTEN=$!
sleep 1

"$@" >"$LOG" 2>&1 &
ENC=$!

sleep "$DUR"
sleep 0.3
"$KILL" "$ENC" 2>/dev/null
sleep 0.6
"$KILL" "$LISTEN" 2>/dev/null
wait 2>/dev/null
echo "captured $(stat -c%s "$OUT" 2>/dev/null || echo 0) bytes -> $OUT ; log -> $LOG"
