# Handoff Document: CtrlIRLCoder-Go — GStreamer Multi-Layer Streaming Tool

## 1. Project Overview

**Goal:** Rewrite [CtrlIRLCoder](https://github.com/CERALIVE/ctrlirl_coder) (originally in C) as a Go application that:

1. Preserves **100% of the original CtrlIRLCoder pipeline contract** (mandatory and optional named GStreamer elements).
2. Adds **OBS-like multi-layer scene composition** using the GStreamer `compositor` element.
3. Keeps the adaptive bitrate control loop (SRT-statistics-driven, `adaptive` / `fixed` / `aimd` balancers).
4. Keeps SRT output as the primary transport.
5. Keeps TOML-based configuration with `SIGHUP`-triggered live reload.

**Non-goals (initial scope):**

- Cross-platform Windows support (target Linux + macOS first).
- GUI — CLI only, but with live scene switching via config reload and/or the HTTPS control API.
- Replacing GStreamer with ffmpeg or hand-rolled encoders.

**Reference repos:**

- Original C implementation: `https://github.com/CERALIVE/ctrlirl_coder`
- Go GStreamer bindings: `https://github.com/go-gst/go-gst` (**pinned to v1.4.1**; import `pkg/gst`, `pkg/gstapp`)
- GStreamer: `https://github.com/gstreamer/gstreamer` (core + `plugins-{base,good,ugly}`); SRT is sent by the Go process, not a GStreamer sink.
- SRT transport: `https://github.com/haivision/srtgo` (a **cgo** wrapper) linked against the **CERALIVE SRT fork** (`https://github.com/CERALIVE/srt`) native `libsrt.so.1.5`. See the recorded revision below.

> **Recorded revision (matches CtrlIRLCoder/belacoder, re-confirmed by the operator; supersedes the interim in-pipeline `srtsink` pivot AND the earlier `identity`+`handoff` byte-tap stand-in):** SRT output is transmitted by a **Go-owned SRT socket** (`haivision/srtgo`), fed by a terminal **`appsink name=appsink`** drained with `gst_app_sink_pull_sample` — the exact original design. This is _not_ GStreamer's `srtsink`. Consequences: (1) the mandatory SRT terminal is the **`appsink`** element named `appsink`; (2) the Go **data path exists** — `internal/gstpull` pulls samples off the appsink on a dedicated goroutine (`gstpull` binds `libgstreamer-app` via cgo), `internal/srt Client.Write` sends each frame to the socket; (3) `haivision/srtgo` **is** a dependency (pure-Go, replaced an earlier cgo `libsrt` plan); (4) **the original `ctrlirl_coder`/`belacoder` appsink data path is used verbatim** — `libgstreamer-app` (dev + runtime) is now a build/runtime prerequisite, matching the original tool. The appsink reports **stats** frames Go reads for the overlay: frames pulled/sent/dropped, bytes, throughput (kbit/s from the pull loop), negotiated framerate (read once from the appsink's sink-pad caps), last-PTS, and a frame-production **stall detector** that the adaptive controller uses to drop the encoder to its floor (see §11.5). Reason for the owned socket: the 20 ms adaptive controller needs low-level SRT stats (`srt_bstats` RTT, `SRTO_SNDDATA` send buffer, packet loss, throughput) that an in-pipeline `srtsink` cannot be polled for, and the owned socket is what `srtla` bonding needs. **The appsink is set `drop=true`** in `internal/compositor/contract.go` so a stalled SRT sender back-pressures by dropping, never wedging the pipeline.

> **Recorded revision (CERALIVE SRT fork, 2026-11): supersedes the "pure-Go `srtgo`, no cgo / no libsrt" wording above.** `haivision/srtgo` is a **cgo** wrapper that links a native `libsrt.so.1.5`; the native library is now the **CERALIVE fork** (SRT 1.5.7, GnuTLS), built from a pinned source commit by `scripts/build-ceralive-srt.sh` into a private bundle. Consequences: (1) `internal/sinks/srt` is **cgo** and its build/test needs the CERALIVE bundle; (2) a bare `go build` no longer yields a working SRT sender — the binary must embed the bundle's library **SHA-256** (ldflags) and `srt.EnsureSRTBackend()` re-verifies the mapped library at runtime, refusing to stream on mismatch; (3) the fork's `libsrt.so.1.5` ships next to the binary (loaded via `$ORIGIN` rpath); the stock `libsrt` apt package is deliberately not installed (shared SONAME risk); (4) the fork's receiver-side `SRTO_*` options are left default-off, so **sender behaviour is unchanged**. Build/test entry points: `make dist-amd64`, `make test`, the Dockerfile (in-band build), and the CI gstreamer/cross-arm64 tiers.
>
> **Recorded revision (user-directed, supersedes the INI config format):** the configuration file is now **TOML** (`config.toml`, parsed with `github.com/BurntSushi/toml`) instead of INI (`ctrlirl_coder.conf` / `gopkg.in/ini.v1`). Consequences: (1) table names and key names are **unchanged** — the original `ctrlirl_coder.conf` schema carries over verbatim, only the syntax changes (`section` → `[table]`, strings quoted); (2) `SIGHUP` live reload is unchanged; (3) the example file is `config.toml`; (4) the TypeScript config serializer (§13) must emit TOML, not INI.

---

## 2. Hard Constraints — The Pipeline Contract (READ FIRST)

The original CtrlIRLCoder inspects the _running_ pipeline at startup by **element name**. If these elements are missing or misnamed, the application silently degrades or fails. **Every implementation must honor the following table exactly.**

| Element name                  | Required                           | Used by                       | Purpose                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| :---------------------------- | :--------------------------------- | :---------------------------- | :----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `appsink`                     | **MANDATORY** for SRT output       | byte-tap (`internal/gstpull`) | Terminal element the pipeline ends in: `… ! appsink name=appsink`. Go drains it with `gst_app_sink_pull_sample` (cgo, via `internal/gstpull`) and sends the bytes over the **Go-owned SRT socket** (`internal/srt`; no in-pipeline `srtsink`). `contract.go` verifies the element type is `appsink` and forces `drop=true` (back-pressure drops frames; the pull goroutine never wedges). Same design as the original `ctrlirl_coder`/`belacoder`. See §5.6. |
| (no `srtsink`)                | n/a                                | —                             | GStreamer `srtsink` is **not** in the pipeline. The original C tool drained `appsink` → `srt_send`; this Go port does exactly that — `appsink` → `gstpull` pull loop → `internal/srt Client.Write`. SRT is sent by the Go process, not an element.                                                                                                                                                                                                           |
| `venc_bps` **or** `venc_kbps` | **MANDATORY** for adaptive bitrate | `encoder_control`             | Video encoder whose `bitrate` property is mutated at runtime. **Exactly one** must be present. The suffix indicates units: `_bps` = bits per second, `_kbps` = kilobits per second. `x264enc`'s `bitrate` is kbit/s (a `guint`), so an x264enc pipeline **must** name it `venc_kbps`.                                                                                                                                                                        |
| `overlay`                     | Optional                           | `overlay_ui`                  | Text overlay element (typically `textoverlay`) updated with live stats (bitrate, RTT, loss).                                                                                                                                                                                                                                                                                                                                                                 |
| `v_delay`                     | Optional                           | A/V sync                      | `identity` element on the video branch, `signal-handoffs=TRUE`, used for fine-grained delay adjustment.                                                                                                                                                                                                                                                                                                                                                      |
| `a_delay`                     | Optional                           | A/V sync                      | Same as above but on the audio branch.                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `ptsfixup`                    | Optional                           | PTS smoothing                 | `identity` element (or dedicated fixup element) that smooths PTS jitter, mainly for OBS compatibility.                                                                                                                                                                                                                                                                                                                                                       |

### 2.1 Behavioral rules derived from the contract

- **Startup validation:** After building the pipeline, the app must (a) look up the SRT terminal `GetElementByName("appsink")` and (b) look up the encoder `GetElementByName("venc_bps")` (falling back to `venc_kbps`). If the encoder is missing, log a clear error and disable adaptive bitrate; if the `appsink` is missing from a pipeline that is expected to stream, log a clear error (it cannot stream over SRT without it). When the `appsink` is found, the app **must verify it is an `appsink` element and set `drop=true`** (done in `contract.go`; without `drop=true` a stalled sender wedges the pipeline). **Do not** invent new encoder names — `venc_bps`/`venc_kbps` are the contract.
- **Bitrate units:** When mutating the encoder's `bitrate` property, multiply/divide by 1000 according to whether the found name ends in `_bps` or `_kbps`. **Never** assume a unit.
- **Overlay updates:** If `overlay` exists and is a `textoverlay`, update its `text` property from the same goroutine that owns the pipeline (see §7.2). Frequency must be configurable (default ~1 Hz).
- **Delay elements:** If `v_delay`/`a_delay` exist, they must have `signal-handoffs=TRUE` when constructed, so runtime delay adjustments via `pad-offset` work.
- **PTS fixup:** If `ptsfixup` exists and is an `identity`, no action is needed from the app — its mere presence is part of the contract for OBS-compat. Do **not** remove it if the user put it in the pipeline template.
- **SRT framing (template requirement):** The template **must** normalize H.264 to Annex-B start-code framing before the appsink. Without it, SRT receivers (OBS `srt://` inputs) receive bytes but cannot decode (x264enc emits AVCC length-prefixed NALs, and the appsink pull passes those raw bytes straight to the SRT socket). Required chain: `… ! h264parse config-interval=1 ! video/x-h264,stream-format=byte-stream ! appsink name=appsink`. **Gotcha:** on GStreamer ≥1.24 the caps field is `stream-format=` — the legacy `format=byte-stream` capsfilter is silently ignored and the stream stays AVCC. `config-interval=1` re-emits SPS/PPS so players connecting mid-stream can decode.

> **Acceptance test:** A pipeline built from a template that contains all optional elements must start, run for ≥30 s, adjust bitrate at least once, update the overlay at least once, and terminate cleanly.

---

## 3. Architecture Decisions (already validated with product owner)

| Decision                | Choice                                                                                                                                      | Rationale                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| :---------------------- | :------------------------------------------------------------------------------------------------------------------------------------------ | :------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Multimedia framework    | **GStreamer via `go-gst`** (cgo bindings)                                                                                                   | Only way to satisfy the pipeline contract and support `compositor`. Pinned to **go-gst v1.4.1** (MIT); the classic-path v1.0–v1.3 releases are retracted by the author and import under `pkg/gst` / `pkg/gstapp` in v1.4.1.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| Multi-layer composition | **Single `compositor` element** with one sink pad per layer                                                                                 | Native GStreamer, matches OBS z-order semantics.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| Layer abstraction       | **`Layer` type** producing a **pre-linked element chain** (`BuildChain() ([]gst.Element, error)`)                                           | Keeps pipeline construction per source type isolated. **Revision:** the original design returned a `*gst.Bin`; go-gst v1.4.1 does not expose `gst_bin_add_ghost_pad`, so a bin's source pad cannot be reached from Go. Layers are built as flat element chains and the compositor links each chain's `src` pad to a `RequestPadSimple("sink_%u")` pad.                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| SRT transport           | **Go-owned SRT socket** fed by a terminal **`appsink name=appsink`** (`gst_app_sink_pull_sample` drained in Go; `… ! appsink name=appsink`) | **Revision (matches CtrlIRLCoder/belacoder, re-confirmed by the operator):** Go owns the `SRTSOCKET` (`haivision/srtgo`) and sends the pulled bytes itself — no in-pipeline `srtsink`. **The original `ctrlirl_coder`/`belacoder` appsink data path is used verbatim** — `internal/gstpull` binds `libgstreamer-app` via cgo and pulls samples on a dedicated goroutine. Reason for the owned socket: the 20 ms adaptive controller needs low-level SRT feedback (`srt_bstats` RTT, `SRTO_SNDDATA` send-buffer, packet loss, throughput) that an in-pipeline sink cannot be polled for, and the owned socket is what `srtla` bonding needs. The appsink also reports pull-loop **stats** (frames, bytes, throughput, framerate, stall) that the overlay and the adaptive controller consume. |
| SRT statistics          | **Read from the live Go socket** (`srtgo` `SrtStats`)                                                                                       | `Stats()` maps `srtgo` fields (`MsRTT`, `ByteSndBuf`≙`SRTO_SNDDATA`, cumulative loss/retrans, …); `SendBufferBytes` is live. **Throughput is measured on the write path** (`sendRateTracker`) and overrides `MbpsSendRate`, which reads ~0 on low-RTT/loopback links and would zero the balancer's `RTT_TO_BS` buffer threshold. See §5.6.                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| Concurrency model       | **Single pipeline-owning goroutine + command channel**                                                                                      | GStreamer is not thread-safe; serializing all pipeline operations is the only safe path.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| Configuration           | TOML file (table/key names identical to the original `ctrlirl_coder.conf` schema), reloaded on `SIGHUP`                                     | Schema values carry over verbatim; only the syntax changes.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| Build system            | `go build` + `Makefile` wrapper, Dockerfile for CI                                                                                          | Standard Go toolchain.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |

---

## 4. Repository Layout

```
ctrlirl_coder-go/
├── cmd/
│   └── ctrlirl-coder/
│       ├── main.go                    # Entry point, CLI flags, signal loop
│       └── run_gst.go                 # Wires compositor + gstpull + srt Client + controller
├── internal/
│   ├── config/
│   │   ├── config.go                  # TOML struct, Load(), Reload()
│   │   ├── balancer.go                # Balancer config → balancer.New
│   │   └── config_test.go
│   ├── gstutil/
│   │   └── gstutil.go                 # Helpers: Parse, GetByName, state transitions
│   ├── compositor/
│   │   ├── compositor.go              # CompositorPipeline: owns the whole GstPipeline
│   │   ├── contract.go                # Contract check: verifies the `appsink` terminal + forces `drop=true`
│   │   └── layers.go                  # Layer pad/element management
│   ├── layer/
│   │   ├── layer.go                   # Layer interface
│   │   ├── video_source.go            # v4l2src / rtspsrc / filesrc(video)
│   │   ├── image_overlay.go           # filesrc ! pngdec ! imagefreeze
│   │   └── text_overlay.go            # textoverlay-based static text layer
│   ├── scene/
│   │   ├── scene.go                   # Scene definition, Apply(), Diff()
│   │   └── scene_test.go
│   ├── gstpull/
│   │   ├── pull.go                    # Appsink pull loop: `gst_app_sink_pull_sample` → send callback; stats + stall detector
│   │   ├── pull_cgo.go                # cgo bindings to `libgstreamer-app` (`gstreamer-app-1.0`)
│   │   └── pull_test.go               # E2E videotestsrc→appsink test incl. stall detection
│   ├── srt/
│   │   ├── stats.go                   # Stats struct (balancer input) + IsValid()
│   │   ├── source.go                  # StatsSource interface (pure Go)
│   │   ├── applier.go                 # Applier interface + EncoderUnits/BitratePropertyValue
│   │   ├── client.go                  # Go SRT send client (srtgo) + sendRateTracker
│   │   ├── client_stats.go            # mapStats: srtgo.SrtStats → Stats
│   │   └── mock.go                    # MockStatsSource: pure-Go profile-driven source
│   ├── balancer/
│   │   ├── balancer.go                # Balancer interface
│   │   ├── adaptive.go                # statistics-driven
│   │   ├── fixed.go                   # no-op
│   │   ├── aimd.go                    # additive-increase multiplicative-decrease
│   │   └── *_test.go
│   └── controller/
│       ├── controller.go              # Control loop: StatsSource + Applier + balancer
│       └── commands.go                # Command types for the pipeline-owning goroutine
├── pipeline/                          # GStreamer pipeline templates (unchanged format)
│   ├── example-full.pipeline
│   └── example-layered.pipeline
├── config.toml
├── go.mod
├── go.sum
├── Makefile
├── Dockerfile
└── README.md                          # MUST mirror the original contract table verbatim
```

---

## 5. Module Specifications

### 5.1 `internal/config`

**Struct shape** (mirror the original `ctrlirl_coder.conf` schema, TOML syntax):

```go
type Config struct {
    SRT       SRTConfig       `toml:"srt"`
    Encoding  EncodingConfig  `toml:"encoding"`
    Balancer  BalancerConfig  `toml:"balancer"`
    Overlay   OverlayConfig   `toml:"overlay"`
    Pipeline  PipelineConfig  `toml:"pipeline"`
    Scene     SceneConfig     `toml:"scene"`    // NEW
}

type SRTConfig struct {
    URI        string `toml:"uri"`         // srt://host:port
    Latency    int    `toml:"latency"`      // ms (separate field; not a URI query)
    Passphrase string `toml:"passphrase"`
    StreamID   string `toml:"streamid"`
}

type EncodingConfig struct {
    MinBitrate   int `toml:"min_bitrate"`   // kbps
    MaxBitrate   int `toml:"max_bitrate"`   // kbps
    StartBitrate int `toml:"start_bitrate"` // kbps
}

type BalancerConfig struct {
    Algorithm string `toml:"algorithm"`     // adaptive | fixed | aimd
    Interval  int    `toml:"interval_ms"`   // default 20
    Aimd *AimdParams `toml:"aimd"`
}

type OverlayConfig struct {
    Enabled   bool   `toml:"enabled"`
    Format    string `toml:"format"`        // e.g. "Bitrate: %d kbps\nRTT: %.1f ms"
    Interval  int    `toml:"interval_ms"`   // default 1000
}

type PipelineConfig struct {
    Template  string `toml:"template"`      // path to .pipeline file (single-layer fallback)
}

type SceneConfig struct {
    ConfigPath string `toml:"config_path"`  // path to scene.json (multi-layer)
}
```

**Behavior:**

- `Load(path)` parses with `github.com/BurntSushi/toml` (TOML; table/key names mirror the original INI schema).
- `Reload()` is called on `SIGHUP`; returns diff-friendly snapshot (old, new).
- Invalid values → fail fast with a descriptive error.

### 5.2 `internal/gstutil`

Thin helpers over `go-gst` (import `github.com/go-gst/go-gst/pkg/gst`):

- `EnsureInit()` — process-wide GStreamer init (idempotent).
- `Pipeline` — a wrapper struct holding a `gst.Pipeline`.
- `FromLaunch(desc string) (*Pipeline, error)` — builds from a `gst-launch`-style string via `gst.ParseLaunch`.
- `(p *Pipeline) As() gst.Pipeline` — the underlying pipeline.
- `(p *Pipeline) SetState(s gst.State) error` — `SetState` + `GetState` check for `StateChangeFailure`.
- `SetStateSafe(el gst.Element, s gst.State) (gst.StateChangeReturn, error)` — package func; state set with failure check.
- `SetNumericProperty(el gst.Element, name string, v int64)` — cgo `g_object_setv` path for integer/enum properties (the varargs `g_object_set` used by the go-gst binding silently drops int GValues in this build).
- `IsFactoryType(kind string) bool` — factory availability probe.

> All of these operate on the classic (interface) GStreamer API exposed by go-gst v1.4.1; element properties use `SetObjectProperty`/`GetObjectProperty` (inherited from go-glib v2), not the older `SetProperty`/`GetProperty`.

### 5.3 `internal/compositor`

**Owns the entire `*gst.Pipeline`** (main pipeline), not just the compositor element. It is the _only_ struct allowed to touch GStreamer state from outside its owning goroutine.

```go
type CompositorPipeline struct {
    pipeline   *gst.Pipeline
    compositor *gst.Element
    layers     map[string]*layerHandle  // key = Layer.ID()
    cmdCh      chan Command             // serialized command channel
    doneCh     chan struct{}
}

type layerHandle struct {
    id         string
    pad        gst.Pad                // the compositor sink_%u pad
    chain      []gst.Element          // layer's element chain (no bin)
    outputPad  gst.Pad                // the chain's terminal src pad
    props      map[string]float64     // last-known compositor-pad props
    audioChain []gst.Element          // nil if the layer carries no audio
    audioPad   gst.Pad                // mux sink_%d pad for the audio chain
    layer      layer.Layer            // the scene layer (Close on teardown)
}
```

> **Revision:** `layerHandle.bin` became `layerHandle.chain []gst.Element` because layers are element chains, not bins (see §5.4).

**Construction sequence (in `New`):**

1. Build the pipeline string. The terminal is **`appsink name=appsink`** — the Go-owned SRT socket (not an in-pipeline `srtsink`) pulls the muxed bytes off the appsink with `gst_app_sink_pull_sample`:
    ```
    compositor name=comp background=black
      ! videoconvert
      ! {optional: identity name=v_delay signal-handoffs=TRUE}
      ! {optional: identity name=ptsfixup}
      ! {optional: textoverlay name=overlay ...}
      ! {encoder} name=venc_bps OR venc_kbps bitrate=<start-kbps>
      ! h264parse config-interval=1    # per template
      ! mpegtsmux name=mux             # muxes video+audio
      ! appsink name=appsink           # terminal; Go drains it (drop=true)
    ```
    The encoder segment uses the **`bitrate`** property (kbit/s — the real `x264enc` property; see §7.4). The SRT send is **not** an element: `internal/srt.Client` transmits the pulled bytes over a Go-owned `srtgo` socket to the configured `srt://HOST:PORT`. The terminal **`appsink` is mandatory** — it is the byte-tap the original tool drained and the one `internal/gstpull` pulls from.
2. Build the audio branch in parallel if the template requests it; audio is muxed into the same `mpegtsmux` (matching the original).
3. Parse with `FromLaunch`.
4. **Contract check** (`contract.go`): try `venc_bps`, fall back to `venc_kbps` — if neither, disable adaptive bitrate. Look up the `appsink` terminal; **verify it is an `appsink` element (GType) and force `drop=true`** (a missing/mis-typed terminal cannot fill the socket — the "no video" bug). Cache the found encoder name + units and the appsink element (for `gstpull.Attach`).
5. Start pipeline in `PAUSED`, then `PLAYING` (after first layer is attached).

**Public API:**

```go
func New(launch string) (*CompositorPipeline, error)   // parses launch, runs checkContract

func (cp *CompositorPipeline) Start() error
func (cp *CompositorPipeline) Stop() error

// Layer operations - all go through a serial cmdCh (the pipeline goroutine)
func (cp *CompositorPipeline) AddLayer(ctx context.Context, sl scene.Layer) error
func (cp *CompositorPipeline) RemoveLayer(ctx context.Context, id string) error
func (cp *CompositorPipeline) UpdateLayerProps(ctx context.Context, id string, props map[string]float64) error

// Contract discovery (set by checkContract at New time)
func (cp *CompositorPipeline) HasCompositor() bool
func (cp *CompositorPipeline) HasEncoder() bool
func (cp *CompositorPipeline) HasTap() bool          // appsink terminal present
func (cp *CompositorPipeline) TapElement() gst.Element // the terminal appsink (for gstpull.Attach)
func (cp *CompositorPipeline) EncoderName() string

// Encoder / overlay — the controller drives these via the srt.Applier interface
func (cp *CompositorPipeline) SetBitrate(bps int) error
func (cp *CompositorPipeline) SetOverlayText(text string)

// The SRT stats source is NOT on the compositor: the Go-owned srt.Client
// implements srt.StatsSource itself (it reads the socket), and the controller
// is handed that client directly (see cmd/ctrlirl-coder/run_gst.go).
```

**Internal command goroutine** (`run()`):

- Loop on `cmdCh`. Handles one command at a time, blocking.
- All GStreamer calls (`RequestPadSimple`, `ReleaseRequestPad`, `SetObjectProperty`, `SetState`) happen **only here**.
- Uses `sync.WaitGroup` or per-command `chan error` for synchronization with callers.

**AddLayer pseudo-code:**

```go
chain, err := l.BuildChain()                       // ordered element chain
pipeline.AddMany(chain...)                         // add all to the main pipeline
for i := range chain - 1 { chain[i].Link(chain[i+1]) }  // link the chain
pad := cp.compositor.RequestPadSimple("sink_%u")   // NOT sink_%d, keep %u
chain[last].GetStaticPad("src").Link(pad)          // chain → compositor
for prop, val := range l.DefaultProperties() { pad.SetObjectProperty(prop, val) }  // zorder, xpos, ypos, alpha
```

**RemoveLayer pseudo-code:**

```go
// 1. for each el in chain (sink→src): pipeline.Remove(el); el.SetState(gst.StateNull)
// 2. chain[last].Unlink()  (break chain → compositor link)
// 3. cp.compositor.ReleaseRequestPad(pad)
```

````

### 5.4 `internal/layer`

The base package holds the `Layer` interface, the `BaseLayer` base class (id/type accessors, default compositor-pad properties, no-op Close), the shared `Chain` type, and the chain-building helpers (`MakeElement`, `SurfaceChain`, `Tail`, `VideoFilters`, `AudioFilters`). Concrete layer types live in subpackages — `internal/layer/{video,image,text,browser}` — and embed `BaseLayer`; the compositor dispatches on the scene layer type (`compositor.buildLayer`).

```go
type Layer interface {
    ID() string
    Type() string
    // BuildChain returns the layer's elements in link order (source → … → sink),
    // not yet linked. The compositor links them into the pipeline.
    BuildChain() ([]gst.Element, error)
    DefaultProperties() map[string]interface{}  // keys: zorder, xpos, ypos, width, height, alpha
    Close() error
}
````

> **Revision:** the original design had `BuildBin() (*gst.Bin, error)`. go-gst v1.4.1 does not expose `gst_bin_add_ghost_pad`, so a bin's source pad cannot be reached from Go to link into the `compositor`. Layers are therefore built as flat, pre-ordered element **chains**; the compositor links `chain[i].src → chain[i+1].sink` and then `chain[last].src → compositor.RequestPadSimple("sink_%u")`. This matches the go-gst `requestpad` example pattern.

**Implementations:**

- **`VideoSourceLayer`**: `{v4l2src|rtspsrc|filesrc} ! decodebin ! [filters] ! videoconvert ! videoscale ! video/x-raw,format=BGRA ! queue` (`[filters]` = the scene layer's `filters` array, empty when undeclared). Default props `{zorder: 1, xpos: 0, ypos: 0, width: 1280, height: 720, alpha: 1.0}`.
- **`ImageOverlayLayer`**: `filesrc location=… ! decodebin ! imagefreeze ! videoconvert ! videoscale ! queue`. Default `{zorder: 10, alpha: 0.9, width: <img>, height: <img>}`.
- **`TextOverlayLayer`**: a pango `textoverlay` fed by a fully transparent BGRA surface (an `appsrc`, `stream-type=stream`, pushed from a Go goroutine at 30 fps, one whole frame per buffer) — `appsrc is-live=true format=time caps=video/x-raw,width=W,height=H,format=BGRA ! textoverlay text="…" … ! queue`. Text is styled via the scene's `text-options` (element properties: `font-desc`, `color`, `shaded-background`, `halignment`, `valignment`, …). Useful for lower-thirds.

All implementations must ensure output caps are **BGRA** or **AYUV** (compositor-friendly formats) to avoid renegotiation stalls.

### 5.5 `internal/scene`

JSON scene descriptor loaded at startup and on `SIGHUP`:

```json
{
    "name": "main",
    "canvas": { "width": 1280, "height": 720 },
    "layers": [
        {
            "id": "camera",
            "type": "video",
            "source": "v4l2src device=/dev/video0",
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
            "source": "file:///opt/logo.png",
            "properties": {
                "zorder": 10,
                "xpos": 1080,
                "ypos": 0,
                "width": 200,
                "height": 100,
                "alpha": 0.85
            }
        }
    ]
}
```

**`filters` (optional, per layer):** array of `{ "kind": …, "params": { … } }`
media filters (see `FILTERS.md`) spliced into the layer's chain. Video
filters — `videoscale`, `videocrop`, `videobalance`, `videobox`, `videoflip`,
`videorotate`, `effectv` — land between the source and the `videoconvert`
tail, in scene order; audio filters — `volume`, `equalizer`, `audiodynamic`,
`pitch` — land in the browser layer's audio chain between the converter and
the AAC encoder (browser is the only layer type that carries audio). Kinds
and params are validated at scene load (closed registry in
`internal/scene/filters.go`). A filter change on reload forces a chain
rebuild: `Diff` emits `RemoveLayer` + `AddLayer` for that layer.

**`Scene.Apply(cp)`** — first-time apply: adds all layers in ascending `zorder`.
**`Scene.Diff(old, new)`** — returns a list of `AddLayer` / `RemoveLayer` / `UpdateLayerProps` operations.
**`Scene.ApplyDiff(cp, ops)`** — executes ops through `cp` in the correct order (remove before add when z-order collisions occur).

### 5.6 `internal/srt`

**Revision (matches CtrlIRLCoder/belacoder, re-confirmed by the operator):** SRT is transmitted by a
**Go-owned SRT socket** (`haivision/srtgo`, cgo — links the CERALIVE fork `libsrt`). This package is a real **send client**
again: it dials the socket, sends the bytes the `appsink` puller hands it, reconnects
with backoff, and reads live stats from the socket for the balancer. The balancer/controller depend
on the `StatsSource` interface, never on the concrete client:

```go
// source.go (pure Go)
type StatsSource interface {
    Stats() (Stats, error)
    Close() error
}

// Stats is the balancer's input. Do not rename fields without
// updating balancer/adaptive.go.
type Stats struct {
    RTT                float64 // ms
    PacketLossRate     float64 // %
    SendRateMbps       float64
    BandwidthEstimate  float64 // Mbps
    RetransmitRate     float64 // packets/s
    PktSndLossTotal    int64
    PktRetransTotal    int64
    SendBufferBytes    int
    ACKTotal           int64
    PacketsSentTotal   int64
    PacketsReceivedTotal int64
    TimestampMs        uint64
}
```

The client is also the `Applier` the controller uses to act on the pipeline (`applier.go`):

```go
type Applier interface {
    SetBitrate(bps int)      error
    SetOverlayText(text string)
}
```

**`Client`** (`client.go`, cgo via `srtgo` → CERALIVE fork):

- `NewClient(cfg ClientConfig, log)` parses `URI` (`srt://host:port`; `LatencyMs`, `StreamID`,
  `Passphrase`, optional `MaxBandwidthKbps` → `SRTO_MAXBW`). Non-blocking.
- `Start(ctx)` dials with exponential backoff and runs housekeeping (reconnect on link-down).
- `Write(b []byte)` sends one pulled buffer; on failure marks the link down (**dropping** the buffer
  rather than blocking the pipeline) and records the bytes sent for the throughput tracker.
- `Stats()` reads the live socket via `client_stats.go mapStats`, then **overrides `SendRateMbps`
  with the write-path `sendRateTracker`** (see below).
- `Connected()`, `LatencyMs()`, `Close()`.

**`MockStatsSource`** (pure Go, `mock.go`) — profile-driven `StatsSource` for unit tests and
`--dry-run`; monotonically increasing cumulative loss/retrans counters.

**Field mapping — from `srtgo` `SrtStats`** (`client_stats.go`, the Go equivalent of the original's
`srt_bstats` + `SRTO_SNDDATA`):

| `Stats` field          | `srtgo` field                                        | notes                            |
| :--------------------- | :--------------------------------------------------- | :------------------------------- |
| `RTT`                  | `MsRTT`                                              | ms, instantaneous                |
| `SendRateMbps`         | `MbpsSendRate` → **overridden by `sendRateTracker`** | see below                        |
| `BandwidthEstimate`    | `MbpsBandwidth`                                      | Mbps                             |
| `PktSndLossTotal`      | `PktSndLossTotal`                                    | cumulative                       |
| `PktRetransTotal`      | `PktRetransTotal`                                    | cumulative                       |
| `ACKTotal`             | `PktRecvACKTotal`                                    | cumulative                       |
| `PacketsSentTotal`     | `PktSentTotal`                                       | cumulative                       |
| `PacketsReceivedTotal` | `PktRecvTotal`                                       | cumulative                       |
| `SendBufferBytes`      | `ByteSndBuf` (≙ `SRTO_SNDDATA`)                      | **live** (was 0 under `srtsink`) |
| `PacketLossRate`       | derived: `PktSndLossTotal/PktSentTotal·100`          | % of sent                        |
| `TimestampMs`          | `MsTimeStamp`                                        | sampled                          |

The cumulative counters drive the balancer's delta logic unchanged, and `SendBufferBytes` is now
real, so the adaptive balancer's buffer-pressure term is active.

**Throughput measured on the write path (`sendRateTracker`, `client.go`).** `srtgo`'s
`MbpsSendRate` reads ~0 on low-RTT / loopback links (its packet-rate accounting doesn't tick
there). Because the adaptive balancer's send-buffer threshold `RTT_TO_BS = throughput·rtt/pkt_size`
**collapses to 0 when throughput is 0**, a zero read pins the branch to "heavy" and the bitrate to
the floor on a healthy link. `Client` therefore records the bytes actually sent in `Write`
(`sendRateTracker.record`) and, in `Stats()`, overrides `SendRateMbps` with the measured rate
(when > 0). The tracker is smoothed (0.7/0.3) with a 50 ms minimum window; **its first call must
prime the `lastT`/`lastB` baseline** — the original code returned early without priming, so it
reported 0 forever (a "no video" class bug, fixed; see IMPLEMENTATION.md change log).

### 5.7 `internal/balancer`

```go
type Balancer interface {
    Name() string
    Next(currentBPS int, s srt.Stats, p Params) int
}
// Params is a config-decoupled struct the controller builds from config.Config
// (balancer package does not import internal/config).
func New(name string, p Params) Balancer  // "adaptive" | "fixed" | "aimd"
```

- **`Fixed`**: returns `currentBPS` unchanged.
- **`Adaptive`**: mirrors original `balancer_adaptive.c`. Rules (approximate — reverse-engineer from original; required behavior is preserved):
    - If `loss > highThreshold` or `RTT > highRTT` → decrease by up to 10%.
    - If `loss < lowThreshold` and `RTT < lowRTT` → increase by up to 5%.
    - Clamp to `[min, max]`.
- **`AIMD`**: additive increase (+50 kbps / interval), multiplicative decrease (×0.7 on loss > threshold).

Unit tests are mandatory for each; they should be pure functions of `(currentBPS, stats, cfg)`.

### 5.8 `internal/controller`

**Revision:** the original C tool's `appsink` pull → `srt.Send` data-path goroutine is now a **separate Go package** (`internal/gstpull`), not part of the controller. `gstpull` pulls samples off the terminal `appsink` with `gst_app_sink_pull_sample` (cgo to `libgstreamer-app`) on a dedicated goroutine and hands each frame's bytes to a `send` callback; `cmd` wires that callback to `srt.Client.Write`. The pull loop also tracks **stats** (frames, bytes sent/pulled, throughput kbit/s, negotiated framerate, last-PTS) and a **frame-production stall detector** (no sample for ≥ `frame_stall_ms`) exposed via `Puller.Stalled()`. The **controller itself is a pure control loop** depending on a `srt.StatsSource` (stats in — implemented by the `srt.Client`), a `Liveness` probe (the appsink `Puller`, used to force the encoder to its floor on stall), and an `Applier` (bitrate/overlay out — implemented by the compositor). This keeps it unit-testable with a `MockStatsSource` + fake applier + fake liveness. See §11.5.

```go
// Applier is the interface the controller uses to act on the pipeline.
// The gstreamer compositor implements it; tests use a fake. Units are bps,
// matching balancer.Next.
type Applier interface {
    SetBitrate(bps int) error
    SetOverlayText(text string)
}

type Controller struct {
    src     srt.StatsSource
    applier Applier
    bal     balancer.Balancer
    cfg     config.Config
    stop    chan struct{}
}

const controlTick = 500 * time.Millisecond

func (c *Controller) Run(ctx context.Context) error
//   1. balancer loop on a fixed control tick (default 500 ms):
//        s, _ := c.src.Stats()
//        if s.IsValid() { c.cur = c.bal.Next(c.cur, s, p); c.applier.SetBitrate(c.cur) }
//   2. overlay loop on cfg.Overlay.IntervalMS:
//        s, _ := c.src.Stats()
//        c.applier.SetOverlayText(formatStats(s, c.cur))
//   3. block on ctx.Done() (signal.Notify SIGHUP/TERM/INT upstream)
```

The current bitrate is tracked in the controller; `Next` is called with it and the returned value is applied and stored. Increase/decrease pacing is **internal to each balancer** (driven by `Stats.TimestampMs` and the per-baler `IncrInterval`/`DecrInterval` in the config), so the controller only needs a fixed control tick. A `Stats` sample with `IsValid()==false` (RTT ≤ 0, i.e. before the SRT handshake completes) is skipped so the bitrate is not adjusted on a cold link.

### 5.9 `cmd/ctrlirl-coder/main.go`

- Parse flags: `--config path`, `--scene path`, `--pipeline path`, `--dry-run` (validate-only).
- `signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)`.
- On `SIGHUP`: reload config; compute `scene.Diff`; submit ops to `cp`.
- On `SIGINT`/`SIGTERM`: graceful shutdown — stop pipeline, close SRT, exit 0.

---

## 6. Implementation Phases (recommended order)

### Phase 0 — Scaffolding (0.5 day)

- Create repo, `go.mod` (pin `github.com/go-gst/go-gst v1.4.1`, `github.com/haivision/srtgo`, `github.com/BurntSushi/toml`), `Makefile`, `Dockerfile` (base: `ubuntu:22.04` + `libgstreamer1.0-dev` + `libgstreamer-plugins-base1.0-dev` (the `gstreamer-app-1.0` library `internal/gstpull` links) + `gstreamer1.0-plugins-{base,good,bad,ugly}` + `gstreamer1.0-tools` + `gstreamer1.0-libav`). No `libsrt`/`srtsink` dependency is required — SRT is sent by the **pure-Go `srtgo` socket**, not an in-pipeline element.
- Copy `config.toml` (TOML, schema mirrors the original `ctrlirl_coder.conf`) and `pipeline/example-full.pipeline` from the original repo and end the terminal at `… ! mpegtsmux ! appsink name=appsink` (SRT destination lives in `config.toml`, not the pipeline string).
- Write README with the contract table from §2 and the recorded revisions (this §-preface).

### Phase 1 — Pure-Go core, no GStreamer (2 days)

- Implement `config`, `balancer` (all three algorithms), `scene`, unit tests.
- Implement `srt.MockStatsSource` (pure Go) — a profile-driven `srt.StatsSource` that fakes stats with cumulative counters. This lets you test the balancer and controller loops in CI without GStreamer.

**Acceptance:** `go test ./internal/balancer/... ./internal/config/... ./internal/scene/...` passes in CI without GStreamer installed.

### Phase 2 — GStreamer pipeline smoke test (2 days)

- Implement `gstutil.FromLaunch`, `GetByName`/`GetByNameRecursive`, `SetStateSafe`, `Link`.
- Write an integration test that:
    1. Builds a pipeline from `pipeline/example-full.pipeline` (terminal: `… ! appsink name=appsink`).
    2. Asserts that the `appsink` terminal, `venc_bps` or `venc_kbps`, `overlay`, `v_delay`/`a_delay`, `ptsfixup` are all discoverable, and that `appsink` is an `appsink` element with `drop=true`.
    3. Starts it, runs 2 s, sets the encoder `bitrate` property, pulls one sample via `gstpull` (and reads one `client.Stats()` sample if a Go SRT peer is up), stops.

**Acceptance:** Test passes on a machine with GStreamer + plugins (`gstreamer1.0-plugins-{base,good,bad,ugly}`, `gstreamer1.0-libav`, `gstreamer1.0-tools`).

### Phase 3 — Compositor + layers (3 days)

- Implement `compositor.CompositorPipeline` command goroutine.
- Implement `layer.VideoSourceLayer` and `layer.ImageOverlayLayer` (element chains via `BuildChain`).
- Add `AddLayer` / `RemoveLayer` integration tests with two layers.
- Add `SetLayerProps` test that changes `xpos` and verifies via `pad.GetObjectProperty("xpos")`.

**Acceptance:** A two-layer scene runs for 5 s, changes logo `xpos`, reads back the property, stops cleanly.

### Phase 4 — Scene + config reload (2 days)

- Implement `scene` package + JSON loader.
- Wire `SIGHUP` → reload → diff → apply.
- Test: start with logo at (0,0); SIGHUP with logo at (100,100); assert property changed.

### Phase 5 — SRT + full loop (2 days)

- Wire the `srt.Client` (the Go `srtgo` send socket) as the `StatsSource`; wire the `gstpull.Puller` as the `Liveness` probe; run the balancer loop + overlay loop against them. The data path is `gstpull` pulling samples off the terminal `appsink` → `client.Write` (no in-pipeline `srtsink`).
- End-to-end test: local SRT pair (the Go client sender + a `srtsrc` listener receiver, or `srt-live-transmit` in receiver mode); assert the socket `Stats` are populated and one bitrate change is applied to the encoder.

**Acceptance:** 30 s run, at least one bitrate change observed in logs, at least one overlay update observed, no leaks (`gst-launch`-style debug or a valgrind pass if feasible).

### Phase 6 — Hardening (2 days)

- Log context (structured logging — `slog`).
- Graceful shutdown.
- Metrics endpoint (optional, `/metrics` on `localhost`).

---

## 7. Critical Pitfalls & Rules for the Implementing Agent

### 7.1 Thread safety

GStreamer is **not** thread-safe. Every call to `SetObjectProperty`, `RequestPadSimple`, `ReleaseRequestPad`, `SetState`, `Add`, `Remove`, `Link`, `Unlink` on the pipeline must go through the **single command goroutine** in `CompositorPipeline.run()`. The controller's balancer/overlay loops only _read_ the Go SRT socket's stats via `client.Stats()` (a read-only observation that never touches GStreamer) and submit all mutations (`SetBitrate`, `SetOverlayText`) as commands — never touching elements directly.

### 7.2 Overlay updates

`textoverlay.text` is a pipeline mutation. Route it through `cp.SetOverlayText()` → command goroutine. Do not call `overlay.SetObjectProperty("text", …)` from the overlay loop goroutine directly.

### 7.3 SRT socket (Go owns the send path)

SRT is sent by a **Go-owned `srtgo` socket**; the Go process **does** own the stream-data send path. The pipeline ends in `… ! mpegtsmux ! appsink name=appsink`; `internal/gstpull` pulls each encoded frame off the `appsink` with `gst_app_sink_pull_sample` (cgo) and hands the bytes to `client.Write`, which transmits them over the socket. Stats come from `client.Stats()` (the socket), not from any GStreamer element property. Do **not** put a `srtsink` in the pipeline and do **not** read an element `stats` structure — the socket is the source of truth. If the `appsink` terminal is missing (or is the wrong element type), no data reaches the socket (the "no video" bug); `contract.go` verifies the terminal is an `appsink` and forces `drop=true` so a stalled sender back-pressures by dropping instead of wedging the pipeline.

### 7.4 Bitrate units

If the encoder is `venc_kbps`, mutate `bitrate` in kbps (bps / 1000). If `venc_bps`, mutate in bps. Cache the mode at startup in a `bitrateUnits` field. Do not re-detect per call.

### 7.5 Layer removal

Always `SetState(gst.StateNull)` each element in the layer's chain **before** `pipeline.Remove`, and `Unlink` the chain's last element from the compositor pad before `ReleaseRequestPad`. Otherwise the streaming thread may still be holding buffers when an element is freed. (There is no bin to null out; a chain has no lifetime owner.)

### 7.6 Z-order changes

Changing `zorder` on a compositor pad may require the element to briefly renegotiate. Accept 1–2 dropped frames. If `zorder` collisions occur (two pads with same value), `compositor` picks arbitrarily — the `scene` package must ensure uniqueness before applying.

### 7.7 Build configuration (single cgo build)

There is no build-tag split: GStreamer-dependent packages (`internal/gstutil`, `internal/compositor`, `internal/layer`, `internal/gstpull`) are plain cgo packages, and `go build ./...` / `go test ./...` require the GStreamer and go-gst dev libraries. `internal/srt` is pure Go (it speaks `srtgo`, not GStreamer), as are config, balancer, `internal/scene`, and `internal/rpc` (`srt.StatsSource`/`MockStatsSource`/`Client`/`mock` keep the SRT side testable without GStreamer).

### 7.8 Signal handling

`SIGHUP` reloads config. Do **not** call `os.Exit` on `SIGHUP` — the original reloads in-place. Only `SIGINT`/`SIGTERM` exit.

### 7.9 Named element discovery

Use `pipeline.GetElementByName("appsink")` (the SRT terminal) and the encoder (`venc_bps`/`venc_kbps`) **after** the pipeline is in `PAUSED` state (element names are only resolvable once the pipeline has been parsed). Do not try before. `checkContract` does this discovery once, at `New` time.

### 7.10 Video caps at compositor inputs

Force `BGRA` or `AYUV` caps on each layer's output pad. If two layers negotiate different formats, `compositor` will insert a `videoconvert` internally — but this is slow and causes renegotiation glitches on layer add/remove.

---

## 8. Testing Strategy

| Test suite           | Framework                   | Environment         | Runs on      |
| :------------------- | :-------------------------- | :------------------ | :----------- |
| `balancer` unit      | `testing`                   | None                | Every commit |
| `config` unit        | `testing`                   | None                | Every commit |
| `scene` diff unit    | `testing`                   | None                | Every commit |
| `compositor`/`layer` | `testing` (cgo)             | GStreamer installed | Every commit |
| End-to-end SRT       | shell + `srt-live-transmit` | GStreamer + libsrt  | Weekly       |
| Race detector        | `go test -race`             | Any                 | Every commit |

---

## 9. Acceptance Criteria (the project is "done" when…)

1. **Contract preserved:** Running with the adapted `pipeline/example-full.pipeline` starts successfully, discovers the `identity` byte-tap (with `signal-handoffs=TRUE`) and `venc_bps` (or `venc_kbps`), discovers `overlay` / `v_delay` / `a_delay` / `ptsfixup` if present, and updates all of them at runtime.
2. **Multi-layer:** A JSON scene with ≥2 layers composes correctly, with z-order and position honored. `SIGHUP` with a modified scene applies the diff without restarting the pipeline.
3. **Adaptive bitrate:** With `balancer.algorithm=adaptive`, the encoder's `bitrate` property changes at least once per 5 s under simulated loss (inject via a lossy network path between the Go SRT socket and the receiver, or a `queue leaky=downstream` in front of the `identity` byte-tap).
4. **SRT output:** Video reaches an external SRT receiver (via the **Go-owned `srtgo` socket** fed by the `identity` byte-tap). Round-trip latency under 500 ms on localhost.
5. **Graceful shutdown:** `SIGTERM` stops the pipeline **and closes the Go SRT socket**, exiting 0 within 2 s.
6. **No regressions vs. original:** Every optional element listed in §2 is honored when present; none is required.
7. **CI green:** All unit tests pass without GStreamer. All integration tests pass in the Docker CI image.

---

## 10. Deliverables Checklist

- [x] `cmd/ctrlirl-coder` (`main.go` + `run_gst.go`) builds and runs — streams SRT end-to-end.
- [x] `internal/config` with TOML schema mirroring the original INI keys.
- [x] `internal/balancer` with three algorithms + tests.
- [x] `internal/srt` with `StatsSource` + `Client` (Go `srtgo` send socket) + `mapStats` + `sendRateTracker` + `MockStatsSource` + tests.
- [x] `internal/gstpull` pulling the terminal `appsink` with `gst_app_sink_pull_sample` (cgo) → send callback; stats + stall detector.
- [x] `internal/gstutil` helpers (gstreamer).
- [x] `internal/compositor` with command-goroutine pipeline ownership + `checkContract` (verifies `appsink` terminal, forces `drop=true`).
- [x] `internal/layer` with video + image + text, element chains via `BuildChain`.
- [x] `internal/scene` with JSON loader + diff/apply.
- [x] `internal/controller` wiring StatsSource + Applier + balancer loop.
- [x] `pipeline/example-full.pipeline` (terminal: `… ! mpegtsmux ! appsink name=appsink`; SRT sent by the Go socket).
- [x] `pipeline/example-layered.pipeline` (new, demonstrates compositor).
- [x] `config.toml` (TOML, original schema + `[scene]` table).
- [ ] `README.md` containing the pipeline contract table from §2 and the recorded `appsink` revision.
- [x] `IMPLEMENTATION.md` documenting actual progress + the Go-owned SRT socket + the two "no video" fixes.
- [ ] `Dockerfile` + `.github/workflows/ci.yml`.
- [ ] `Makefile` targets: `build`, `test`, `test-integration`, `docker`.

---

## 11. Open Questions to Resolve During Implementation

1. **PTS fixup element**: The original README says "`ptsfixup`" but does not specify whether it must be an `identity` or a custom element. Inspect the original C code to confirm — if it's a plain `identity`, no runtime action needed; if it's a wrapper that mutates `PTS`, replicate that wrapper.
2. **Audio mux (resolved):** Audio is **muxed into the same socket** as video. `mpegtsmux` sits **between** the encoder and the terminal `appsink`, so the Go socket transmits a single MPEG-TS carrying both video and AAC (verified — the captured stream decodes to H.264 + AAC).
3. **Balancer exact thresholds**: Read `balancer_adaptive.c` from the original repo and mirror the thresholds; the defaults in §5.7 are approximations and must be verified against the source.
4. **Overlay format string**: The original `overlay_ui.c` builds the string from a format in the config. Confirm the placeholder set (`%d`, `%.1f`, etc.) before Phase 5.
5. **Frame-production liveness (resolved):** Mirrors ctrlirl_coder's `frame_liveness` watchdog. `general.frame_stall_ms` (default **3000**, the original's `FRAME_LIVENESS_DEFAULT_STALL_MS`) is the threshold: if `internal/gstpull` has pulled no encoded frame for ≥ that long, `Puller.Stalled()` reports true and the controller's 500 ms loop forces the encoder to `MinBitrate` (zombie-encode emergency) and logs an error. The baseline is seeded at `gstpull.Attach` time so the time-to-first-frame is bounded.

---

## 12. Reference: Minimal Working Example (for smoke test)

The following pipeline **must** run at the end of Phase 2. It satisfies every mandatory element of the contract:

```
videotestsrc is-live=true
  ! videoconvert
  ! identity name=v_delay signal-handoffs=TRUE
  ! identity name=ptsfixup
  ! textoverlay name=overlay text="init" valignment=top halignment=left font-desc="Sans 24"
  ! x264enc name=venc_kbps bitrate=2000 tune=zerolatency
  ! h264parse
  ! mpegtsmux name=mux
  ! appsink name=appsink
```

The SRT destination (`srt://127.0.0.1:9101`) is **not** in the pipeline string: the `srt.Client` (Go `srtgo` socket) dials it directly and `gstpull` feeds it the bytes pulled from the terminal `appsink` (`gst_app_sink_pull_sample`). In a real run an SRT listener is required — e.g. `gst-launch-1.0 srtsrc uri=srt://:9101 mode=listener ! fakesink` — because a Go `srtgo` client in default `caller` mode has no peer to send to otherwise. (No-receiver smoke test: just point the client at a port and watch the logs for the expected "no peer" state.)

If your `gstutil` and `compositor` packages cannot start this pipeline, discover the named elements (`v_delay`, `ptsfixup`, `overlay`, `venc_kbps`, `appsink`), verify the `appsink` terminal and force `drop=true` on it, set the encoder `bitrate`, update the overlay text, and pull one sample in ≤200 LOC of test code — the architecture is wrong. Fix it before continuing.

## 13. TypeScript / Bun Bindings

The original CtrlIRLCoder ships TypeScript bindings (`bindings/typescript`, published as `@ceralive/ctrlirl_coder`) consumed by CeraUI and other control planes. The new bindings for CtrlIRLCoder-Go must **mirror this API one-to-one** so existing consumers work unchanged, while adding the multi-layer scene generation that the Go compositor backend requires.

Shell wrappers are explicitly **out of scope**. All process control, config generation, and signal delivery must happen through the TypeScript/Bun package.

### 13.1 Package Layout

```
ctrlirl_coder-go/
├── bindings/
│   └── typescript/                  # npm package @ctrlirl/ctrlirl-coder
│       ├── package.json
│       ├── tsconfig.json
│       ├── bun.lock
│       ├── README.md
│       ├── src/
│       │   ├── index.ts             # Public exports
│       │   ├── schemas.ts           # Zod schemas (config, scene, CLI options)
│       │   ├── config.ts            # buildCtrlIRLCoderConfig, serializeCtrlIRLCoderConfig
│       │   ├── args.ts              # buildCtrlIRLCoderArgs
│       │   ├── pipeline.ts          # PipelineBuilder (single-layer, parity with original)
│       │   ├── scene.ts             # SceneBuilder (compositor layers, NEW)
│       │   ├── process.ts           # spawnCtrlIRLCoder, sendHup, sendTerm, writeConfig
│       │   └── types.ts             # Shared TypeScript types
│       └── test/
│           ├── config.test.ts
│           ├── args.test.ts
│           ├── pipeline.test.ts
│           ├── scene.test.ts
│           └── process.integration.test.ts
```

### 13.2 `package.json`

```json
{
    "name": "@ctrlirl/ctrlirl-coder",
    "version": "0.1.0",
    "type": "module",
    "main": "./src/index.ts",
    "types": "./src/index.ts",
    "exports": {
        ".": "./src/index.ts"
    },
    "engines": {
        "bun": ">=1.1.0",
        "node": ">=20.0.0"
    },
    "dependencies": {
        "zod": "^4.0.0"
    },
    "devDependencies": {
        "@types/bun": "latest",
        "typescript": "^5.6.0"
    },
    "scripts": {
        "test": "bun test",
        "typecheck": "tsc --noEmit"
    }
}
```

**Package name decision:** use `@ctrlirl/ctrlirl-coder` (distinct from the original `@ceralive/ctrlirl_coder`) so both backends can coexist during migration without dependency conflicts.

### 13.3 `src/schemas.ts` — Zod schemas

Mirror the original bindings' schema shape exactly, and add `SceneSchema`.

```typescript
import { z } from "zod";

// --- Config (TOML) ---
export const GeneralSchema = z.object({
    log_level: z.enum(["debug", "info", "warning", "error"]).default("info"),
    balancer: z.enum(["adaptive", "fixed", "aimd"]).default("adaptive"),
    min_bitrate: z.number().int().min(100).default(500),
    max_bitrate: z.number().int().max(100_000).default(20_000),
    start_bitrate: z.number().int().optional(),
});

export const SRTSchema = z.object({
    host: z.string().default("127.0.0.1"),
    port: z.number().int().default(9000),
    latency: z.number().int().default(2000),
    passphrase: z.string().optional(),
    streamid: z.string().optional(),
});

export const EncodingSchema = z.object({
    width: z.number().int().default(1920),
    height: z.number().int().default(1080),
    framerate: z.number().int().default(30),
    codec: z.enum(["h264", "h265"]).default("h264"),
});

export const OverlaySchema = z.object({
    enabled: z.boolean().default(false),
    format: z.string().default("Bitrate: %d kbps\\nRTT: %.1f ms"),
    interval_ms: z.number().int().default(1000),
});

export const CtrlIRLCoderConfigSchema = z.object({
    general: GeneralSchema,
    srt: SRTSchema,
    encoding: EncodingSchema,
    overlay: OverlaySchema,
});

// --- Scene (JSON, NEW) ---
export const LayerPropertiesSchema = z.object({
    zorder: z.number().int().default(0),
    xpos: z.number().int().default(0),
    ypos: z.number().int().default(0),
    width: z.number().int().optional(),
    height: z.number().int().optional(),
    alpha: z.number().min(0).max(1).default(1.0),
});

export const SceneLayerSchema = z.object({
    id: z.string(),
    type: z.enum(["video", "image", "text"]),
    source: z.string(),
    properties: LayerPropertiesSchema,
});

export const SceneSchema = z.object({
    name: z.string().default("main"),
    canvas: z.object({
        width: z.number().int().default(1280),
        height: z.number().int().default(720),
    }),
    layers: z.array(SceneLayerSchema),
});

export type CtrlIRLCoderConfig = z.infer<typeof CtrlIRLCoderConfigSchema>;
export type Scene = z.infer<typeof SceneSchema>;
export type SceneLayer = z.infer<typeof SceneLayerSchema>;
```

### 13.4 `src/config.ts` — Config & scene serializers

```typescript
import {
    CtrlIRLCoderConfigSchema,
    SceneSchema,
    type CtrlIRLCoderConfig,
    type Scene,
} from "./schemas";

export function buildCtrlIRLCoderConfig(
    partial: Partial<CtrlIRLCoderConfig> = {},
): {
    config: CtrlIRLCoderConfig;
    ini: string;
} {
    const config = CtrlIRLCoderConfigSchema.parse(partial);
    const ini = serializeCtrlIRLCoderConfig(config);
    return { config, ini };
}

export function serializeCtrlIRLCoderConfig(
    config: CtrlIRLCoderConfig,
): string {
    // Emit TOML matching the Go config.Load() schema.
    // Sections: [general], [srt], [encoding], [overlay]
    // MUST NOT emit legacy flags or fields the Go loader rejects.
    return [
        "[general]",
        `log_level=${config.general.log_level}`,
        `balancer=${config.general.balancer}`,
        `min_bitrate=${config.general.min_bitrate}`,
        `max_bitrate=${config.general.max_bitrate}`,
        ...(config.general.start_bitrate !== undefined
            ? [`start_bitrate=${config.general.start_bitrate}`]
            : []),
        "",
        "[srt]",
        `host=${config.srt.host}`,
        `port=${config.srt.port}`,
        `latency=${config.srt.latency}`,
        ...(config.srt.passphrase
            ? [`passphrase=${config.srt.passphrase}`]
            : []),
        ...(config.srt.streamid ? [`streamid=${config.srt.streamid}`] : []),
        "",
        "[encoding]",
        `width=${config.encoding.width}`,
        `height=${config.encoding.height}`,
        `framerate=${config.encoding.framerate}`,
        `codec=${config.encoding.codec}`,
        "",
        "[overlay]",
        `enabled=${config.overlay.enabled}`,
        `format=${config.overlay.format}`,
        `interval_ms=${config.overlay.interval_ms}`,
        "",
    ].join("\n");
}

export function buildScene(partial: Partial<Scene> = {}): {
    scene: Scene;
    json: string;
} {
    const scene = SceneSchema.parse(partial);
    return { scene, json: JSON.stringify(scene, null, 2) };
}
```

**Contract requirement:** `serializeCtrlIRLCoderConfig` must produce TOML that the Go `config.Load()` accepts without modification. Enforced by the integration test in §13.9.

### 13.5 `src/args.ts` — CLI argument builder

```typescript
export interface CtrlIRLCoderArgsOptions {
    pipelineFile?: string; // -p
    configFile?: string; // -c  (NEVER -b; -b was removed in original bindings)
    sceneFile?: string; // -s  (Go-only extension)
    host?: string;
    port?: number;
    latencyMs?: number;
    algorithm?: "adaptive" | "fixed" | "aimd";
    logLevel?: "debug" | "info" | "warning" | "error";
}

export function buildCtrlIRLCoderArgs(opts: CtrlIRLCoderArgsOptions): string[] {
    const args: string[] = [];
    if (opts.configFile) args.push("-c", opts.configFile);
    if (opts.pipelineFile) args.push("-p", opts.pipelineFile);
    if (opts.sceneFile) args.push("-s", opts.sceneFile); // Go extension
    if (opts.logLevel) args.push("--log-level", opts.logLevel);
    // NOTE: -b is intentionally never emitted.
    return args;
}
```

**Rule:** if both `pipelineFile` and `sceneFile` are given, the Go binary treats `-s` as taking precedence (scene composition overrides a flat pipeline). Document this in `README.md`.

### 13.6 `src/pipeline.ts` — PipelineBuilder (parity with original)

Preserve the original `PipelineBuilder` public API verbatim.

```typescript
export class PipelineBuilder {
    static build(opts: {
        hardware: "jetson" | "rk3588" | "n100" | "generic";
        source: string;
        overrides?: { resolution?: string; framerate?: number };
        writeTo?: string;
    }): { pipeline: string; path?: string };

    static listHardwareTypes(): string[];
    static listSources(hardware: string): string[];
}
```

**Contract validation:** `build()` must throw if the generated pipeline string lacks the SRT terminal (`appsink name=appsink`) or lacks both `venc_bps` and `venc_kbps`. This mirrors the (updated) Go backend's startup check (§2.1). The terminal `appsink` is the SRT byte source — Go drains it with `gst_app_sink_pull_sample` and sends over the `srtgo` socket, so no in-pipeline `srtsink` is expected.

```typescript
function validateContract(pipeline: string): void {
    // SRT terminal is `appsink name=appsink`; Go drains it
    // (gst_app_sink_pull_sample) and sends over the srtgo socket, so no
    // in-pipeline srtsink is expected.
    if (!/\bappsink\s+name=appsink\b/.test(pipeline)) {
        throw new Error(
            "pipeline contract violated: missing terminal `appsink name=appsink`",
        );
    }
    if (!/\bvenc_bps\b/.test(pipeline) && !/\bvenc_kbps\b/.test(pipeline)) {
        throw new Error(
            "pipeline contract violated: missing venc_bps/venc_kbps",
        );
    }
}
```

### 13.7 `src/scene.ts` — SceneBuilder (new, compositor layers)

```typescript
import { buildScene, type Scene } from "./config";
import type { SceneLayer } from "./schemas";

export class SceneBuilder {
    private layers: SceneLayer[] = [];
    private canvas = { width: 1280, height: 720 };

    setCanvas(width: number, height: number): this {
        this.canvas = { width, height };
        return this;
    }

    addVideoLayer(opts: {
        id: string;
        source: string; // e.g. "v4l2src device=/dev/video0"
        x?: number;
        y?: number;
        w?: number;
        h?: number;
        zorder?: number;
        alpha?: number;
    }): this {
        this.layers.push({
            id: opts.id,
            type: "video",
            source: opts.source,
            properties: {
                zorder: opts.zorder ?? 1,
                xpos: opts.x ?? 0,
                ypos: opts.y ?? 0,
                width: opts.w,
                height: opts.h,
                alpha: opts.alpha ?? 1.0,
            },
        });
        return this;
    }

    addImageLayer(opts: {
        id: string;
        filePath: string; // local path; "file://" prefix added internally
        x: number;
        y: number;
        w: number;
        h: number;
        zorder?: number;
        alpha?: number;
    }): this {
        this.layers.push({
            id: opts.id,
            type: "image",
            source: `file://${opts.filePath}`,
            properties: {
                zorder: opts.zorder ?? 10,
                xpos: opts.x,
                ypos: opts.y,
                width: opts.w,
                height: opts.h,
                alpha: opts.alpha ?? 1.0,
            },
        });
        return this;
    }

    addTextLayer(opts: {
        id: string;
        text: string;
        x: number;
        y: number;
        w: number;
        h: number;
        zorder?: number;
        alpha?: number;
    }): this {
        this.layers.push({
            id: opts.id,
            type: "text",
            source: `text:${opts.text}`, // Go layer parses this prefix
            properties: {
                zorder: opts.zorder ?? 20,
                xpos: opts.x,
                ypos: opts.y,
                width: opts.w,
                height: opts.h,
                alpha: opts.alpha ?? 1.0,
            },
        });
        return this;
    }

    build(): { scene: Scene; json: string } {
        return buildScene({
            name: "main",
            canvas: this.canvas,
            layers: this.layers,
        });
    }

    writeTo(path: string): Promise<{ scene: Scene; json: string }> {
        const result = this.build();
        return Bun.write(path, result.json).then(() => result);
    }
}
```

**Contract requirement:** generated JSON must validate against `SceneSchema` and must be accepted by the Go `scene.Load()` with zero errors. Enforced by the integration test in §13.9.

### 13.8 `src/process.ts` — Process control (Bun-first, Node fallback)

No shell scripts. All signal delivery and file writing happens here.

```typescript
import { buildCtrlIRLCoderArgs, type CtrlIRLCoderArgsOptions } from "./args";
import { buildCtrlIRLCoderConfig, type CtrlIRLCoderConfig } from "./config";
import type { Scene } from "./schemas";

export interface SpawnOptions extends CtrlIRLCoderArgsOptions {
    binaryPath?: string;
    env?: Record<string, string>;
    onStdout?: (line: string) => void;
    onStderr?: (line: string) => void;
    onExit?: (code: number) => void;
}

export interface CtrlIRLCoderProcess {
    pid: number;
    kill(signal?: NodeJS.Signals): void;
    sendHup(): void;
    sendTerm(): void;
    wait(): Promise<number>;
}

const isBun = typeof (globalThis as any).Bun !== "undefined";

export function spawnCtrlIRLCoder(
    opts: SpawnOptions = {},
): CtrlIRLCoderProcess {
    const bin = opts.binaryPath ?? resolveBinary();
    const args = buildCtrlIRLCoderArgs(opts);
    const env = { ...process.env, ...(opts.env ?? {}) };

    if (isBun) return spawnBun(bin, args, env, opts);
    return spawnNode(bin, args, env, opts);
}

function resolveBinary(): string {
    const env = process.env.CTRLIRL_CODER_BIN;
    if (env) return env;
    return "ctrlirl_coder-go";
}

async function writeConfig(
    path: string,
    config: CtrlIRLCoderConfig,
): Promise<void> {
    const { ini } = buildCtrlIRLCoderConfig(config);
    await writeFile(path, ini);
}

async function writeScene(path: string, scene: Scene): Promise<void> {
    await writeFile(path, JSON.stringify(scene, null, 2));
}

async function writeFile(path: string, content: string): Promise<void> {
    if (isBun) {
        await (globalThis as any).Bun.write(path, content);
    } else {
        const fs = await import("node:fs/promises");
        await fs.writeFile(path, content, "utf8");
    }
}

export { writeConfig, writeScene };
```

**Rules:**

- `Bun` must be feature-detected **at call time**, never captured at module scope.
- `sendHup()` calls `process.kill(pid, "SIGHUP")` — never `os.Exit`.
- `sendTerm()` calls `process.kill(pid, "SIGTERM")` for graceful shutdown.
- The implementation of `spawnBun` and `spawnNode` is straightforward (`Bun.spawn` vs `child_process.spawn`), both must return a `CtrlIRLCoderProcess`; the line-by-line stdout/stderr piping is shared helper logic.

### 13.9 `src/index.ts` — Public surface

```typescript
export * from "./schemas";
export * from "./config";
export * from "./args";
export * from "./pipeline";
export * from "./scene";
export * from "./process";
```

**API parity requirement:** every export of the original `@ceralive/ctrlirl_coder` package must have an identically named export here with a compatible signature. `SceneBuilder` is the only addition; nothing is removed.

### 13.10 Implementation Phases

These run in parallel with the Go backend after §6 Phase 1. Shell work is removed from the plan.

#### Binding Phase A — Scaffolding & schemas (1 day)

- Create `bindings/typescript` with `package.json`, `tsconfig.json`.
- Implement `schemas.ts`, `config.ts`, `args.ts` with unit tests.

**Acceptance:** `bun test bindings/typescript/test/config.test.ts bindings/typescript/test/args.test.ts` passes.

#### Binding Phase B — PipelineBuilder (1 day)

- Port `PipelineBuilder` verbatim, add `validateContract` guard.
- Unit tests covering all hardware/source combinations.

**Acceptance:** `build()` throws when a deliberately broken template lacks the terminal `appsink name=appsink` or `venc_bps`/`venc_kbps`.

#### Binding Phase C — SceneBuilder & process helpers (2 days)

- Implement `SceneBuilder`.
- Implement `process.ts` with Bun-first, Node-fallback logic.
- Integration test: `SceneBuilder` → `writeScene` → `spawnCtrlIRLCoder({ sceneFile })` → `sendHup()` → `sendTerm()` → assert exit 0. Gated on Go backend Phase 3 or later.

**Acceptance:** Go binary starts with a generated two-layer scene, receives SIGHUP, and exits cleanly on SIGTERM.

#### Binding Phase D — Docs & CI (0.5 day)

- `README.md` mirroring the original package's structure, plus a scene section.
- CI job `bindings` that runs `bun test` and `bun run typecheck`.

**Acceptance:** CI green on all binding tests.

### 13.11 Testing Matrix

| Test                           | Runtime                 | Requires Go binary | Runs on      |
| :----------------------------- | :---------------------- | :----------------- | :----------- |
| Schema validation              | Bun                     | No                 | Every commit |
| Config TOML generation         | Bun                     | No                 | Every commit |
| Args (`-c`/`-p`/`-s`)          | Bun                     | No                 | Every commit |
| PipelineBuilder contract       | Bun                     | No                 | Every commit |
| SceneBuilder JSON schema       | Bun                     | No                 | Every commit |
| TOML round-trip vs Go          | Bun + Go `--dry-run`    | Yes (Phase 1+)     | CI nightly   |
| Scene round-trip vs Go         | Bun + Go `--dry-run -s` | Yes (Phase 3+)     | CI nightly   |
| Process spawn + SIGHUP/SIGTERM | Bun                     | Yes (Phase 3+)     | CI nightly   |

### 13.12 Acceptance Criteria

1. **API parity:** every export of `@ceralive/ctrlirl_coder` exists in `@ctrlirl/ctrlirl-coder` with a compatible signature. A consumer can swap import paths and compile unchanged.
2. **Bun-first, Node-compatible:** `bun test` passes; importing the package under Node 20+ does not throw (no Bun globals at module scope).
3. **Config round-trip:** `serializeCtrlIRLCoderConfig` output is accepted by the Go `--dry-run` with exit 0.
4. **Scene round-trip:** `SceneBuilder.writeTo()` output is accepted by `ctrlirl_coder-go --dry-run -s scene.json` with exit 0.
5. **Contract validation:** `PipelineBuilder.build()` throws on pipelines missing the terminal `appsink name=appsink` or `venc_kbps`/`venc_bps`.
6. **Process control:** `spawnCtrlIRLCoder → sendHup → sendTerm` completes a full lifecycle against the real Go binary. No shell wrappers involved.

### 13.13 Deliverables Checklist

Replace the corresponding items in §10 with:

- [ ] `bindings/typescript/package.json` (`name: "@ctrlirl/ctrlirl-coder"`, `type: "module"`).
- [ ] `bindings/typescript/tsconfig.json` (`moduleResolution: "bundler"`, `strict: true`).
- [ ] `bindings/typescript/src/schemas.ts` — Zod schemas for config + scene.
- [ ] `bindings/typescript/src/config.ts` — TOML + JSON serializers.
- [ ] `bindings/typescript/src/args.ts` — CLI builder (`-c`, `-p`, `-s`; never `-b`).
- [ ] `bindings/typescript/src/pipeline.ts` — `PipelineBuilder` (parity + contract guard).
- [ ] `bindings/typescript/src/scene.ts` — `SceneBuilder` (new).
- [ ] `bindings/typescript/src/process.ts` — Bun-first spawn + signals (no shell).
- [ ] `bindings/typescript/src/index.ts` — public re-exports.
- [ ] `bindings/typescript/test/*.test.ts` — unit + integration tests.
- [ ] `bindings/typescript/README.md` — API docs mirroring the original, plus a scene section.
- [ ] CI job `bindings` running `bun test` and `bun run typecheck`.

### 13.14 Open Questions

1. **Default scene path:** if `-s` is omitted but `[scene] config_path` is set in the TOML config, the Go binary should use that path. Confirm with the Go team before finalizing `args.ts` documentation.
2. **Node support tier:** "Node-compatible" here means _import works and `writeConfig`/`writeScene`/`spawnCtrlIRLCoder` function_. Bun is the primary target; Node is best-effort and covered by one smoke test only.
3. **Legacy `-b` flag:** never emitted by `buildCtrlIRLCoderArgs`. The Go binary must reject `-b` with a clear error message.

# Section 14 — SRTLA Output & Go `srtla_send` Rewrite

### 14.1 Context and Goal

SRTLA (SRT Link Aggregation) is an SRT extension that transports a single SRT stream across multiple network interfaces simultaneously (bonding) to aggregate bandwidth and provide redundancy. It is typically used for mobile IRL streaming, where the capacity of several 4G/5G modems is summed.

The original `srtla_send` from BELABOX/IRLServer exists in C and, more recently, in Rust. The goal is to produce a **native Go implementation of the sender** fully integrated into CtrlIRLCoder-Go as an alternative to direct SRT output. Users can select `srt` (existing behavior) or `srtla` (new behavior) in the configuration, and CtrlIRLCoder-Go runs the SRTLA sender logic itself, with no external process.

### 14.2 SRTLA Sender Architecture in Go

The SRTLA sender acts as a **UDP proxy** between a local SRT source and a remote SRTLA receiver. It accepts SRT packets from the local SRT socket, distributes them across the configured network interfaces (IP addresses), and processes SRTLA control packets (ACK/NAK/keepalive) coming back from the receiver.

```text
[SRT encoder (GStreamer appsink)]
        │  SRT packets (UDP)
        ▼
┌─────────────────────────────┐
│  CtrlIRLCoder-Go SRTLA Sender  │
│  ┌───────────────────────┐  │
│  │  UDP listener :6000   │  │  ← local SRT stream lands here
│  └──────────┬────────────┘  │
│             │                │
│  ┌──────────▼────────────┐  │
│  │  Scheduler / Balancer │  │  ← picks uplink by score
│  └──────────┬────────────┘  │
│             │                │
│  ┌──────────▼────────────┐  │
│  │  Uplink 1 (IP A)      │──┼──▶ SRTLA receiver
│  │  Uplink 2 (IP B)      │──┼──▶ (srtla_rec)
│  │  Uplink N (IP N)      │──┼──▶
│  └───────────────────────┘  │
│                             │
│  ┌───────────────────────┐  │
│  │  Feedback processor   │  │  ← ACK/NAK/keepalive from receiver
│  └───────────────────────┘  │
└─────────────────────────────┘
```

**Data flow:**

1. The local SRT encoder (in our case GStreamer `appsink` → SRT socket) sends packets to `127.0.0.1:6000` (or a configurable port).
2. The SRTLA sender accepts these packets, wraps them in an SRTLA header, and dispatches them over the selected uplink.
3. The receiver (`srtla_rec`) accepts packets from multiple uplinks, reconstructs the original SRT stream, and forwards it.
4. The receiver sends SRTLA ACK/NAK and keepalives back over the same uplinks. The sender processes them and updates uplink scores.

### 14.3 Key Components and Specifications

#### 14.3.1 `internal/srtla/protocol.go` — SRTLA protocol

Constants and packet formats matching the reference implementation.

```go
package srtla

const (
    // SRTLA message types
    TypeKeepalive = 0x9000
    TypeACK       = 0x9100
    TypeNAK       = 0x9200
    TypeReg1      = 0x9300
    TypeReg2      = 0x9400
    TypeReg3      = 0x9500

    // SRT message types (for identification)
    SRTTypeData      = 0x0000
    SRTTypeHandshake = 0x8000
    SRTTypeACK       = 0x8002
    SRTTypeNAK       = 0x8003
)

// SRTLAHeader is the 8-byte header wrapping an SRT packet.
type SRTLAHeader struct {
    Type   uint16 // SRTLA type (0x9000–0x9500)
    Length uint16 // payload length (original SRT packet)
    Seq    uint32 // sequence number for SRTLA
}

// RegistrationPacket is the format of REG1/REG2/REG3.
type RegistrationPacket struct {
    Type     uint16
    ID       uint32 // connection group ID
    UplinkID uint32 // uplink ID within the group
    // ... additional fields as needed
}
```

**Rules:**

- The SRTLA header must be **8 bytes** and must be prepended to the original SRT packet.
- REG1 initializes the group, REG2 assigns an uplink, REG3 confirms registration.
- Keepalive packets carry a timestamp for RTT measurement.

#### 14.3.2 `internal/srtla/sender.go` — Main sender

```go
type Sender struct {
    cfg        SRTLAConfig
    uplinks    []*Uplink
    localConn  *net.UDPConn   // local SRT listener (:6000)
    regID      uint32
    scheduler  Scheduler
    stopCh     chan struct{}
}

type SRTLAConfig struct {
    ListenAddr    string   // "127.0.0.1:6000"
    ReceiverHost  string   // "srtla.example.com"
    ReceiverPort  int      // 5000
    UplinkIPs     []string // ["192.168.1.10", "10.0.0.5"]
    Mode          string   // "enhanced" | "classic"
    ConnTimeoutMs int      // default 5000
}

func NewSender(cfg SRTLAConfig) (*Sender, error)

func (s *Sender) Run(ctx context.Context) error
//   1. Open a UDP listener on ListenAddr.
//   2. For each uplink IP, create a UDP socket bound to that IP.
//   3. Perform REG1/REG2/REG3 registration across all uplinks.
//   4. Start goroutines:
//      - readLoop: reads SRT packets from the local listener, hands to scheduler.
//      - feedbackLoop: reads SRTLA ACK/NAK/keepalive from uplinks, updates scores.
//      - keepaliveLoop: sends keepalives every N ms on each uplink.
//   5. Block until ctx.Done() or error.

func (s *Sender) Close() error
```

**Registration (REG1/REG2/REG3):**

- The sender sends REG1 on the first uplink, requesting a `regID`.
- The receiver responds with REG2 carrying the assigned `regID` and the list of recommended uplinks (or just confirmation).
- The sender sends REG3 on each uplink with `regID` and `uplinkID`.
- Once registration succeeds, data transmission can begin.

#### 14.3.3 `internal/srtla/uplink.go` — Per-uplink management

```go
type Uplink struct {
    IP       string
    conn     *net.UDPConn
    regID    uint32
    uplinkID uint32
    // Statistics
    rttMs         float64
    lossRate      float64
    inFlight      int
    lastACK       time.Time
    lastKeepalive time.Time
    score         float64
}
```

**Scoring (enhanced mode):**

- Base score is derived from `lossRate` and `rttMs`.
- **Exponential NAK decay** (gradual recovery after loss) and **burst detection** (penalty on ≥5 NAKs in a short window) are applied.
- **Score hysteresis** (10% threshold) prevents uplink flapping.
- An uplink holding a large `inFlight` backlog without fresh ACKs is temporarily deselected (stall deselect).

#### 14.3.4 `internal/srtla/scheduler.go` — Uplink selection

```go
type Scheduler interface {
    Select(uplinks []*Uplink, packetSize int) *Uplink
}

type EnhancedScheduler struct { /* ... */ }
type ClassicScheduler struct { /* ... */ }
```

**Classic mode:** pure capacity-based selection with no quality awareness — a faithful replica of the original `srtla_send.c`.

**Enhanced mode (default):** weighted selection by `score`, factoring in `inFlight` and `rtt`.

### 14.4 Integration into CtrlIRLCoder-Go

#### 14.4.1 New configuration section

Extend `internal/config` with a `[srtla]` section:

```ini
[srtla]
enabled = false
listen_addr = 127.0.0.1:6000
receiver_host = srtla.example.com
receiver_port = 5000
uplink_ips = 192.168.1.10,10.0.0.5
mode = enhanced
conn_timeout_ms = 5000
```

**Behavior:**

- If `srtla.enabled = false` → CtrlIRLCoder-Go uses the existing direct SRT output (§5.6).
- If `srtla.enabled = true` → CtrlIRLCoder-Go **does not** open a direct SRT connection to the `[srt]` target. Instead, it opens a local SRT listener on `srtla.listen_addr` and sends all `appsink` output there. The SRTLA sender is responsible for delivery to `srtla.receiver_host:srtla.receiver_port`.

#### 14.4.2 Changes to `internal/controller`

```go
func (c *Controller) setupOutput() error {
    if c.cfg.SRTLA.Enabled {
        // 1. Start the SRTLA sender (internal Go implementation).
        sender, err := srtla.NewSender(c.cfg.SRTLA.ToSenderConfig())
        // 2. Create an SRT client that connects to srtla.listen_addr
        //    (i.e., to our own SRTLA sender).
        srtClient, err := srt.Dial("srt://" + c.cfg.SRTLA.ListenAddr + "?mode=caller")
        // 3. appsink → srtClient.Send() (same loop as direct SRT).
        c.srtla = sender
        c.srt = srtClient
    } else {
        // Existing behavior: direct SRT to c.cfg.SRT.URI
        c.srt, err = srt.Dial(c.cfg.SRT.URI)
    }
    return nil
}
```

**Overhead note:** Because the SRTLA sender is a native part of the process, there is no external process and no additional latency. The local SRT connection to `127.0.0.1` has negligible overhead.

#### 14.4.3 Statistics and adaptive bitrate

The balancer (§5.7) needs SRT statistics (RTT, loss). In SRTLA mode, it can pull them **directly from the SRTLA sender**, which aggregates across all uplinks. This is preferable to reading stats from the local SRT connection, which does not reflect uplink conditions.

```go
// In internal/srt/stats.go, extend Stats with SRTLA fields, or
// add a method on Sender:
func (s *Sender) AggregateStats() srt.Stats {
    // Returns averaged RTT, aggregate loss, summed bandwidth.
}
```

The balancer then receives `AggregateStats()` instead of `srtClient.Stats()`.

### 14.5 Implementation Phases for SRTLA

These phases follow the main plan (§6) and can start once Phase 1 (pure-Go core) is done.

#### SRTLA Phase 1 — Protocol and unit tests (2 days)

- Implement `protocol.go` with constants and header serialization/deserialization.
- Write unit tests for pack/unpack of REG1/REG2/REG3, keepalive, ACK/NAK.
- Verify against reference packets from `irlserver/srtla` (captured packets or simulation).

**Acceptance:** `go test ./internal/srtla/...` passes; serialized packets match the expected binary format.

#### SRTLA Phase 2 — Sender with a single uplink (3 days)

- Implement `Sender` with one uplink.
- Test against a real `srtla_rec` (e.g., Docker image `bardiir/srtla_rec` or `irlserver/srtla`).
- Verify REG1/REG2/REG3 handshake and SRT packet pass-through.

**Acceptance:** An SRT stream from `ffmpeg` or `gst-launch` traverses the Go sender into `srtla_rec` and is delivered on output.

#### SRTLA Phase 3 — Multi-uplink and scheduler (3 days)

- Add multi-uplink support (bind to different IPs).
- Implement `EnhancedScheduler` and `ClassicScheduler`.
- Implement scoring, stall deselect, and keepalive RTT measurement.

**Acceptance:** With two uplinks (simulatable via `tc netem` or two network namespaces), the sender distributes packets and reacts correctly to loss on one of them.

#### SRTLA Phase 4 — Integration into CtrlIRLCoder-Go (2 days)

- Extend configuration.
- Adjust `Controller` for SRTLA mode.
- Wire `AggregateStats` into the balancer.

**Acceptance:** `ctrlirl_coder-go -c config.ini` with `srtla.enabled=true` streams over SRTLA, and adaptive bitrate responds to changes.

#### SRTLA Phase 5 — Hardening (1 day)

- SIGHUP reload of the IP list (as in the original `srtla_send`).
- Graceful shutdown (close uplinks, drain in-flight packets).
- Logging and metrics.

### 14.6 SRTLA Test Matrix

| Test                             | Environment               | Requires `srtla_rec` | Runs on      |
| :------------------------------- | :------------------------ | :------------------- | :----------- |
| Protocol pack/unpack             | None                      | No                   | Every commit |
| Sender single uplink             | Docker `srtla_rec`        | Yes                  | CI nightly   |
| Multi-uplink simulation          | `tc netem` + 2 interfaces | Yes                  | CI nightly   |
| Integration with CtrlIRLCoder-Go | Full stack                | Yes                  | CI nightly   |
| SIGHUP reload                    | Full stack                | Yes                  | CI nightly   |

### 14.7 SRTLA Acceptance Criteria

1. **Functionality:** SRT stream passes from `appsink` through the Go SRTLA sender into `srtla_rec` and is delivered losslessly.
2. **Multi-uplink:** With two uplinks, the sender distributes packets proportionally to score and correctly re-routes traffic when one uplink fails.
3. **Classic mode:** `mode=classic` produces behavior identical to the original `srtla_send.c` (capacity selection).
4. **Balancer integration:** `AggregateStats()` provides valid data and adaptive bitrate responds to it.
5. **Reload:** `SIGHUP` with a modified `uplink_ips` applies the change without restarting the stream.
6. **No external process:** All SRTLA logic runs inside CtrlIRLCoder-Go.

### 14.8 TypeScript / Bun Bindings Update

Extend the schema (and `SceneBuilder` / `CtrlIRLCoderConfigSchema`) with SRTLA:

```typescript
export const SrtlaSchema = z.object({
    enabled: z.boolean().default(false),
    listen_addr: z.string().default("127.0.0.1:6000"),
    receiver_host: z.string(),
    receiver_port: z.number().int().default(5000),
    uplink_ips: z.array(z.string()).default([]),
    mode: z.enum(["enhanced", "classic"]).default("enhanced"),
    conn_timeout_ms: z.number().int().default(5000),
});

// Add to CtrlIRLCoderConfigSchema:
// srtla: SrtlaSchema.optional()
```

`serializeCtrlIRLCoderConfig` must emit the `[srtla]` section when `enabled=true`. `buildCtrlIRLCoderArgs` is unchanged (SRTLA is configured via INI only).

### 14.9 Deliverables Checklist Update

Add to §10:

- [ ] `internal/srtla/protocol.go` — SRTLA headers and types.
- [ ] `internal/srtla/sender.go` — main sender.
- [ ] `internal/srtla/uplink.go` — per-uplink management.
- [ ] `internal/srtla/scheduler.go` — enhanced + classic.
- [ ] `internal/srtla/*_test.go` — unit tests.
- [ ] `[srtla]` section in `ctrlirl_coder.conf.example`.
- [ ] Integration in `internal/controller`.
- [ ] `SrtlaSchema` in TypeScript bindings.
- [ ] CI job with `srtla_rec` in Docker.

### 14.10 Open Questions

1. **SRTLA listener:** Should CtrlIRLCoder-Go open the SRT listener on `srtla.listen_addr` as `caller` or `listener`? Since the SRTLA sender is local, the local SRT socket should be in `caller` mode, connecting to `srtla.listen_addr` where the SRTLA sender listens. Verify against the behavior of the original `srtla_send`, which expects an SRT stream on port 6000.
2. **Source IP handling:** The original `srtla_send` uses `bind()` to a specific IP. In Go, the same behavior is achievable with `net.DialUDP` and `LocalAddr` set to that IP. Verify this works on Linux without requiring `SO_BINDTODEVICE`.
3. **Compatibility with `irlserver/srtla` vs. `BELABOX/srtla`:** Rust `irlserver/srtla_send` has an extended keepalive format (38 bytes). Decide whether to implement the extended keepalive or stick with the legacy (vanilla) format. Recommendation: implement both, with a configuration switch.

SRTLA implementation phases can run in parallel with Phases 2–4 of the main plan; the `Controller` integration requires completed Phase 1 (SRT client) and Phase 3 (compositor).

---

**End of handoff.** The implementing agent should begin with Phase 0 and can consider the contract table in §2 immutable. All other decisions documented here are recommendations and may be revised if they conflict with the original C source — but any revision must be recorded in the README and approved before merging.

# Section 15 — Runtime Remote Control (JSON-RPC 2.0 over Unix Socket)

## 15.1 Scope

Live control of the running compositor without restart: layer parameter
changes, layer add/remove, filter add/remove/update, scene switch and
save. Wire format: **JSON-RPC 2.0** over the **shared HTTP server**
(`POST /rpc`); the same listener also serves `GET /metrics` (Prometheus
text exposition format).

## 15.2 Transport & endpoint

- Address: `--api-addr <host:port>` flag, else `[control] addr` config.
  Empty → API server disabled, no listener created.
- Transport: plaintext HTTP (the listener is intended for a trusted local/
  management network; there is no TLS key-pair configuration).
- One request object (or a 2.0 batch array) per `POST /rpc`, request →
  response; an all-notification batch returns `204`. Multiple concurrent
  clients allowed; mutations are serialized under a single lock.

## 15.3 Methods

| Method          | Params                                                                | Notes                                                                           |
| --------------- | --------------------------------------------------------------------- | ------------------------------------------------------------------------------- |
| `system.ping`   | —                                                                     | liveness                                                                        |
| `system.status` | —                                                                     | current scene name, layer list (id/type/zorder/filter kinds), known scene names |
| `scene.get`     | —                                                                     | full current scene JSON                                                         |
| `scene.names`   | —                                                                     | named scenes                                                                    |
| `scene.load`    | `{name}` \| `{json}`                                                  | full scene switch (removes/updates/adds diffed)                                 |
| `scene.switch`  | `{name}`                                                              | alias of load                                                                   |
| `scene.save`    | `{path, name?}`                                                       | write current (or named) scene to file                                          |
| `layer.add`     | `{layer}`                                                             | new layer (id required, must not exist)                                         |
| `layer.remove`  | `{id}`                                                                |                                                                                 |
| `layer.set`     | `{id, source?, type?, text?, properties?, "text-options"?, filters?}` | present fields replace                                                          |
| `filter.add`    | `{id, filter, index?}`                                                | index = insert position, default append                                         |
| `filter.remove` | `{id, index}`                                                         |                                                                                 |
| `filter.set`    | `{id, index, kind?, params?}`                                         |                                                                                 |

## 15.4 Mutation semantics (invariants)

1. Every mutation produces a new scene object; it must pass
   `scene.Parse` + `Validate` before touching the pipeline.
2. `scene.Diff` emits ops ordered removes → updates → adds. Chain-affecting
   changes (type/source/text/text-options/filters) emit remove+add;
   pad-property-only changes emit a single in-place update.
3. Ops apply one at a time against the live pipeline. First failure stops
   the batch; already-applied ops are **inverted in reverse** (add →
   remove, remove → re-add original layer, update → original props) and
   the controller's scene is restored to the pre-mutation state.
4. SIGHUP file reloads and socket mutations share one scene state (the
   `rpc.Controller`); the file is authoritative on SIGHUP.
5. Errors: standard JSON-RPC codes for protocol problems; `-32000` for
   application failures (validation, pipeline). A failed mutation never
   leaves a partial scene.

## 15.5 Reference

Full usage, examples and the `nc -U` one-liners: `RPC.md`.
