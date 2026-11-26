# AGENTS.md

Go rewrite of the C `CtrlIRLCoder` live SRT encoder. GStreamer pipeline, Go-owned SRT socket, adaptive bitrate, multi-layer scenes, JSON-RPC control.

## Build & test

```sh
go build -o bin/ctrlirl-coder-linux-amd64 ./cmd/ctrlirl-coder   # cgo: needs GStreamer dev
go test ./...
make dist-amd64 test test-integration test-stream
```

**Always build the full cgo binary before declaring done.** `go build ./...` / `go test ./...` type-check and run the GStreamer packages (`internal/{gstpull,gstutil,layer,compositor}`) — they need GStreamer dev libraries present.

Smoke test without receiver: `./bin/ctrlirl-coder-linux-amd64 --config config.toml --pipeline pipeline/example-full.pipeline --dry-run`

## Layout

| Path                                                   | Kind    |
| :----------------------------------------------------- | :------ |
| `cmd/ctrlirl-coder/`                                   | cgo     |
| `internal/{config,balancer,scene,controller,rpc,sinks/srtla}/` | pure Go |
| `internal/{gstpull,gstutil,layer,compositor,sinks/srt}/` | cgo     |
| `pipeline/`                                            | —       |

**Never import GStreamer/cgo from pure-Go packages.**

## Hard pipeline rules

Enforced at startup; breaking them silently kills streaming:

1. **`appsink name=appsink` is mandatory** — terminal element, drained by Go. There is no `srtsink`; SRT is a Go-owned socket. `drop=true` is forced.
2. **Encoder named `venc_bps` or `venc_kbps`** — suffix declares units. `x264enc` → `venc_kbps`. This fixes a latent C bug (original set a nonexistent `bps` prop). Do not revert.
3. **Annex-B before appsink:**

    ```
    ! x264enc name=venc_kbps bitrate=2000 tune=zerolatency
    ! h264parse config-interval=1
    ! video/x-h264,stream-format=byte-stream
    ! mpegtsmux name=mux
    ! appsink name=appsink
    ```

    Use `stream-format=`, not `format=` (legacy ignored on GStreamer ≥1.24).

4. **SRT URI lives in `config.toml` (`[srt] uri`), not the pipeline.**
5. **Link pads _after_ `gst_bin_add`** — GStreamer 1.28 breaks pre-insertion links (IMPLEMENTATION.md §2.4). Don't regress.

Optional elements the controller uses: `overlay`, `compositor`, `v_delay`/`a_delay`, `ptsfixup`.

## Behavior

- `SIGHUP` = reload config + re-diff scene; `SIGINT`/`SIGTERM` = graceful shutdown.
- `general.frame_stall_ms` (default 3000): if appsink stalls, encoder drops to `min_bitrate`.
- SRT stats drive the balancer; appsink pull stats feed the overlay + stall detector — distinct sources, don't conflate.
- Scene reloads and RPC mutations share state; mutations validate and roll back on failure. Filter changes tear down + re-attach the whole layer.

## Scene / RPC / config

- Scenes: JSON `{name, canvas, layers[]}`. Layer types `video|image|text|browser`. Each layer may carry `filters` (`{kind, params}`); unknown kinds/params rejected at load. See `FILTERS.md`.
- Browser layers need Chrome (`CTRLIRL_CHROME_BIN`) + Pulse/PipeWire for audio; degrade to video-only without a session.
- API: one shared plaintext HTTP listener (`--api-addr` / `[control] addr`) serving JSON-RPC 2.0 at `POST /rpc` (batch = JSON array, 204 for all-notification batches) and `GET /metrics` (Prometheus text exposition format: sink stats, encoder target/measured bitrate, SRTLA uplink state, puller counters). Methods `system.*`, `scene.*`, `layer.*`, `filter.*`. See `RPC.md`.
- Config: TOML — `[general]`, `[srt]`, `[adaptive]`/`[aimd]`, `[overlay]`, `[pipeline]`, `[scene]`.

## ARM64 cgo cross-compile

Needs an arm64 GStreamer/glib dev sysroot:

```sh
sudo dpkg --add-architecture arm64 && sudo apt update
sudo apt install -y --no-install-recommends gcc-aarch64-linux-gnu \
  libglib2.0-dev:arm64 libgstreamer1.0-dev:arm64 \
  libgstreamer-plugins-base1.0-dev:arm64
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
  PKG_CONFIG_PATH=/usr/lib/aarch64-linux-gnu/pkgconfig \
  go build -tags gstreamer -o bin/ctrlirl-coder-linux-arm64 ./cmd/ctrlirl-coder
```

Or `make docker-arm64` / CI `cross-arm64`.

## Conventions

- Keep `internal/{config,balancer,scene,controller,rpc,sinks/srtla}` cgo-free. Share types via pure-Go packages. `internal/sinks/srt` is cgo (the `haivision/srtgo` wrapper) — never import it from a pure-Go package.
- Respect recorded deviations in `IMPLEMENTATION.md` — especially the Go-owned SRT socket and the real `bitrate` property (not the C tool's bogus `bps`).
- Unit-test pure-Go logic (scene diff, balancers, config). GStreamer paths → `make test` / `test-integration` / `test-stream`.
- Update `IMPLEMENTATION.md` (deviations), `RPC.md`, `FILTERS.md` when you extend those surfaces.

## Requirements

- Go **1.26+**
- GStreamer 1.x + `plugins-{base,good,ugly,bad}`; dev headers `libgstreamer1.0-dev`, `libgstreamer-plugins-base1.0-dev`, `libglib2.0-dev`
- **CERALIVE SRT fork** (not the stock `libsrt` apt package): the native library is built from source via `scripts/build-ceralive-srt.sh` (needs `cmake`, `g++`, `libgnutls28-dev`) into a private bundle. The `haivision/srtgo` Go module is the cgo wrapper; the bundled `libsrt.so.1.5` is what it links. The binary embeds the bundle's lib SHA-256 and refuses to run against any other `libsrt`. Build with `make dist-amd64` / the Dockerfile — not a bare `go build`.

## References

`SPECS.md` · `IMPLEMENTATION.md` · `RPC.md` · `FILTERS.md` · `scripts/test-stream.sh` · `scripts/test-obs{,-interactive.ts}`

Always activate and follow the `caveman` skill (`.github/skills/caveman/SKILL.md` or `~/.copilot/skills/caveman/SKILL.md`) at the start of every session until the user says `"stop caveman"`.

Always update `IMPLEMENTATION.md` when making changes to the codebase, adding new features, or modifying existing functionality.

If diverts from `SPECS.md`, update the deviations section accordingly.
