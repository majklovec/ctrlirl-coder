# CtrlIRL-Coder

**CtrlIRL-Coder** is a Go rewrite of the original CtrlIRLCoder C tool: a
GStreamer-based live encoder with **SRT output**, an **adaptive bitrate
control loop**, an **OBS-like multi-layer scene compositor**, and **live
config/scene reload** via `SIGHUP`.

See [SPECS.md](SPECS.md) for the full specification and
[IMPLEMENTATION.md](IMPLEMENTATION.md) for implementation progress and the
recorded deviations.

## Highlights

- **SRT via a Go-owned socket + terminal `appsink`.** The pipeline ends in
  `appsink name=appsink`; Go drains it with `gst_app_sink_pull_sample`
  (`internal/gstpull`) and sends the bytes over a Go-owned SRT socket
  (`internal/sinks/srt`, cgo via the `haivision/srtgo` wrapper linked against
  the **CERALIVE SRT fork**). This is the original `ctrlirl_coder`/`belacoder`
  data path verbatim. Live SRT stats (RTT, send buffer,
  loss) drive the balancer; appsink pull stats (frames, throughput, framerate,
  stall) feed the overlay and a stall→min-bitrate emergency.
- **Adaptive bitrate.** The control loop samples SRT stats (RTT, packet loss,
  retransmissions) every 500 ms and adjusts the encoder's `bitrate` property
  with a configurable balancer: `adaptive` (faithful port of the original),
  `fixed`, or `aimd`.
- **Multi-layer scenes.** A JSON scene descriptor drives a GStreamer
  `compositor`: video, image, and text layers, each attached to a
  `sink_%u` pad with `zorder`/`xpos`/`ypos`/`width`/`height`/`alpha`.
  `SIGHUP` re-diffs the scene and applies additions/removals/moves while the
  pipeline is playing.
- **Runtime remote control.** JSON-RPC 2.0 over a shared HTTP API
  (`--api-addr`, `POST /rpc`): change layer parameters, add/remove layers,
  add/remove/update filters, and switch/save scenes live, with validation +
  rollback (see `RPC.md`). The same listener also serves `/metrics`.
- **Pure-Go core, cgo GStreamer + SRT pipeline.** Config parsing, the
  balancers, the scene diff engine, and the SRTLA aggregator are GStreamer-free
  and fully unit-tested. The pipeline packages (`internal/{gstpull,gstutil,layer,compositor}`)
  and the SRT send socket (`internal/sinks/srt`, via the cgo `haivision/srtgo`
  wrapper) are plain cgo (no build tags): building and testing them requires the
  GStreamer dev libraries **and** a build of the CERALIVE SRT fork.
- **CERALIVE SRT fork, bundled + verified.** The native SRT library is the
  [CERALIVE fork](https://github.com/CERALIVE/srt) (a SRT 1.5 fork, GnuTLS
  backend), built from a pinned source commit into a private bundle
  (`scripts/build-ceralive-srt.sh`) and linked against in place of the stock
  Haivision library. The binary embeds the bundle's library SHA-256 at build
  time and **refuses to run** if a non-CERALIVE `libsrt` is resolved at runtime.
  The library ships next to the binary (loaded via a `$ORIGIN` rpath).

## Requirements

- **Go 1.26+**
- **GStreamer 1.x** with, for the full stack:
    - `gstreamer1.0-plugins-{base,good,ugly,bad}` (SRT reception for tests is
      in `plugins-bad`; the `appsink` element comes from `plugins-base`)
    - dev headers for cgo builds: `libgstreamer1.0-dev`,
      `libgstreamer-plugins-base1.0-dev` (the `gstreamer-app-1.0` library
      `internal/gstpull` links against), `libglib2.0-dev`
    - **CERALIVE SRT fork** (not the stock `libsrt` package): built from source
      via `scripts/build-ceralive-srt.sh`, which needs `cmake`, `g++`, and the
      GnuTLS dev headers (`libgnutls28-dev`). The fork is a cgo-linked native
      library wrapped by the `haivision/srtgo` Go module.

  > The stock `libsrt`/`libsrt-dev` apt packages are intentionally **not**
  > used: CERALIVE and stock SRT share the same `libsrt.so.1.5` SONAME, so a
  > mis-linked binary would silently load whatever system library is present.
  > The bundle + `$ORIGIN` rpath + embedded-SHA runtime guard remove that risk.

## Building

```sh
# Preferred: builds the CERALIVE SRT fork, then links the binary against it,
# embeds the fork's identity, and ships the runtime lib next to the binary.
make dist-amd64
```

> A bare `go build ./cmd/ctrlirl-coder` **will not produce a working binary**:
> the SRT cgo package must link the CERALIVE fork, and the binary must embed
> the fork's library SHA-256 (a bare build leaves it empty and the binary
> refuses to start the SRT data path). Use `make dist-amd64` (or the Dockerfile).
> `make test`/`make vet` carry the same cgo flags + identity so the full suite
> runs against the fork.

Make targets: `make bundle-amd64` (build the fork bundle only),
`make dist-amd64`, `make dist` (amd64 + arm64 →
`bin/`), `make test`, `make test-integration`, `make test-stream`
(streams SRT to `127.0.0.1:7000` and verifies the listener receives data —
see [scripts/test-stream.sh](scripts/test-stream.sh)), `make docker`.

Live OBS test: `scripts/test-obs.sh` streams into a running OBS SRT Input,
and [scripts/test-obs-interactive.ts](scripts/test-obs-interactive.ts)
(`bun scripts/test-obs-interactive.ts`) does the same setup with **arrow-key**
live control of the `hls` layer over the HTTP API (10 px/keypress, `r`
reset, `q` stop).

For SRTLA source checks, [scripts/test-obs-interactive-srtla.ts](scripts/test-obs-interactive-srtla.ts)
(`bun scripts/test-obs-interactive-srtla.ts`) switches between red/440 Hz and
blue/660 Hz video scenes. Each scene includes a movable browser layer and a
timestamp/frame-counter overlay with a QR code. See [TEST.md](TEST.md).

### Cross-compiling for ARM64 (`linux/arm64`)

`bin/ctrlirl-coder-linux-<arch>` is the cgo binary. Cross-compiling
it needs an arm64 GStreamer/glib _dev_ sysroot — on Debian/Ubuntu the arm64
multiarch packages plus the cross compiler suffice (headers are shared; only
the C compiler and the `.pc` path are architecture-specific):

```sh
sudo dpkg --add-architecture arm64 && sudo apt update
sudo apt install -y --no-install-recommends \
  gcc-aarch64-linux-gnu libglib2.0-dev:arm64 \
  libgstreamer1.0-dev:arm64 libgstreamer-plugins-base1.0-dev:arm64

CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
PKG_CONFIG_PATH=/usr/lib/aarch64-linux-gnu/pkgconfig \
go build -tags gstreamer -o bin/ctrlirl-coder-linux-arm64 ./cmd/ctrlirl-coder
# or: make dist-arm64
```

Alternatives for the cgo tier: `make docker-arm64` (Docker buildx; on an
x86 host the go binary comes from the native `cross` stage of the
Dockerfile — emulated cgo is not used) or CI's `cross-arm64` job.
Smoke-checking an arm64 binary on an x86 host: `qemu-aarch64-static -L
/usr/aarch64-linux-gnu bin/ctrlirl-coder-linux-arm64 --dry-run …`.

Note: the Dockerfile builds on `golang:1.26-trixie` on purpose — go-gst's
cgo preamble needs glib's `gpathbuf.h` (glib ≥ 2.78), which bookworm's
glib 2.74 dev package does not ship.

### Packaging (`.deb`)

`make deb` (or `scripts/build-deb.sh`) builds a self-contained Debian package
for the host arch: `dist/ctrlirl-coder_<version>_<arch>.deb`. It bundles the
binary **and** the CERALIVE `libsrt.so.1.5` under `/usr/local/bin` (the
binary's `$ORIGIN` rpath + lib-SHA guard resolve with no system `libsrt`),
plus config/scene in `/etc/ctrlirl-coder` (config is a dpkg conffile), the
pipeline templates, and a systemd unit. All runtime deps (GStreamer plugin
sets, `libgnutls30`/`libnettle8` for the bundled SRT, Pulse/PipeWire for the
browser layer) are declared in `Depends` from `packaging/deps.txt`. `make
deb-amd64` / `make deb-arm64` pin the arch. An arm64 package from an amd64
host cross-compiles natively (no docker) when the arm64 toolchain is present:

```sh
sudo dpkg --add-architecture arm64 && sudo apt update
sudo apt install -y g++-aarch64-linux-gnu libgnutls28-dev:arm64 \
    libglib2.0-dev:arm64 libgstreamer1.0-dev:arm64 \
    libgstreamer-plugins-base1.0-dev:arm64 nettle-dev:arm64
make deb-arm64
```

(Without the toolchain it falls back to a docker buildx image, emulated.)

```sh
make deb
sudo dpkg -i dist/ctrlirl-coder_*.deb
systemctl enable --now ctrlirl-coder
# or manual:
ctrlirl-coder --config /etc/ctrlirl-coder/config.toml \
  --pipeline /usr/share/ctrlirl-coder/pipeline/example-full.pipeline
```

## Quick start

1. Start an SRT listener (any SRT receiver works — this is a gst-launch one):

    ```sh
    gst-launch-1.0 srtsrc uri=srt://:9101?mode=listener ! decodebin ! fakesink
    ```

2. Run CtrlIRL-Coder:

    ```sh
    ./bin/ctrlirl-coder-linux-amd64 \
      --config config.toml \
      --pipeline pipeline/example-full.pipeline
    ```

3. For the multi-layer compositor, add a scene:

    ```sh
    ./bin/ctrlirl-coder-linux-amd64 \
      --config config.toml \
      --pipeline pipeline/example-layered.pipeline \
      --scene scene.example.json
    ```

4. Validate without GStreamer (pure-Go dry run):

    ```sh
    ./bin/ctrlirl-coder-linux-amd64 --config config.toml \
      --pipeline pipeline/example-full.pipeline --dry-run
    ```

### Running as a user

- `SIGHUP` — reload the TOML config and re-diff the scene.
- `SIGINT` / `SIGTERM` — graceful shutdown (pipeline → NULL, SRT socket closed).

```sh
kill -HUP  $(pgrep -f ctrlirl-coder)   # live reload
kill -INT   $(pgrep -f ctrlirl-coder)  # stop
```

## Pipeline contract

The pipeline template (a `gst-launch`-style string, `#` comments allowed —
they are stripped before `gst_parse_launch`) is validated for the following
named elements:

| Element name                  | Required                           | Purpose                                                                                                                                                                                                                                                                                                         |
| :---------------------------- | :--------------------------------- | :-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `appsink`                     | **Mandatory** for SRT              | Terminal element the pipeline ends in (`… ! appsink name=appsink`). Go drains it with `gst_app_sink_pull_sample` and sends the bytes over the Go-owned SRT socket (`internal/sinks/srt`, `haivision/srtgo`). Same data path as the original `ctrlirl_coder`/`belacoder`. `drop=true` is forced by the contract check. |
| (no `srtsink`)                | n/a                                | SRT is **not** sent by a GStreamer element — the Go process owns the socket.                                                                                                                                                                                                                                    |
| `venc_bps` **or** `venc_kbps` | **Mandatory** for adaptive bitrate | Video encoder whose `bitrate` property is mutated. The suffix declares units: `_bps` = bits/s, `_kbps` = kbits/s. **`x264enc`'s `bitrate` is kbit/s, so an x264enc pipeline must name it `venc_kbps`.**                                                                                                         |
| `overlay`                     | Optional                           | `textoverlay` updated with live stats at the configured interval.                                                                                                                                                                                                                                               |
| `compositor`                  | Optional                           | The multi-layer compositor; scene layers attach to its `sink_%u` pads.                                                                                                                                                                                                                                          |
| `v_delay` / `a_delay`         | Optional                           | `identity` elements (`signal-handoffs=TRUE`) for A/V sync.                                                                                                                                                                                                                                                      |
| `ptsfixup`                    | Optional                           | `identity` PTS smoothe (OBS-compat); presence-only, no action.                                                                                                                                                                                                                                                  |

If the encoder is missing, adaptive bitrate is disabled and a clear error is
logged. If the `appsink` is missing from a pipeline expected to stream (or is
the wrong element type), a clear error is logged.

**H.264 framing before the appsink is mandatory for real players.** x264enc
emits AVCC (length-prefixed) NALs, while SRT receivers (e.g. OBS `srt://`
inputs) expect Annex-B start codes — without the fix below OBS shows no
video. The template must end:

```
! x264enc name=venc_kbps bitrate=2000 tune=zerolatency
! h264parse config-interval=1
! video/x-h264,stream-format=byte-stream
! mpegtsmux name=mux
! appsink name=appsink
```

The SRT destination is **not** in the pipeline string — `config.toml`
(`[srt] uri`) names it and the Go `srtgo` socket dials it. Note the caps field
is **`stream-format=`**, not the legacy `format=` — on GStreamer ≥1.24 a
`format=byte-stream` capsfilter is ignored and the stream stays AVCC. Viewing:
in OBS use an _SRT Input_ source at `srt://localhost:<port>` (OBS listens; the
app calls).

The appsink also feeds the **stats surface** the overlay and adaptive
controller consume: frames pulled/sent/dropped, bytes, throughput (kbit/s),
negotiated framerate, and a frame-production stall detector (`general.frame_stall_ms`,
default 3000 ms). While stalled, the controller drops the encoder to its
minimum bitrate (the original's zombie-encode emergency).

### Recorded spec revisions

1. **SRT via a Go-owned socket fed by the terminal `appsink`** — the original
   `ctrlirl_coder`/`belacoder` data path (`gst_app_sink_pull_sample` → `srt_send`)
   is used verbatim; `libgstreamer-app` is a build/runtime prerequisite.
2. **The original C code had a latent bitrate bug**: it set a non-existent
   `bps` property on `x264enc` (a no-op), so adaptive bitrate never applied.
   This rewrite sets the real `bitrate` property (kbit/s for `venc_kbps`), so
   adaptive bitrate genuinely works.

## Scene format

```json
{
    "name": "example",
    "canvas": { "width": 1280, "height": 720 },
    "layers": [
        {
            "id": "main",
            "type": "video",
            "source": "/dev/video0",
            "properties": {
                "zorder": 1,
                "xpos": 0,
                "ypos": 0,
                "width": 1280,
                "height": 720,
                "alpha": 1.0
            }
        },
        {
            "id": "logo",
            "type": "image",
            "source": "logo.png",
            "properties": {
                "zorder": 10,
                "xpos": 16,
                "ypos": 16,
                "width": 128,
                "height": 128,
                "alpha": 0.9
            }
        },
        {
            "id": "title",
            "type": "text",
            "text": "Hello",
            "text-options": {
                "font-desc": "Sans Bold 48",
                "color": "#00E5FF",
                "shaded-background": "true"
            },
            "properties": { "zorder": 5, "xpos": 0, "ypos": 0 }
        }
    ]
}
```

Layer types:

- **`video`** — a live source: `srt://`/`urisrt://` URI, a `/dev/videoN`
  device, a local file or HTTP/HLS URI (auto `decodebin`/`uridecodebin`),
  empty (SMPTE test pattern), or test-only `testsrc://red?freq=440` /
  `testsrc://blue?freq=660` solid-color video with a sine audio tone
  (`freq` 20–20000 Hz; default 440).
- **`image`** — a PNG/JPEG looped via `filesrc` + `image-decode`.
- **`text`** — a pango `textoverlay` painting `text` into a transparent
  surface, composited as an alpha layer. `text-options` maps directly onto
  the element's style properties (`font-desc`, `color`, `outline-color`,
  `draw-outline`, `draw-shadow`, `shaded-background`, `shading-value`,
  `halignment`, `valignment`, `line-alignment`, `xpad`, `ypad`, `deltax`,
  `deltay`, `wrap-mode`, `scale-mode`, `auto-resize`); colors are
  `#AARRGGBB` (`#RRGGBB` accepted), alignments are names
  (`left`/`center`/`right`/`top`/`bottom`/`baseline`/...) or enum numbers.
- **`browser`** — a live web page: a persistent headless Chrome (one per
  layer) loaded with `source` (`http(s)://` or `file://`), captured over the
  Chrome DevTools Protocol (`Page.captureScreenshot`) and fed in-pipeline as
  JPEG frames (`appsrc ! jpegdec`). The page's **audio** is captured on a
  per-layer Pulse/PipeWire null-sink monitor and muxed to the `mpegtsmux`
  (`mux`) as a separate AAC stream. Browser-specific `properties`:
  `framerate` (capture FPS, default 30) and `quality` (JPEG 1–100, default
  75); `width`/`height` also set the capture resolution. Requires a
  Chrome/Chromium binary (override with `CTRLIRL_CHROME_BIN`) and, for
  audio, a Pulse/PipeWire user session (`pipewire-pulse` + `pulseaudio-utils`);
  without a session the layer degrades to video-only.
- **`testcode`** — a transparent live overlay with a UTC timestamp, frame
  counter, 1 Hz blink marker, and QR code carrying the timestamp/frame
  payload. Its `width`, `height`, and `framerate` properties set the rendered
  surface.

**Filters.** Any layer can declare a `filters` array (see `FILTERS.md` for
the supported set). Video filters are spliced between the layer's source and
its `videoconvert` tail, in scene order; audio filters (browser layers only
— they are the layers that carry an audio chain) sit between the
`audioconvert` and the AAC encoder. Each entry is
`{ "kind": …, "params": { … } }`:

```json
"filters": [
  { "kind": "videocrop",   "params": { "top": 24, "bottom": 24 } },
  { "kind": "videobalance","params": { "brightness": 0.05 } },
  { "kind": "videoscale",  "params": { "width": 640, "height": 360 } },
  { "kind": "effectv",     "params": { "edge-tv": true } },
  { "kind": "volume",      "params": { "volume": 0.8, "mute": false } },
  { "kind": "equalizer",   "params": { "bands": 3, "band0": 1.5 } }
]
```

Kinds: video — `videoscale`, `videocrop`, `videobalance`, `videobox`,
`videoflip`, `videorotate`, `effectv`; audio — `volume`, `equalizer`
(resolves to `equalizer-Nbands`, band gains `band0`…`bandN-1`), `audiodynamic`
(`mode`: 0 compressor, 1 expander), `pitch`. Filters are
validated when the scene is loaded (unknown kinds/params are rejected); a
filter change on `SIGHUP` tear-downs and re-attaches the layer (chains
cannot be spliced while running). `videoscale` params land on a trailing
`capsfilter` (the element scales to whatever caps it is given).

Each layer is its own element chain (source → filters → `videoconvert` →
BGRA `capsfilter` → `queue`) linked into the `compositor`'s next `sink_%u`
pad. Chains are linked **after** their elements are added to the pipeline —
GStreamer 1.28's `gst_bin_add` breaks pad links made before insertion
(see IMPLEMENTATION.md §2.4).

## Remote control + metrics (shared HTTP API)

The runtime exposes one **HTTP** listener that carries both
the **JSON-RPC 2.0** control API (`POST /rpc`) and the **`/metrics`**
endpoint (`GET /metrics`) on the same address. Start it with `--api-addr
127.0.0.1:8899` or `[control] addr` in the config.

Methods: `system.ping` / `system.status`; `scene.get` / `scene.names` /
`scene.load` / `scene.switch` / `scene.save`; `layer.add` / `layer.remove`
/ `layer.set`; `filter.add` / `filter.remove` / `filter.set`. The `/rpc`
body is a single JSON-RPC request object or a 2.0 batch (array); an
all-notification batch returns `204 No Content`. Mutations validate the
resulting scene and roll back on pipeline failure. SIGHUP scene reloads and
`/rpc` requests share the same scene state. See `RPC.md` for the full
reference and examples.

`/metrics` exposes the sink statistics (RTT, packet loss, send rate,
retransmit rate, send-buffer occupancy, ACK/packet counters), the encoder
state (target/min/max bitrate, measured encode rate, rolling RTT min/max),
the SRTLA aggregator state (uplink counts, connection/failover flags,
per-uplink RTT/in-flight/window/weight/NAK-burst/activity), and the puller
counters (frames, dropped frames, bytes sent, throughput, last-frame age)
in the **Prometheus text exposition format** (`# TYPE` + `name value`
lines, gauge/counter types) — scrapeable by Prometheus. The same sink
statistics are also logged as a structured `sink stats` line every 5 s.

```sh
curl -s 127.0.0.1:8899/metrics
curl -s -X POST 127.0.0.1:8899/rpc \
     -H 'Content-Type: application/json' \
     -d '{"jsonrpc":"2.0","id":1,"method":"system.status"}'
```

## Configuration

TOML with a top-level `sink` selector plus these tables (see `config.toml`):

- `sink` — output path: `"srt"` (direct SRT socket to `[srt].uri`, default) or
  `"srtla"` (SRT Link Aggregation over the `[srtla]` uplinks to a `srtla_rec` receiver).
- `[general]` — `min_bitrate`, `max_bitrate`, `balancer` (`adaptive`|`fixed`|`aimd`)
- `[srt]` — `latency` (ms), optional `uri`/`streamid`/`passphrase`
- `[srtla]` — SRT link aggregation (bond several uplinks / 4G-5G modems into one SRT stream);
  `receiver_host`, `receiver_port`, `uplink_ips` (one local source IP per bonded uplink),
  `mode` (`enhanced`|`classic`), `conn_timeout_ms`. Required when `sink = "srtla"`.
  The SRT client connects through an ephemeral loopback relay; its datagrams are fanned out
  over the uplinks. `[srt]` latency, stream ID, passphrase, and bandwidth cap still apply;
  `[srt].uri` is unused.
- `[adaptive]` / `[aimd]` — per-balancer step sizes and intervals
- `[overlay]` — `enabled`, `interval_ms`, optional format string
- `[pipeline]` — `template` (path to the pipeline file)
- `[scene]` — `config_path` (path to the scene JSON, optional)

### SRT link aggregation (`sink = "srtla"`)

Bonds several uplinks (e.g. multiple 4G/5G modems) into one SRT stream for higher, more
resilient throughput. Requires a running **`srtla_rec`** or compatible SRTLA receiver at
`[srtla].receiver_host:receiver_port`. The sender opens one UDP socket per `uplink_ip` and
completes the SRTLA REG1→REG2→REG3 handshake on each. An SRT client negotiates the real
end-to-end SRT session through a private, ephemeral loopback UDP relay; no relay port needs
configuration. SRT datagrams go unchanged over the best registered uplink, selected by a
congestion window (classic) or a quality-aware score (enhanced, default). Receiver feedback
returns to the SRT client, which handles loss recovery and retransmission. An SRTLA
registration alone does not establish that an SRT session or playback works.

If all uplinks are unresponsive for 10 seconds, the sender tries the next resolved receiver
address. Change `[srtla]` and send **SIGHUP** to reload the receiver and uplinks without
restarting the pipeline; invalid reloads leave the existing uplinks running.

To exercise two browser layers against an OBS SRTLA input listening on UDP port 5000:

```sh
bun scripts/test-obs-interactive-browser.ts
```

Set `PORT`, `SRTLA_HOST`, and `SRTLA_UPLINK_IPS` (comma-separated local IPv4 addresses) to
override the defaults. Switch red/blue video and audio with space (the hidden layer's audio
is muted), adjust the visible layer's volume with `a`/`v`, and verify both videos and tones
in OBS. Press `c` to confirm playback, then `q` to stop. The test also checks SRTLA
registration, the SRT session, and packet counters.

## Project layout

```
cmd/ctrlirl-coder/        # CLI entry point (flags, signals, wiring)
internal/config/          # TOML schema (pure Go)
internal/balancer/        # adaptive / fixed / aimd balancers (pure Go)
internal/scene/           # scene parse / diff / apply (pure Go)
internal/controller/      # control loop: balancer tick + overlay + SIGHUP (pure Go)
internal/sinks/           # base contracts (base.go): Stats, Sink, StatsSource, Applier, mock (cgo-free)
internal/sinks/srt/       # srtgo send socket (cgo) implementing Sink
internal/sinks/srtla/     # SRT Link Aggregation datagram relay (pure Go)
internal/gstpull/         # terminal appsink pull loop (cgo, gstreamer tag)
internal/gstutil/         # go-gst helpers (gstreamer tag)
internal/layer/           # base layer framework: Layer + BaseLayer + Chain (gstreamer tag)
internal/layer/{video,image,text,browser}/  # concrete layer types, each embedding BaseLayer
internal/compositor/      # command-goroutine pipeline (gstreamer tag)
pipeline/                 # example pipeline templates
```

## License

See the original [CtrlIRLCoder](https://github.com/CERALIVE/ctrlirl_coder) for the
upstream license; this is a clean-room Go rewrite.
