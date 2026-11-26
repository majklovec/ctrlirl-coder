# Live OBS/SRTLA visual test

The interactive test starts two switchable scenes and streams them to an OBS
SRTLA input:

- Scene `a`: solid red `video` layer with 440 Hz sine audio.
- Scene `b`: solid blue `video` layer with 660 Hz sine audio.
- Both scenes add a movable browser layer and a transparent `testcode` overlay
  with UTC timestamp, frame counter, 1 Hz blink marker, and QR payload.

Start an OBS SRTLA input listening on `127.0.0.1:5000`, then run:

```sh
bun scripts/test-obs-interactive-srtla.ts
```

Press Space to switch scenes, arrow keys to move browser layer, PgUp/PgDn to
rotate it, `r` to reset, and `q` to stop. Scan the lower-right QR code to read
`CTRLIRL|<UTC timestamp>|f=<frame number>`.

The source URI format `testsrc://red?freq=440` or
`testsrc://blue?freq=660` is test-harness-only. QR encoder and overlay tests:

```sh
go test ./internal/layer/testcode ./internal/layer/video
```
