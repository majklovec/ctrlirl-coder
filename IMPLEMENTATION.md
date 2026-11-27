# CtrlIRL-Coder — Implementation Progress

Status log for the Go rewrite of CtrlIRLCoder, per [`SPECS.md`](./SPECS.md). This file is updated
as work proceeds. Read the spec for the _requirements_; this file records _what is actually done,
how, and what remains_.

**Project name:** `ctrlirl-coder` (module `ctrlirl-coder`, binary `cmd/ctrlirl-coder`).
Renames from the original `ctrlirl_coder` are cosmetic; the pipeline contract is preserved.

---

## 1. Phase status

| Phase (spec §6) | Title                     | Status         | Notes                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| :-------------- | :------------------------ | :------------- | :---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 0               | Scaffolding               | ✅ done        | `go.mod` pins `go-gst v1.4.1`; repo layout in place; `Makefile`, `Dockerfile`, `scripts/integration-smoke.sh`, `.github/workflows/ci.yml` all present.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| 1               | Pure-Go core              | ✅ done        | `config`, `balancer` (adaptive/fixed/aimd), `scene` — build and **test green without GStreamer**. (`srt` was listed here at the time; it is cgo via the `srtgo` wrapper — see deviations #8/#10 — and the pure parts now live under `internal/sinks`/`sinks/srtla`.)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| 2               | GStreamer pipeline smoke  | ✅ done        | `gstutil` (`FromLaunch` w/ comment stripping, `EnsureInit`, `GetByName`, `SetStateSafe`, `Link`, `GetBytes`); full pipeline streams over SRT end-to-end locally (dev headers installed — see §6).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| 3               | Compositor + layers       | ✅ done        | `compositor` (command-goroutine pipeline, `AddLayer`/`RemoveLayer`/`UpdateLayerProps`, `SetBitrate`, `SetOverlayText`) + `layer` (flat element chains, `LinkChain` **after** bin insert). Layered pipeline reaches PLAYING and streams (see §2.4 gotcha).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| 4               | Scene + config reload     | ✅ done        | `scene` (Load/Diff/Apply + `Applier`) done and tested; SIGHUP diff-apply wired in `main`; reload verified by controller tests.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| 5               | SRT + full loop           | ✅ done        | SRT via a **Go-owned `srtgo` socket** fed by the terminal **`appsink`** (`internal/gstpull` pulls with `gst_app_sink_pull_sample` → `client.Write`); `srt.Client` (stats source) + `mapStats` + `sendRateTracker` + `MockStatsSource`; pure-Go `controller` (balancer tick + overlay loop + appsink stall gate, cold-link guard) tested; full pipeline streams to a live `srtsrc` listener.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| 6               | Hardening                 | 🔄 in progress | slog (done), graceful shutdown (done), **/metrics** endpoint: loopback-only HTTP `name value` scrape at `[control] metrics_addr` / `--metrics-addr` (sink rtt/loss/send rate/retrans/send-buffer + puller frames/dropped/throughput/last-frame-age); **bus-message drain**: GStreamer bus → slog (Error→error, Warning→warn, chatter→debug); **structured stats logging**: `sink stats` line every 5 s. Tests: metrics 4 (loopback refuse, 404, 405), bus drain 2 (async error → log, goroutine exits with pipeline).                                                                                                                                                                                                                                                                                                                                                                                                         |
| —               | TypeScript bindings (§13) | ✅ done        | `bindings/typescript` — `@ctrlirl/ctrlirl-coder`: Zod schemas for the Go TOML config + scene JSON, full TOML/JSON serializers, `buildCtrlIRLCoderArgs` (long flags), `PipelineBuilder` (all 4 hw × 3 source combos, `venc_bps`/`venc_kbps` + `appsink` contract), `SceneBuilder` (Go geometry/zorder invariants), `spawnCtrlIRLCoder` (Bun + Node fallback, SIGHUP/SIGTERM, no shell). 34 bun tests green incl. a dry-run round-trip against the real Go binary; `tsc --noEmit` clean; CI `bindings` job. **Deviations from the stale spec §13** (Go backend is ground truth): long flags `--config/--pipeline/--scene/--dry-run/--rpc-sock` (no `-b`), TOML tables per `internal/config` (not the spec's `host`/`port`/`encoding` shape), text layers use `text` + `text-options` (no `text:` source prefix), `log_level` via `CTRLIRL_LOG_LEVEL` env (no flag). Scene-flag precedence matches the binary: scene > pipeline. |

Legend: ✅ done · 🟡 partial/in-progress · ⬜ pending

---

## 2. Key technical decisions (and why)

### 2.1 GStreamer bindings: `go-gst v1.4.1`

- **Chosen:** `github.com/go-gst/go-gst v1.4.1` (import `pkg/gst`, `pkg/gstapp`), with
  `github.com/go-gst/go-glib v0.0.1`.
- **Why v1.4.1:** The classic-path releases **v1.0.0–v1.3.0 are all _retracted_ by the author**
  ("Handwritten bindings, not as stable as planned and not licensed open enough"). v1.4.1 is the
  author's current release and is **MIT-licensed**. Pinning it is the only sane choice.
  `go get …@v1.4.1` emits an advisory retraction _warning_ (from the classic versions in the
  module graph) — this is non-fatal; the explicit pin works and is retained by `go mod tidy`.
- **API shape:** v1.4.1 uses an **interface-based API** (`gst.Element`, `gst.Bin`, `gst.Pipeline`)
  over the classic v1.x `*Element` pointer API. Element properties use
  `SetObjectProperty`/`GetObjectProperty` (inherited from go-glib v2), **not**
  `SetProperty`/`GetProperty`. Pads use `RequestPadSimple("sink_%u")` + `GetStaticPad("src").Link(…)`.

### 2.2 SRT transport: Go-owned socket + terminal `appsink` (matches CtrlIRLCoder/belacoder)

The spec's early drafts went back and forth; the **current** architecture (re-confirmed by the
operator and matching the original CtrlIRLCoder/belacoder design **verbatim**) is that **Go owns
the SRT socket** and pulls the encoded bytes off a terminal **`appsink`**:

```
… ! mpegtsmux name=mux ! appsink name=appsink
                                     ▲
                        gstpull pulls samples here with
                        gst_app_sink_pull_sample (cgo,
                        libgstreamer-app) on a Go goroutine
                        ──(pull loop)──►  internal/sinks/srt Client (srtgo)
                                          owns the SRTSOCKET + sends
```

Why this and not GStreamer's `srtsink`:

- **Fine-grained SRT statistics for the adaptive controller.** The controller runs every 20 ms
  and needs `srt_bstats` RTT, `SRTO_SNDDATA` send-buffer occupancy, packet loss and throughput at
  that cadence. An in-pipeline `srtsink` can't be polled that way; a `socket` owned by Go can.
- **`srtla` bonding.** Owning the socket gives direct control over connection parameters and
  packet flow, which is what the `srtla` link-aggregation integration needs.
- **Tight coupling with the bitrate loop** (inherited from belacoder).
- **The appsink is the original data path.** `ctrlirl_coder`/`belacoder.c` drain an `appsink` with
  `gst_app_sink_pull_sample` → `srt_send`; this port does exactly the same. The appsink also
  yields **stats** Go consumes (frames, bytes, throughput kbit/s, negotiated framerate,
  last-PTS) and a **frame-production stall** signal that the controller uses to drop the
  encoder to its floor (see §2.5, §7).

Consequences (a revision _back to_ the original appsink path, see the change log):

- **Go data path exists.** `internal/gstpull` pulls samples off the `appsink` (cgo to
  `libgstreamer-app`) on a dedicated goroutine; `internal/sinks/srt` `Client.Write` sends to the
  socket. The pipeline never contains an SRT sink.
- **`internal/sinks/srt` is a real client** (`haivision/srtgo`), exposing `StatsSource`
  (`Stats()` over the live socket) **and** `Applier`; `MockStatsSource` remains for pure-Go tests.
- **The terminal is `appsink name=appsink`** (mandatory) — `libgstreamer-app` (dev + runtime) is
  a build/runtime prerequisite, matching the original tool. `contract.go` verifies the element
  is an `appsink` (GType) and forces `drop=true` so a stalled SRT sender back-pressures by
  dropping frames instead of wedging the pipeline.
- **`libsrt` is a native library wrapped by the `haivision/srtgo` cgo module** (not a GStreamer plugin). The build links the **CERALIVE fork** bundle (deviations #8/#10): `scripts/build-ceralive-srt.sh` produces `lib/libsrt.so.1.5` + `include/srt/*` + `build.json`, and the app links `-L<bundle>/lib -lsrt -Wl,-rpath,$ORIGIN` with the lib SHA-256 embedded via ldflags.
- **`drop=true` is required** on the `appsink` — set authoritatively in `contract.go`. Without
  it a full appsink back-pressures the whole pipeline when the socket stalls.

### 2.3 SRT statistics: from the live socket (`srtgo`), throughput measured on the write path

`internal/sinks/srt` reads stats straight off the Go-owned SRT socket via `srtgo`
(`client_stats.go` `mapStats`, the Go equivalent of the original's `srt_bstats` +
`SRTO_SNDDATA`). Field mapping:

| `sink.Stats` field     | srtgo `SrtStats` field                                                | type         | cumulative?   |
| :--------------------- | :-------------------------------------------------------------------- | :----------- | :------------ |
| `RTT`                  | `MsRTT`                                                               | float64 (ms) | instantaneous |
| `SendRateMbps`         | `MbpsSendRate` → **overridden by the write-path tracker** (see below) | float64      | instantaneous |
| `BandwidthEstimate`    | `MbpsBandwidth`                                                       | float64      | instantaneous |
| `PktSndLossTotal`      | `PktSndLossTotal`                                                     | int64        | **yes**       |
| `PktRetransTotal`      | `PktRetransTotal`                                                     | int64        | **yes**       |
| `ACKTotal`             | `PktRecvACKTotal`                                                     | int64        | **yes**       |
| `PacketsSentTotal`     | `PktSentTotal`                                                        | int64        | **yes**       |
| `PacketsReceivedTotal` | `PktRecvTotal`                                                        | int64        | **yes**       |
| `SendBufferBytes`      | `ByteSndBuf` (≙ `SRTO_SNDDATA`)                                       | int          | instantaneous |
| `TimestampMs`          | `MsTimeStamp`                                                         | uint64       | sampled       |

**Throughput is measured on the write path, not read from SRT.** `srtgo`'s `MbpsSendRate`
reads ~0 on low-RTT / loopback links (its packet-rate accounting doesn't tick there), and the
adaptive balancer's send-buffer threshold `RTT_TO_BS` = `throughput·rtt/pkt_size` **collapses to
0 when throughput is 0** — which pins the branch to "heavy" and the bitrate to the floor
(§7 "no video" fix, bug #2). So `Client` carries a `sendRateTracker`: `Write` records the bytes
actually sent and `Stats()` overrides `SendRateMbps` with the measured rate (when > 0). The
tracker had a first-call bug (returned before priming `lastT` → 0 forever) that is now fixed.

### 2.4 Layers: element chains, not bins

`go-gst v1.4.1` does **not** expose `gst_bin_add_ghost_pad`, so a `*gst.Bin`'s source pad can't be
reached from Go to link into the `compositor`. The layer design
(spec §5.4) therefore returns **pre-ordered element chains** (`BuildChain() (*Chain, error)`);
the compositor adds the elements, links the chain, and links the last element's `src` pad to
`compositor.RequestPadSimple("sink_%u")`. This matches the go-gst `requestpad` example.

**Critical gotcha (found the hard way):** on GStreamer 1.28, `gst_bin_add` **breaks pad links
made before insertion** (the bin's pad-management re-links the element's pads and silently
unlinks the first hop). Symptom: NULL→PLAYING fails with
`streaming stopped, reason not-linked (-1)` on the layer's first source, while every
`gst_pad_link_prepare` in the log succeeds. Verified with a standalone probe: link-before-add →
`src` pad unlinked; add-before-link → PLAYING. The fix is structural: `BuildChain` returns
**unlinked** elements, and `Chain.LinkChain()` is invoked only _after_ every element has been
`bin.Add`-ed to the pipeline (`internal/layer/layer.go`, `internal/compositor/layers.go`).

Other binding/GStreamer-1.28 specifics that shaped the code:

- **`gst.Init()` is mandatory before `gst_parse_launch`** — wrapped in `gstutil.EnsureInit()`.
- **`gst_parse_launch` rejects `#` comment lines** (the `gst-launch` tool strips them, the C
  function does not) — `gstutil.FromLaunch` strips full-line `#` comments first.
- **A compositor with no sources can never preroll to PAUSED** (it waits for its first
  `sink_%u` pad). `New()` therefore leaves the pipeline in NULL; `Start()` does the single
  NULL→PLAYING transition. Layers attached while NULL simply come up with the pipeline.
- **`x264enc`'s `bitrate` property is a `guint`** — the go-glib GValue converter panics on
  platform-dependent Go `int`/`uint`, so `SetBitrate` writes `uint32` and `sanitizeProps`
  normalizes all numeric layer properties. (Writing `int64` produced a GLib CRITICAL on every
  500 ms balancer tick.)
- **`videotestsrc` has no `width`/`height` properties** in GStreamer 1.28.
  `textoverlay`'s `valignment`/`halignment`/`wrap-mode` are GEnums whose
  values are assigned as their numbers (the scene's `text-options` accepts
  the names `left`/`center`/`top`/… and maps them, `internal/layer/
text_overlay.go`); the `color`/`outline-color` properties are big-endian
  ARGB guints set as `uint32`.
- **A text layer renders through pango, not Go PNGs**: each text layer feeds
  an all-zero (transparent) BGRA surface into a `textoverlay`, which paints
  the text into it. The surface is an `appsrc` pushed from a Go goroutine
  (30 fps, one whole frame per buffer — `PushBuffer`'s block-on-full-queue
  is the backpressure).
    - Not an `fdsrc` + Go pipe: an anonymous pipe always yields ≤64 KiB per
      read, and the `textoverlay` faults on partial buffers ("invalid buffer
      size 65536 < 3686400") and EOSes on frame 0 — verified; no `blocksize`
      setting changes the read cap.
    - **`stream-type` must be `stream` (0)**: in GStreamer 1.28 the appsrc
      enum is stream/seekable/random-access (there is no `live`), and any
      seekable type makes `gst_base_src` perform an _initial seek_ at
      start that calls the app's `seek-data` callback; with no callback
      registered the seek fails and the pipeline start aborts with
      `StateChangeFailure` ("Failed to perform initial seek").
    - go-gst has no `Element→AppSrc` cast: build via the `gst` factory, then
      `gstapp.UnsafeAppSrcFromGlibFull(gobject.UnsafeObjectToGlibNone(el))`.
    - Layers attach while the pipeline is in `NULL` and the appsrc rejects
      pushes there, so the pusher retries with backoff until the first
      `FlowOK` and exits on `FlowFlushing` after it has started.
- **The terminal `appsink` needs `drop=true`** (set authoritatively in `compositor.checkContract`)
  (go-gst v1.4.1 has no `SetBoolProperty`; `SetObjectProperty` is the inherited setter). Without
  it a full appsink back-pressures the whole pipeline when the SRT socket stalls, freezing
  encoding — the pipeline "works" but data never reaches the receiver.
- **Text-overlay detection** uses `Element.GoValueType()` against the factory type, because the
  v1.4.1 factory metadata (`klasses`/`media-type`) comes back empty and `GetMetadataKeys()`
  crashes.

### 2.5 Byte extraction: pull loop on the terminal `appsink` (history: why the cgo callback ring deadlocked)

**Current model (matches `ctrlirl_coder`/`belacoder` verbatim):** `internal/gstpull` pulls the
encoded frames off the terminal `appsink` with **`gst_app_sink_pull_sample`** (cgo to
`libgstreamer-app`) on a **dedicated Go goroutine** (`Run`). Each iteration: pull a sample
(blocking until the producer has one), map+copy its buffer bytes, unref the sample, then hand the
bytes to the `send` callback (`srt.Client.Write`). No callback, no ring, no channel — the pull is
the drain. The pull goroutine is a normal Go citizen, so it may block on the SRT write without
ever touching a GStreamer streaming thread (the exact failure mode of the previous design, below).
The same loop tracks **stats** — frames pulled/sent/dropped, bytes, throughput kbit/s (500 ms
window), negotiated framerate (read once from the appsink's sink-pad caps), last-PTS — and a
**frame-production stall detector** (`gstpull.Puller.Stalled()`, threshold
`general.frame_stall_ms`, default 3000 ms) that the controller uses to force the encoder to its
floor when no encoded frames arrive (see §7).

**History — the designs this replaced, and why:**

The **first** extraction design kept the buffers in a fixed-size C MPSC ring guarded by
`pthread_mutex`/`pthread_cond`, and the GStreamer streaming thread ran the ring producer inside
a cgo callback (`identity` `handoff`). That layout **deadlocked under sustained load** and,
worse, made SIGTERM teardown hang. The bug and the fix:

- **Root cause — a blocking cgo callback must never touch a native blocking primitive.** The
  `handoff` callback runs on a GStreamer **streaming thread**, which through cgo is a _Go_ thread
  that the Go runtime may migrate or reclaim (a `sysmon`-driven preemption / a thread the GC
  wants). When that callback blocked on a native `pthread_mutex_lock`/`pthread_cond_wait` and the
  runtime took the OS thread from under it, the futex wakeup was **lost permanently** — the
  producer wedged inside the critical section holding the mutex, so both the consumer goroutine
  and the main-thread teardown blocked on it forever. Data dried up **and** shutdown never
  returned. (A native C pthread ring cannot be handed a Go function-pointer, so the ring
  itself was C; that is what forced the unsafe blocking callback.)
- **Interim fix — no native sync on the cgo boundary; a Go channel.** `internal/gstpull` went
  pure `go-gst`: the `identity` `handoff` callback became **strictly non-blocking** (map the
  buffer, copy the bytes, `select { case ch <- b: default: dropped++ }`), and a dedicated **drain
  goroutine** (`Run`) read the bounded channel and called `client.Write` — safe because it is a
  Go citizen, not the streaming thread. This removed the freeze and the teardown hang, and stayed
  in place until the appsink path below.
- **`handoff` callback signature (panics otherwise — historical).** go-gst v1.4.1's marshaller
  (`_goglib_goMarshal`, `gobject/v2/connect_export.go`) asserts `nCbParams == nTotalParams`.
  `handoff` emits exactly **2** params: param 0 = the emitting element as a **`*gst.ElementInstance`**
  (not the `gst.Element` interface), param 1 = the `*gst.Buffer`. The only signature that does not
  panic at emit time is `func(el *gst.ElementInstance, buf *gst.Buffer)`. A 3-arg form and a
  `*gst.Element` param-0 both panic. (No longer used — the appsink pull model takes no callback.)
- **Why the pull model over a callback/tap.** `gst_app_sink_pull_sample` is an
  _application-thread_ API — it is _designed_ to be driven from a Go goroutine, so there is no
  cgo callback on a GStreamer streaming thread at all (the whole class of deadlock above is
  structurally impossible). It is also exactly what `ctrlirl_coder`/`belacoder` do. `libgstreamer-app`
  is a build/runtime prerequisite for this (it ships with `gstreamer1.0-plugins-base`).

Verified with the appsink pull model: `go build`/`go vet` clean, `go test ./...` green (including
`internal/gstpull` E2E `videotestsrc → appsink` pull + stall-detection tests), a raw capture is a
well-formed **MPEG-TS (h264 + AAC) that decodes with 0 errors**, and **SIGTERM exits cleanly**
(no teardown hang — `pull_cgo.go` teardown order is unchanged since the ring era).

---

## 3. What is built and verified (pure-Go, no GStreamer)

All of the following build, `go vet` clean, and pass tests **without** GStreamer installed — the
Phase 1 acceptance criterion:

```
$ go test ./internal/...
ok   ctrlirl-coder/internal/balancer
ok   ctrlirl-coder/internal/config
ok   ctrlirl-coder/internal/scene
ok   ctrlirl-coder/internal/sinks/srt
ok   ctrlirl-coder/internal/controller
```

| Package                             | Contents                                                                                                                                                                                                                                                                                                                                             | Tests                                                                       |
| :---------------------------------- | :--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | :-------------------------------------------------------------------------- |
| `internal/config`                   | Nested-section INI schema (Load/Reload/Validate/BalancerParams), `[overlay]`, `[pipeline]`, `[scene]`                                                                                                                                                                                                                                                | ✅                                                                          |
| `internal/balancer`                 | `Balancer` interface; `adaptive` (faithful port of `balancer_adaptive.c`), `aimd`, `fixed`; `New(name, p)` factory; config-decoupled `Params`                                                                                                                                                                                                        | ✅ algorithm behavior incl. loss/RTT thresholds, clamping, cold-link guard  |
| `internal/scene`                    | `Scene`/`Layer`/`Canvas`; `Parse`/`Load`; `Diff`/`ApplyDiff`; pure-Go `Applier` interface                                                                                                                                                                                                                                                            | ✅ diff/apply + JSON round-trip                                             |
| `internal/controller`               | Pure-Go control loop over `sink.StatsSource` + `Liveness` (appsink `Stalled()`) + `scene.Applier`: 500 ms balancer tick with cold-link guard + stall→min-bitrate gate, `[overlay]` interval overlay loop, SIGHUP scene-diff apply, clean shutdown. Sink-agnostic (works for both `srt` and `srtla`).                                                 | ✅ driven by `MockStatsSource` + fake applier + fake liveness (4.8 s suite) |
| `internal/sinks`                    | **cgo-free base contracts shared by both sinks and their consumers:** `Stats` (+`IsValid`), `Sink` (`Write`/`Stats`/`Close`), `StatsSource` (`Stats`/`Close`), `Applier` (`SetBitrate`/`SetOverlayText`), `EncoderUnits`/`BitratePropertyValue` unit helpers, `MockStatsSource`/`MockProfile`                                                        | ✅ mock counters                                                            |
| `internal/sinks/srt` (cgo)          | `Client` (Go `srtgo` send socket + `sendRateTracker`) implementing `sink.Sink`; `mapStats` (srtgo→`sink.Stats`)                                                                                                                                                                                                                                      | ✅ mock counters, `sendRateTracker`, `mapStats`                             |
| `internal/sinks/srtla` (pure Go)    | `Sender` relays complete SRT datagrams and feedback over registered uplinks; per-uplink congestion windows, REG1/2/3 handshake + keepalives, classic + enhanced schedulers, reference wire protocol, telemetry                                                                                                                                       | ✅ handshake, selection, feedback, reload, real SRT session e2e             |
| `internal/gstutil` (gstreamer)      | `EnsureInit`, `FromLaunch` (comment stripping), `GetByName(Recurse)`, `SetStateSafe`, `Link`, `GetBytes`                                                                                                                                                                                                                                             | — (no unit tests; exercised by smoke runs)                                  |
| `internal/layer` (gstreamer)        | Base framework: `Layer` interface, `BaseLayer` base class, `Chain.LinkChain` (post-insert linking), `MakeElement`/`SurfaceChain`/`Tail` helpers, BGRA tail. Concrete types in subpackages: `layer/video` (`VideoSource`), `layer/image` (`ImageOverlay`), `layer/text` (`TextOverlay`), `layer/browser` (`BrowserOverlay`), `layer/testcode` (timestamp/frame/QR overlay) — each embeds `BaseLayer` | ✅ pure QR/render tests; GStreamer smoke build |
| `internal/compositor` (gstreamer)   | Command-goroutine `CompositorPipeline`: `AddLayer`/`RemoveLayer`/`UpdateLayerProps` (`scene.Applier`), `SetBitrate`/`SetOverlayText` (`sink.Applier`), NULL→PLAYING in `Start`, `Stop`, `checkContract` (verifies the `appsink` terminal, forces `drop=true`), `TapElement`                                                                          | — (smoke-verified)                                                          |
| `internal/gstpull` (gstreamer, cgo) | Terminal `appsink` pull loop: ` gst_app_sink_pull_sample` on a dedicated goroutine → `send` callback per frame (`Run(ctx, send)`); stats (frames/bytes/throughput/framerate/PTS) + `Stalled()` frame-production detector. cgo to `libgstreamer-app`. See §2.5.                                                                                       | ✅ E2E `videotestsrc→appsink` pull + stall detection                        |

The `MockStatsSource` reproduces the cumulative-counter semantics of the real `srtgo` socket's
SRT counters, so the balancer and controller can be driven in CI without GStreamer (or a live SRT
peer).

---

## 4. Deviations & documented differences

1. **SRT via a Go-owned socket + terminal `appsink`, not GStreamer `srtsink`** — matches the
   original CtrlIRLCoder/belacoder design **verbatim** (they drain an `appsink` with
   `gst_app_sink_pull_sample` → `srt_send`) and re-confirmed by the operator (§2.2); supersedes
   the interim `srtsink` pivot and the interim `identity`+`handoff` stand-in recorded in the
   change log.
2. **Terminal is `appsink name=appsink` (mandatory)** — `libgstreamer-app` (dev + runtime) is a
   build/runtime prerequisite, matching the original tool (§2.2). `contract.go` verifies the
   element type and forces `drop=true`.
3. **Encoder property is `bitrate` (kbit/s)** — this is the _correct_ `x264enc` property and what
   the spec's §2 contract table says. Note the **original C code had a latent bug**: it called
   `g_object_set(el, "bps", …)` (see `ctrlirl_coder-orig/src/gst/encoder_control.c:53`), which is a
   no-op on `x264enc` (that element has no `bps` property), so the original's "adaptive bitrate"
   never actually applied in x264enc pipelines. This rewrite sets the real `bitrate` property
   (scaled to kbps for `venc_kbps`, raw for `venc_bps`), so adaptive bitrate genuinely works.
   Recorded here and in the README.
4. **`Stats.SendBufferBytes` is now LIVE** (was "always 0" under `srtsink`) — the Go-owned socket
   reads `ByteSndBuf` (≙ `SRTO_SNDDATA`), so the adaptive balancer's buffer-pressure term is
   active again (§2.3). The pure-Go `MockStatsSource` can still set it for algorithm tests.
5. **Layers are element chains, not `*gst.Bin`** — go-gst v1.4.1 has no ghost-pad API (§2.4).
6. **go-gst import paths are `pkg/gst`/`pkg/gstapp`** — v1.4.1 module restructure vs. the classic
   paths assumed in early drafts.
7. **SRTLA wire format follows the reference, not spec §14.3.1's sketch.** The spec's
   §14.3.1 sketches an 8-byte `SRTLAHeader{Type,Length,Seq}` prepended to every SRT packet and
   REG types `0x9300/0x9400/0x9500`. The reference implementation (`irlserver/srtla`, forked from
   BELABOX; `src/common.h` + `src/sender.cpp`) does **neither**:
    - **Data packets are bare SRT datagrams** — no SRTLA wrapper. The receiver reorders using the
      SRT sequence number (first 4 bytes, big-endian, bit 31 clear).
    - Control planes share SRT's 2-byte big-endian `type` tag: SRTLA keepalive `0x9000`, SRTLA ACK
      `0x9100`, REG1 `0x9200`, REG2 `0x9201`, REG3 `0x9202`, REG_ERR `0x9210`, REG_NGP `0x9211`,
      REG_NAK `0x9212`.
    - REG1/REG2 carry a 256-byte group ID (length 258); REG3 is the 2-byte type only. The REG2
      ID from the receiver is authoritative (the sender adopts it and re-sends REG2 on every
      uplink).
    - Keepalive: legacy = 2-byte type; extended = 38 bytes (magic `0xC01F` @10, version @12,
      24-byte `connection_info_t` @14).

    `internal/sinks/srtla/protocol.go` implements the reference layout verbatim. This is a spec
    revision; per §2 the README gains SRTLA operator docs when the sender lands (Phase 4).

8. **Shared `internal/sinks` base package keeps everything cgo-layered.** Both output
   paths live under `internal/sinks/`: `internal/sinks/srt` (cgo, wraps
   `github.com/haivision/srtgo`) and `internal/sinks/srtla` (pure Go, no cgo). The base
   `internal/sinks` package is **cgo-free** and defines the contracts both sinks and the
   consumers use: `Stats` (the balancer telemetry shape), `Sink` (`Write`/`Stats`/`Close`, the
   data-path writer), `StatsSource` (`Stats`/`Close`) and `Applier` (`SetBitrate`/
   `SetOverlayText`). `internal/sinks/srt`'s `Client` and the command package's SRTLA adapter
   implement `sink.Sink`; the pure-Go SRTLA sender relays datagrams and reports its own counters.
   The adapter combines SRT session statistics and SRTLA packet/ACK counters. `internal/config`,
   `internal/balancer` and `internal/controller` import only `sink` (never the cgo `srt` package),
   so they stay `CGO_ENABLED=0`-buildable; only `cmd/ctrlirl-coder` (which constructs the chosen
   sink) and `internal/sinks/srt` themselves pull in cgo.
9. **SRTLA uses an internal ephemeral loopback relay.** Spec §14 describes a configurable local
   listener; the application instead binds `127.0.0.1:0` itself and connects the existing SRT
   client to it. The relay fans intact SRT datagrams out over registered uplinks and returns
   receiver feedback to the client. SRT negotiates the remote session and performs retransmission;
   fabricating data packets without a handshake cannot produce a playable stream. No external
   process or configured `listen_addr` is required. `[srt]` latency, stream ID, passphrase and
   bandwidth cap apply in SRTLA mode, while `[srt].uri` does not.
10. **Native SRT library is the CERALIVE fork, not stock Haivision SRT.** The
    `haivision/srtgo` Go module is still the cgo wrapper (its API is unchanged), but the native
    `libsrt.so.1.5` it links is now the [CERALIVE fork](https://github.com/CERALIVE/srt) (a SRT
    1.5.7 fork, GnuTLS backend), built from a pinned source commit by
    `scripts/build-ceralive-srt.sh` into a private bundle. This changes the packaging model and
    the "pure-Go / no libsrt" assumption recorded in earlier phases:
    - **`internal/sinks/srt` is cgo** (it was mislabelled pure-Go in Phase 1): the `srtgo` wrapper
      links `-lsrt`, so building/testing it needs the CERALIVE bundle, not GStreamer alone.
    - **No stock `libsrt` apt package is installed.** CERALIVE and stock SRT share the
      `libsrt.so.1.5` SONAME, so a mis-linked binary would silently load the system library. We
      ship the fork's `libsrt.so.1.5` **next to the binary**, set a `$ORIGIN` rpath, and embed the
      bundle's library **SHA-256** into `internal/sinks/srt` (ldflags `-X ceraliveLibSHA256/…`).
      `srt.EnsureSRTBackend()` (called from `cmd/ctrlirl-coder` before the data path starts)
      re-hashes the mapped `libsrt` at runtime and **refuses to stream** if it does not match the
      embedded identity — a bare `go build` (empty identity) cannot start the SRT path.
    - **Build entry points** `make dist-amd64`, `make test`, `make vet`, the Dockerfile and the
      CI gstreamer/cross-arm64 tiers all build the bundle first and attach the same cgo flags +
      identity. The Dockerfile builds the fork in-band (amd64 + arm64 via buildx) and ships the
      lib + `build.json`; it no longer installs `libsrt-dev`/`libsrt1.5`.
    - **GStreamer's SRT _reception_ plugin** (`libgstsrt.so`, used only by local test
      listeners/`srtsrc`) still resolves the stock GnuTLS library; that is a receiver-side,
      test-only concern and is unaffected by the sender-side fork.
    - The fork adds receiver-side, default-off `SRTO_*` options (lapatches/periodic-nak-gate/
      reorder-freeze). None are enabled by this codebase, so **sender behaviour is unchanged**.
11. **The shared API server is plaintext HTTP, not HTTPS.** The control +
    metrics listener (`internal/server`) used to be a single TLS listener
    (self-signed cert in memory, or `[control] cert_file`/`key_file` PEM —
    clients needed `curl -k`). It is now plain HTTP: the `Config.CertFile`/
    `KeyFile` fields, the self-signed cert generation, and the cert config
    keys are gone. Rationale: the server is intended for a trusted local/
    management network (loopback by default) and the self-signed TLS added
    only friction (browser warnings, `curl -k`) with no real protection on a
    loopback address. The JSON-RPC control plane is not an authentication
    boundary, so TLS was providing assurance without access control.
12. **`/metrics` is Prometheus text exposition format + carries encoder &
    SRTLA metrics.** Previously the endpoint emitted plain `name value`
    lines (gauge-only, no types). It now emits `# TYPE <name> <type>` +
    `name value` (gauge/counter), scrapeable by Prometheus. The gather
    surface grew beyond the sink/puller stats:
    - **Encoder**: `encoder_kbps` (measured), `encoder_target_kbps` (the
      balancer's current target), `encoder_min_kbps`/`encoder_max_kbps`,
      `encoder_rtt_min_ms`/`encoder_rtt_max_ms` (rolling).
    - **SRTLA**: `srtla_uplinks_total`/`_active`, `srtla_connected`,
      `srtla_failover`, `srtla_mode_enhanced`, and per-uplink
      `srtla_uplink_<n>_{rtt_ms,inflight,window,weight,up,nak_burst,last_activity_ms}`
      via the new `srtla.Sender.Detailed()` / `srtla.DetailedProvider`.
    - Counters (`frames`, `frames_dropped`, `bytes_sent`, `sink_pkts_total`)
      are typed `counter`; the rest are `gauge`.
13. **`testcode` layer and `testsrc://` are test-harness extensions.** The
    test-only `testcode` layer renders a transparent UTC timestamp/frame
    overlay with a 1 Hz marker and a byte-mode QR code. The video layer also
    accepts `testsrc://red?freq=440` and `testsrc://blue?freq=660` to create
    solid-color test video with a sine tone. These extend the scene types and
    video source syntax beyond the production spec; they exist for live
    video/audio verification, not general media-source configuration.
- **Scene edits are one critical section** (spec: "each change applied
      atomically with rollback"). `Controller.modify` — the path behind
      every `*.set` RPC method — takes `c.mu` and holds it across
      clone → tx → parse → diff → apply → commit. Without this, two
      back-to-back edits (e.g. the test script's paired `layer.set`
      calls per switch) interleaved: the second edit cloned the scene
      *before* the first committed, and its commit — or the stale
      alphas its ops re-applied — desynchronized the tracked scene and
      the live pipeline (live-repro: spurious re-mute of the just-shown
      layer ~3 ms after each switch, black + silent windows while the
      compositor kept running). Multi-op edits still bracket their ops
      with `SuspendStream`/`ResumeStream`, in-lock (applier ops are
      short; a stuck op now blocks other RPCs — accepted trade for a
      consistent scene state). Load/Switch were already atomic.

---

## 5. What remains (next work)

Done in this round:

1. ~~**`internal/sinks` unit test**~~ ✅ `internal/sinks/mock_test.go` (cumulative counters + `IsValid`).
2. ~~**`internal/layer`**~~ ✅ element chains with BGRA output caps (see §2.4 for the post-insert
   linking requirement).
3. ~~**`internal/compositor`**~~ ✅ command-goroutine pipeline with all `scene.Applier` +
   `sink.Applier` methods; a `sink.StatsSource` wired as the stats source.
4. ~~**`internal/controller`**~~ ✅ pure-Go control loop, unit-tested.
5. ~~**`cmd/ctrlirl-coder/main.go`**~~ ✅ flags `--config/--scene/--pipeline/--dry-run`; SIGHUP
   reload, SIGINT/TERM graceful stop; gstreamer wiring behind the build tag.

Also done this round:

- **`README.md`** ✅ — contract table, scene format, config reference, the two recorded spec
  revisions (SRT via `srtsink`, `venc_kbps` naming + the original's latent `bps` bug), local +
  Docker run instructions.
- **`Makefile`** ✅ — `build` / `build-gst` / `test` / `test-gst` / `vet` / `test-integration` /
  `docker` / `clean`.
- **`Dockerfile`** ✅ — golang build stage (GStreamer dev + `plugins-bad` + CERALIVE build deps
  `cmake`/`g++`/`libgnutls28-dev`) builds the CERALIVE SRT fork in-band (`scripts/build-ceralive-srt.sh`,
  amd64/arm64 via `TARGETARCH`) → links the binary against it with the lib SHA-256 embedded → slim
  runtime carrying the bundled `libsrt.so.1.5` + GnuTLS + the examples (no stock `libsrt`).
- **`scripts/integration-smoke.sh`** ✅ — local SRT listener + both pipeline templates; asserts
  `pipeline playing` and zero CRITICAL/ERROR. **Passes locally.**
- **`.github/workflows/ci.yml`** ✅ — pure-Go job (no GStreamer) + gstreamer-tier job
  (build/vet/test + integration smoke with GStreamer installed).

Still open:

1. ~~**Hardening (spec §6 Phase 6)**~~ ✅ — /metrics endpoint (loopback-only), GStreamer
   bus-message draining to slog, and structured `sink stats` logging (5 s cadence) are in.
   `RemoveLayer` under a playing pipeline is covered by the existing compositor tests
   (`TestRemoveLayer…`, race-clean).
2. **TypeScript bindings (spec §13)** — separate workstream (`@ctrlirl/ctrlirl-coder`);
   **explicitly deferred** until the Go core is complete.
3. **SRTLA sender (spec §14)** — protocol, config, TS schema, classic + enhanced schedulers,
   and telemetry are implemented. The command package wraps the SRT client and a private
   loopback relay as a `sink.Sink`; `internal/sinks/srtla` stays cgo-free and relays complete
   SRT datagrams plus receiver feedback. The SRT client owns session negotiation and
   retransmission. Real-SRT integration coverage verifies one uplink, two bonded uplinks,
   failed and successful reloads, and an installed reference `srtla_rec` (the latter skips
   when unavailable). Raw datagram tests alone verify wire compatibility, not a playable SRT
   session. `[srtla]` SIGHUP reload prepares replacement uplinks before closing old ones;
   operator instructions live in README.md ("SRT link aggregation").

---

## 6. Environment & verification status

- **Toolchain:** Go 1.26.8.
- **GStreamer runtime:** 1.28.2 with `srtsink`/`srtsrc`, `textoverlay`, `compositor`, `x264enc`,
  `image-decode`, `v4l2src` (no `videotext`).
- **GStreamer/glib _dev_ headers:** installed — the cgo GStreamer packages **compile and
  run locally** (earlier note about missing headers is obsolete).
- **Verification done locally (this environment):** - `go build` / `go vet` / `go test -count=1` — green for both the pure-Go tier and
  `-tags gstreamer`. - **Full pipeline MWE** (`pipeline/example-full.pipeline`): NULL→PLAYING, SRT data received by
  a local `srtsrc` listener, 0 CRITICALs, graceful SIGINT shutdown. - **Layered pipeline** (`pipeline/example-layered.pipeline` + 2-layer smoke scene): both layers
  attach to `sink_0`/`sink_1`, NULL→PLAYING succeeds, H.264 stream decoded cleanly by a
  `srtsrc ! decodebin` listener (≈6.6 MB captured in 8 s), 0 CRITICALs. - **No-freeze + clean-shutdown regression** (`pipeline/example-full.pipeline`, 30 s soak):
  on the appsink pull path (§2.5), `bytes_sent` climbs continuously for the full
  30 s (no stall), `loss 0%` / `rtt < 1 ms`, and **SIGTERM exits in ~0.1 s**
  (no teardown hang). `internal/gstpull` unit/E2E tests (pull count, throughput, framerate,
  `Stalled()`) green. - **Parity vs the original `./ctrlirl_coder`:** both emit **MPEG-TS (H.264 1080p + AAC) over SRT**;
  the Go version's decode has **0 errors** (the reference has a single tail decode artifact
  from the abrupt cutoff); steady-state trailing bitrate is in the same class (reference
  ≈526 kbps, Go ≈700 kbps over a 15 s window, both converging toward the 500 kbps floor).
  The Go binary uses the _same_ template shape the operator provided (`smpte → x264enc
(6000k) → h264parse → voaacenc → mpegtsmux → identity → fakesink`). - GStreamer 1.28's `srtsrc` takes `uri="srt://:9101?mode=listener"` — there is no `port`
  property in this version.

---

## 7. Change log

- **2026-11-26 (browser test: 1080p30 default):** A 1080p60 stream into a
  30fps OBS project looks hickup-y (OBS 2:1-decimates the source). New
  `pipeline/example-layered-1080p30.pipeline` twin (caps 30/1,
  key-int-max 30); `scripts/test-obs-browser.ts` now defaults to it (OBS
  project at Settings → Video → 30). E2E verified: h264 1920x1080 30/1,
  PTS mean 33.3ms stdev 0.01ms.

- **2026-11-26 (browser layer: content-driven push cadence — smooth JS
  animation):** User symptom: browser-layer JavaScript animation (a line
  bouncing left/right at 8 px/frame) looked choppy. Root cause: the push
  phase ran a fixed 1/fps ticker while the CDP screencast frames arrived on
  the page's own ~60 Hz raster clock — two ~60 Hz clocks beating against
  each other. The ticker sometimes re-pushed the previous frame between two
  raster frames, so one content step spanned two ticks and the next spanned
  two frames: a 0-px-then-2x stall-then-teleport beat. Measured via the
  line-bounce probe (`browser_smooth_probe_test.go`) through the production
  chain: 3.6% anomalous displacements, 0.9% beat pairs (16 px/frame mean
  8, sd 2.3). Every upstream stage was exonerated first: page rAF = 60.0
  fps with zero >17 ms gaps (CDP probe in the app's live Chrome), screencast
  stream = 60.0 fps, SRT stream PTS = 60.0 fps stdev 0.0, per-block motion
  uniform across the full 1920x1080 canvas (no compositor damage clipping).
  Fix (`browser.go`): push each new frame on arrival (`latestFrame.fresh`
  coalescing signal, cap 1); the ticker is demoted to keepalive — it only
  re-pushes when no fresh push landed within the interval (static page,
  where re-pushing identical content is required to keep the stream alive,
  and content-invisible on an animated page). Result: 0.0–0.7% anomalies,
  0% beat pairs, displacement sd 0.0–0.43, mean 8.0 px. The regression gate
  in `TestBrowserLineBounceSmoothness` was tightened to match (anomPct <
  2.5, beatPct < 0.5; the old 30%/20% ceilings let the beat pass).
  Capture format stays PNG: benchmarks (1080p60 animated, this host) —
  screencast png 60.0 fps/88 KB, jpeg 60.0 fps/326 KB **but no alpha
  (transparent page regions come back black — breaks compositor
  alpha)**, webp 19 KB but capped at ~30 fps; CDP has no raw-BGRA frame
  channel at all, so base64-over-JSON is the wire format in every format.

- **2026-11-26 (GLib `g_object_unref` CRITICAL at startup):** The live-context
  helper `gstutil_make_live` ended with `g_object_unref(ctx)` on a
  `GstContext`. On GStreamer 1.28 `GstContext` is a `GstMiniObject`
  (`is-object-subtype=0`), so the call trips GLib's
  `g_object_unref: assertion 'G_IS_OBJECT (object)' failed` CRITICAL at every
  startup (it fires right after the appsink puller attaches). `gst_object_unref`
  trips it in this build too. Fix: drop the unref entirely —
  `gst_element_set_context` takes its own reference, the element owns the last
  one and frees the context with the pipeline; no leak. Verified with a C
  repro (set_context + no unref + unref element = zero criticals), then with
  the full cgo binary: `scripts/test-obs-browser.ts` log scan now passes.

- **2026-11-24 (appsrc double-free abort):** Fixed
  `GStreamer-App:ERROR:gstappsrc.c:1803:gst_app_src_create: code should not
  be reached` (freed object popped from the appsrc queue).
  `gst_app_src_push_buffer` is transfer-full on *every* flow result — it
  unrefs the buffer itself on FLUSHING/EOS/dropped. Two pushers violated it:
  the testcode feeder unrefed after every push (double-free each frame), and
  `browser_push_data` unrefed after any non-OK push (double-free on every
  FLUSHING push, i.e. each scene switch since feeders ride out FLUSHING).
  Both now allocate + fill + push in a single C call and never touch the
  buffer afterwards (no go-gst `Buffer` wrapper/finalizer involved).
  Regression tests `TestPushAppSrcFeedSurvivesFlushingAndStateCycles` and
  `TestPushBufferSurvivesFlushingAndStateCycles` cycle PLAYING/READY with
  200 FLUSHING pushes per cycle; re-injecting the old unref aborts them
  (`gst_mini_object_unref: assertion ... > 0`, SIGABRT). The interactive
  SRTLA harness now treats a signal exit as failure. Verified: `make vet`,
  `make test`, `make dist-amd64`, and the full harness against a local
  `srtla_rec` → `srt-live-transmit` chain with three scene switches — all
  four checks pass; the received TS decodes as H.264+AAC and every sampled
  frame's QR decodes (`CTRLIRL|<UTC>|f=N`, monotonic across switches).

- **Testcode visual/audio harness:** Added a transparent timestamp + frame
  counter + QR overlay and test-only red/blue video source URIs with
  configurable sine tones. The interactive SRTLA OBS test now switches
  between one video/browser scene per color/tone pair. Unit tests cover
  QR payload/version boundaries, UTC formatting, clipping, and source URI
  validation; appsrc chain construction is also tested. `make test`,
  `make dist-amd64`, and binary dry-run passed; live streaming verified via
  a local SRTLA receiver chain (see appsrc double-free entry above).

- **2026-11-23 (Scene edit race — black/silent switch windows):** Fixed a
  race in `Controller.modify` (the path behind every `*.set` RPC:
  `layer.set`, `filter.set`, …). It cloned the scene under the lock but
  only committed it afterwards, with the diff/apply/commit all outside
  `c.mu`. Two back-to-back edits (the switch test fires a paired
  `layer.set` per switch) interleaved: the second edit diffed against the
  pre-first-commit state, so its ops re-applied the first edit's stale
  alphas and/or its commit rolled the tracked scene back — the live
  pipeline and the scene state diverged. Live repro (SRTLA test, 8
  switches): spurious re-mute of the just-shown layer ~3 ms after two of
  the switches, with 22 s black + fully silent windows in the captured
  stream (video rects blank, audio at digital silence) while the
  compositor kept running at 30 fps. `modify` is now one locked critical
  section from clone to commit; `Load`/`Switch` were already atomic.
  Regression test `TestModifyConcurrentNoLostEdit` pins the contract: a
  concurrent editor blocked mid-apply must not lose the first edit's
  alpha in the tracked state.
- **2026-11-23 (Browser layer survives scene freezes):** Fix for the browser
  layer going blank after a scene switch. The feeder
  (`runBrowser`/`pushFrame` in `internal/layer/browser`) exited when
  `gst_app_src_push_buffer` answered `GST_FLOW_FLUSHING` — exactly what a
  scene switch triggers, since the switch holds the whole pipeline at READY
  for the op batch and a layer with the same id in both scenes is kept (no
  chain rebuild). No feeder after resume = blank layer until process
  restart. The feeder now rides out FLUSHING: 200 ms backoff + re-push of
  the same captured frame until the appsrc accepts; it exits only on
  explicit stop, context cancel, or Chrome exit. Worst case after a freeze
  is one stale frame, never a dead stream. Regression-tested with a
  READY-state appsrc (`TestPushFrameSurvivesFlushFreeze`).
- **2026-11-23 (HTTP API + Prometheus metrics):** The shared API server
  (`internal/server`) is now plaintext HTTP — TLS, the self-signed cert
  generation, and the `[control] cert_file`/`key_file` config keys are
  removed (deviations 11–12). `/metrics` now emits the Prometheus text
  exposition format (`# TYPE` + `name value`, gauge/counter) and the gather
  surface grew: encoder metrics (`encoder_kbps` measured,
  `encoder_target_kbps` from the controller, `encoder_min_kbps`/
  `encoder_max_kbps`, `encoder_rtt_min_ms`/`encoder_rtt_max_ms` via the new
  `controller.Controller.RTTRange`) and SRTLA metrics (`srtla_uplinks_total`/
  `_active`, `srtla_connected`, `srtla_failover`, `srtla_mode_enhanced`,
  per-uplink `srtla_uplink_<n>_*`) via the new `srtla.Sender.Detailed()` /
  `srtla.DetailedProvider` (the `srtlaSink` delegates to it). The controller
  is now built before the API so the metrics closure can read its state.
  Counters are typed `counter`; everything else `gauge`. Updated
  `AGENTS.md`, `README.md`, `RPC.md`, `SPECS.md` §15, `config.toml.example`,
  and the test scripts (`curl -s http://`, no `rejectUnauthorized`).
- **2026-11-23 (video layer source audio):** Video layers now stream the
  source's audio track, not just its video. Previously `Chain.LinkChain`
  linked only the first **video** pad of a decodebin-style dynamic source,
  so the audio pad of an HLS/SRT/file source was never connected and the
  stream went out audio-less (only browser layers carried page audio).
  `VideoSource` now implements `AudioChain()` for media sources (SRT,
  HTTP/HTTPS, local file): a second chain of the same source (audio pad) →
  `audioconvert` → `queue`, mixed into the shared
  `audiomixer` → `voaacenc` → `aacparse` → `mpegtsmux` tail exactly like
  browser audio (still one audio program total — MPEG-TS demuxers play the
  first program only). `Chain` gained `DynamicPadAudio`: the dynamic source
  hooks its first **audio** pad instead of its first video pad. Audio-chain
  elements use an `aud` name infix (`<id>-aud<factory>`) so they cannot
  collide with the video chain's element names. Test pattern and `/dev/`
  capture layers stay video-only. Audio **filters** (volume/equalizer/…)
  remain browser-only at the scene level — video-layer audio is unfiltered,
  and a hidden video layer (alpha 0) does not mute its audio (the in-place
  mute rides the layer's `volume` filter, which video layers cannot have).
  Verified end-to-end: mux.dev HLS (audio + video) through
  `example-layered.pipeline` to an SRT listener captures h264 **+ aac**.
- **2026-11-24 (shared source element for video-layer audio):** The
  previous day's source-audio feature built a **second** fetcher — an
  `auduridecodebin3` for HTTP/HTTPS (a second `audsrtsrc` for SRT, second
  `audfilesrc`+`auddecodebin` for file) — so a live HLS/SRT layer fetched
  the stream twice, each fetcher jitter-buffering and resyncing on its own.
  The video and audio running times then drifted without bound: audible
  audio glitches, encoder artifacts at the same moments, and audio running
  ahead of video (reported as "sound plays before video comes").
  `VideoSource` now keeps the video chain's `uridecodebin3` in a
  `sharedSource` field; `AudioChain()` builds only
  `audioconvert → queue` and hooks the decodebin's **audio_0** pad onto it.
  `Chain` gained `SharedSource gst.Element`: `LinkChain` links the static
  tail, then registers a pad-added handler on the shared source that links
  its first matching pad (audio for audio chains) onto the tail head.
  Both chains of a layer LinkChain in one serialized AddLayer command
  before any state change, so the handler always sees the runtime pad.
  One network stream, one running time — A/V cannot drift. A shared
  `uridecodebin3` is verified present in the live pipeline (one
  `GstURIDecodeBin3` instance instead of two). `TestSharedSourceChain`
  covers the hooking in the compositor's real link order.
  SRT and file sources keep their duplicate fetchers for now: the SRT
  chain carries no decoder (elementary-stream path) and `srtsrc`'s
  `src_%u` pads are request-time, not runtime-added, so the pad-added
  hook needs a `RequestPadSimple` fallback — follow-up.
- **2026-11-24 (live-HLS 10 s glitch → C-level batched SRT send +
  non-silence-filling audio mix):**
  Users report a sound glitch + video artifact every ~10 s on HLS layers
  (source `https://test-streams.mux.dev/x36xhzz/x36xhzz.m3u8`). Two
  **distinct** causes — separated by 130 s SRT captures + per-100 ms RMS
  analysis:

  1. **Chronic appsink drop (9.4 % → ~0 %).** The SRT send path made one cgo
     call per 188-byte TS packet (~40 000 calls/s at 720p30 / 6 Mbps). Each
     cgo crossing costs ~234 μs — impossible to keep up. The appsink filled
     and dropped 9.4 % of packets continuously. Fix in two stages:
       - First: batch 7 TS packets (1316 bytes = SRT_MAXSRTPAYLOAD) into one
         cgo send call (`internal/gstpull/pull.go`, `srtBatchSize`) → 9.4 % →
         2.7 %.
       - Then (this change): a **C-level batched send** —
         `internal/sinks/srt/sendbatch_cgo.go` — that takes a whole 32 KB
         batch in **one** cgo crossing, splits it into 1315-byte live-safe
         `srt_sendmsg2` chunks in C, and waits on `SRT_EPOLL_OUT` inside C
         (120 ms per-attempt / 400 ms total, mirroring the poll budget).
         `Client.Write` calls `cgoSendBatch` once per batch. Drop rate fell
         2.7 % → **~0 %** (the drop counter freezes at the startup
         preroll burst and never climbs again). `srtBatchSize` is now
         32 KB. srtgo keeps socket lifecycle + stats; only the hot send path
         is raw cgo.
  2. **~0.395 s audio silence at each 10 s segment boundary.** The source
     HLS is VOD (`EXT-X-PLAYLIST-TYPE:VOD`, `EXT-X-ENDLIST`, **no**
     `#EXT-X-DISCONTINUITY` tags). Decoding it in full with ffmpeg *or* a
     full GStreamer pre-buffered decode produces **no** such silence, so the
     hole is not in the content. In the **live** pipeline each 10 s TS
     segment (MPEG-TS continuity counter resets per segment) makes
     `hlsdemux2` resync; the audio PTS is offset by ~0.395 s at the splice
     and the mixer bridges that window with **digital silence** (the muxed
     TS has continuous PTS but clean 0.0 % audio there). Video stays
     continuous (no freeze).
     - Root of the silence fill: the shared audio mixer is `audiomixer`,
       which fills any input timestamp gap with silence. `audiomixer` →
       `liveadder` (same `sink_%u` request-pad mix model, built for live
       pipelines, sums inputs and passes gaps through) — `ensureAudioTail`
       in `internal/compositor/layers.go`.
     - The pipeline is marked **live** before it plays
       (`gstutil.MakeLive` in `compositor.Start`). For a real-time encoder
       this is required even for a VOD-flagged source (pace to the wall
       clock, don't play the file ahead) and it lets the audio queue treat
       the splice as a bounded resync.
     - A/B captures (130 s, count of 0.0 % 100 ms buckets at the 10 s
       phase): `audiomixer` ~26–32 → **`liveadder` + live = 11** (best).
       Tried and **reverted** (each worse, 26–32): `audioresample` in the
       per-layer audio chain, `audiorate` before the mixer, `audiorate`
       after the mixer, NOT marking the pipeline live, and a 2 s audio
       time-queue. None recovered the bridged samples — GStreamer 1.28 has
       no `tsdemux discontinuous` mode and no way to suppress the live
       demux resync, so the resampled/bridged 0.395 s cannot be
       reinvented downstream.
     - `internal/layer/video/video.go` `AudioChain()` (shared source) is
       `[audioconvert, queue]`: `liveadder` passes the samples through and
       the queue keeps the splice a bounded jitter. Test:
       `TestSharedSourceChain` / `TestVideoAudioChains`
       (`wantLen = 2`).

  **Residual:** the ~0 % drop is fully fixed (C-level batched send). The
  ~0.395 s audio-only silence at *some* 10 s boundaries remains — a
  GStreamer 1.28 live-resync limitation. `liveadder` + live halves the
  affected boundaries vs `audiomixer` (11 vs ~26–32 of 13); each is a brief
  audio-only dip, not the pre-fix 9.4 % chronic drop. The muxer, encoder,
  and SRT send path are unaffected.
- **2026-11-25 (FullHD pipeline template):** Added
  `pipeline/example-layered-1080p60.pipeline` — the layered-compositor
  template with caps pinned to 1920×1080 @ 60/1 (same contract elements as
  `example-layered.pipeline`, whose 1280×720 @ 30/1 pin squashed FullHD
  scenes into a 720p block). `scripts/test-obs-browser.ts` now defaults to
  it; the 720p template keeps serving the 720p scene fixtures. Both
  templates carry cross-reference comments about the pin-must-match-scene
  contract.
- **2026-11-25 (Browser layer honors width/height/framerate; smooth 60fps
  cadence):** Two browser-layer fixes.
  **Props:** `chainRebuild` (`internal/scene/scene.go`) now also reports a
  rebuild for browser layers whose `width`/`height`/`framerate` change.
  Those define the *capture* resolution and the appsrc caps framerate, both
  fixed at chain build (BuildChain/parseConfig); `UpdateLayerProps`
  (compositor pad box, x/y/zorder/alpha) scales the capture only and could
  never update them in place, so the chain is removed + re-added on change
  (same path as type/source/filters). x/y/zorder/alpha stay in-place.
  Tests: `TestDiffBrowserChainPropsRebuild` /
  `TestDiffBrowserPadPropsInPlace`.
  **60fps smoothness:** the browser feeder is now two-phase
  (`runBrowser` in `internal/layer/browser/browser.go`). Chrome 156
  headless has a ~50ms per-capture floor (verified by dedicated probes:
  unchanged by `--disable-gpu`/swiftshader stripping,
  `--enable-begin-frame-control`, `optimizeForSpeed`, or JPEG;
  `Page.startScreencast` is dead in this build). The old inline
  capture-then-push loop pinned at 60fps therefore stalled at ~20fps with
  50ms+ gaps. Now a capture goroutine screenshots at up to `1/fps` and
  keeps only the latest frame (`latestFrame`), and a push ticker runs at
  exactly `1/fps`, always pushing the freshest capture. The OUTPUT cadence
  is the target framerate; content updates at whatever rate the host's
  Chrome manages (a stale frame repeats to fill the gaps — no temporal
  interpolation is possible for DOM content). At 30fps the capture loop
  simply paces itself. Regression test `TestBrowser60FPSFeedSmooth`: real
  Chrome + rAF 60Hz page through the production chain → fakesink, asserts
  ≥80% of 60fps and no gap >1s — 60.0fps, max gap 17.6ms.
- **2026-11-25 (Browser layer render backend + smoothness regression):**
  The 60fps *feed* was fixed above, but the *content* could still be choppy:
  a bouncing-line pixel probe (`scripts/line-bounce.html` — a magenta line at a
  constant 8px/frame + a 20-cell render counter) can show a 0/16
  stall-then-teleport beat when the page's rasterizer is software-capped well
  below the display refresh. Chrome was launched with
  `--disable-gpu --enable-unsafe-swiftshader`, so `requestAnimationFrame` was
  driven by SwiftShader even when a real GPU (NVIDIA RTX 5090) was present.
  - **Auto render-backend select** (`renderBackendFlags`/`hasGPU` in
    `internal/layer/browser/chrome.go`): probe `glxinfo -B` for a non-software
    renderer (excluding `llvmpipe`/`swrast`/`Mesa`) and, when one exists,
    launch with `--enable-gpu-rasterization --enable-zero-copy`; otherwise fall
    back to SwiftShader. Overridable per host: `CTRLIRL_CHROME_GPU=0|1`
    (force backend) or `CTRLIRL_CHROME_GPU_ARGS="a;b;c"` (replace the whole
    backend flag set). The rAF cadence is locked to the display refresh
    (59.88 Hz here), so a display-locked raster is smooth.
  - **Startup log** (`layer: browser render backend`): logs `renderflags`
    (the backend decision) + a supplementary `webgl` string. The WebGL device
    string is *not* the raster backend — in headless Chrome it reports
    ANGLE/SwiftShader-Vulkan even with GPU raster active (WebGL routing is
    separate from the compositor rasterizer) — so treat `renderflags` as the
    signal. (`glRenderer` is a `Runtime.evaluate` probe; `SystemInfo.queryWebGL`
    is absent from headless Chrome.)
  - **Smoothness regression** (`TestBrowserLineBounceSmoothness`,
    `browser_smooth_probe_test.go`): feeds the bouncing-line page through the
    production chain → fakesink, decodes each BGRA frame, and measures (1) the
    *browser* render rate (20-cell rollover counter, independent of the
    capture rate — the stable "is the animation smooth" signal), (2) per-frame
    line displacement (freezes/teleports/beat-pairs), (3) PTS cadence.
    PRIMARY gate: render rate ≥ 45 fps (a display-locked raster measures
    ~57.5 on this 59.88 Hz display, every backend, every run; a software
    raster capped well below refresh measures ~30). SECONDARY: anomalies
    (|Δ|==0 or >9px) < 30% and beat-pairs (0 then >9) < 20% — a 30fps feed
    produces a *sustained* 0/16 beat at ~50%, while the 57.5→60 alias and
    occasional downstream dupe-delivery bursts stay far under that.
- **2026-11-25 (VOD HLS 1× real-time playback: byte-rate pacer +
  drop=false backpressure + branch-scoped live context):** Users report
  HLS layers that are VOD playlists (finite, `EXT-X-ENDLIST`) "stopping
  after ~4.4 s in OBS". Root cause: GStreamer 1.28 gives no live mode
  for a `uridecodebin`-fed HLS stream, so `hlsdemux2` runs VOD-style and
  prefetches the WHOLE finite playlist at network speed; the 1×-paced
  drain side could not slow that, and the un-paced (or even byte-rate
  paced at `drop=true`) drain consumed a ~30-min 60-fps master playlist
  in seconds — the SRT listener saw a stream already at EOS and OBS
  "stopped" a few seconds in.
  - **Byte-rate pacer** (`internal/gstpull/pull.go`): the mpegts terminal
    appsink buffers carry NO PTS (it is inside the TS payload), so the
    drain is paced by BYTES at the adaptive target bitrate:
    `due = anchorWall + (cumBytes-anchorBytes)*8000/rateBps`, sub-quantum
    debt carry, re-anchor (no catch-up sleep) when drift < −500 ms. Rate
    is a live supplier closure over the balancer's `CurBitrate()`
    (`SetPaceRateSupplier`, wired after Run starts). For self-pacing live
    sources the drain never outruns the source → drift ≤ 0 → no-op.
    (`TestPacerTicks` unit tests the tick math.)
  - **`drop=false` on the terminal appsink** (`compositor.go`): the pacing
    is consumer-side — `drop=true` just drops the VOD burst. With
    `drop=false` the full queue blocks the streaming thread, pausing the
    VOD-pausable source, and the 1×-paced drain keeps it fed: a finite
    playlist plays at real time. `max-buffers=600` (≈20 s at 6 Mbps)
    absorbs the CC-reset burst at segment boundaries without a drop.
  - **The "live" GST_CONTEXT is scoped to the audio-tail branch head
    only** (`liveadder`), not the pipeline root. Two measured dead ends:
    (a) whole-pipeline live made the VOD `hlsdemux2`/soup source live →
    unpauseable → it drains to EOS at network speed (the 4.4 s freeze)
    and, at `drop=false`, deadlocks at the segment boundary;
    (b) live-marking even just the terminal appsink still put the source
    branch in live mode, because a context propagates UP the pad graph to
    decide source liveness. A same-named "carve-out" context on the
    source does NOT help: context fields merge and the parent's `live`
    field wins over any child override (an empty child context has no
    fields at all) — measured on 1.28. Branch-head scope is the only
    shape that keeps the 10 s-glitch audio resync (liveadder needs live)
    while the source stays VOD-pausable. No-audio scenes attach no live
    context at all.
  - **HLS watermarks raised to 8 s / 50 ms** (`hls_watermark.go`): the
    400 ms / 50 ms clamp deadlocked at the first segment boundary under
    `drop=false` — the demux pauses at the low watermark to seek + fetch
    the next segment, the 400 ms of local data runs out in <1 s, and the
    paused pipeline can never resume (measured: frozen with
    `last_frame_age` climbing, "Buffering 0%" never completing). 8 s ≈
    one 6 s segment + headroom covers the seek+fetch window; the 50 ms
    low watermark is the prompt resume trigger.
  - Verified E2E: Apple fMP4 master.m3u8 (1080p60) streams 120 s
    continuous (was frozen at 4.4 s), 0 stalls, 0.06 % dropped batches;
    mux x36xhzz.m3u8 streams 150 s continuous, 0 stalls, A and V packet
    PTS fully continuous with matching start (1.400 s) and end
    (≈151.37 s) — no gap at any 6 s segment boundary.
- **2026-11-27 (CI simplified: test jobs + dist jobs publishing binaries, .debs
  and a static apt repo on GitHub Pages):** The workflow is now
  `pure-go` (vet+test) · `bindings` (bun test+typecheck) · `dist-amd64`
  (native build, `make vet`+`make test`, `test-stream.sh`,
  `integration-smoke.sh`, .deb) · `dist-arm64` (cross-compile, **qemu-user
  `--dry-run` smoke** — boot + CERALIVE SHA guard + contract check, since
  full pipelines are too slow under emulation, .deb) · `package-repo`
  (push to main only). Each dist job uploads `ctrlirl-coder-<arch>`
  (deb + binary + CERALIVE lib + build.json) and contributes its .deb to a
  shared `ctrlirl-coder-debs` artifact. `package-repo` indexes both .debs
  with `dpkg-scanpackages`, writes an **unsigned** apt `Release` file, and
  deploys `site/` to GitHub Pages:
  `deb [trusted=yes] https://majklovec.github.io/ctrlirl-coder/deb/dists
  main`. Package version is `0.9.<github-run-number>-1` (strictly newer
  every push, so `apt upgrade` tracks main); `build-deb.sh --version`
  carries it. Pages was enabled via API
  (`POST /repos/…/pages`, `build_type=workflow`). `install.sh` also gained
  `gstreamer1.0-x` (textoverlay). First-run follow-ups: the other two
  1080p60 SwiftShader perf tests (`TestBrowser60FPSFeedSmooth` /
  `TestBrowser60FPSRecoversAfterStatic`, via the shared
  `probeBrowserFeed` helper) now skip under `CI` like the smoothness
  probe, and `qemu-user-static` (arm64 dry-run smoke) is installed
  best-effort — it has no candidate on the 26.04 runner image (universe),
  so the smoke step no-ops when qemu is absent. **The Pages repo part of
  this was later removed** ("no need of github pages") — see the entry
  below.
- **2026-11-27 (semantic-release tag races):** fast-follow pushes left
  overlapping runs whose semantic-release instances each checked out a
  pinned head SHA, so neither saw the other's freshly-cut tag — both
  computed the same next version and the second `git push --tags` was
  rejected (already exists), failing the run and orphaning a tag with no
  release. The version job now syncs branch + tags to origin
  (`git merge --ff-only origin/main`) before running semrel, so an
  already-cut tag is the analysis baseline. The tagless chore/tag pair
  from the race was deleted.
- **2026-11-27 (main component: read published release assets, not run
  artifacts):** the dist job used to re-download the `main` component from a
  *predecessor run's* workflow artifacts (REST run-scan + `download-artifact`
  with `run-id`). That cross-run fetch intermittently 404'd even when the
  artifact existed, failing the whole dist job. `main` now pulls the latest
  **published GitHub Release** debs (permanent, no cross-run artifact
  dependency). The dist job runs *during* the same run that cuts the tag —
  semantic-release makes the tag exist before the Release job attaches its
  assets — so `/releases/latest` mid-run can point at a tag with zero
  assets. The fetch therefore walks the release list and picks the latest
  release that actually has `.deb` assets (worst case `main` lags one
  release until the next run). A failed fetch warns instead of failing the
  run so one flaky release never holds `main` back.
- **2026-11-27 (canonical apt layout, `main`-only, scanpkg/redirect bugs):**
  the apt repo was non-canonical — a per-component `dists/<component>/` each
  with its own `Release` (`Codename` = `<n>`) — so the consumer line
  `…/deb/dists main main-ci` parsed as distribution `main` / component
  `main-ci`, and every `apt-get update` probe 404'd (the i386 probe was a
  fatal `Err`). Restructured to the canonical layout: one `Codename:
  ctrlirl`, per-arch `dists/ctrlirl/<component>/binary-<arch>/Packages.gz`,
  and a single `Release` with `Architectures: amd64 arm64` (no i386). The
  **`main-ci` component is gone** (user: `main` only) — `main` = release
  debs. Source line is now
  `deb [trusted=yes] https://majklovec.github.io/ctrlirl-coder/deb ctrlirl
  main`. Two latent bugs surfaced building it locally:
  `dpkg-scanpackages <dir> -` treats the bare `-` as the *override file*
  (→ "override file - not found", empty Packages) — use `/dev/null`; and
  after a `cd`, redirecting to the pre-cd absolute path writes nowhere —
  redirect to the basename relative to the new CWD (the step now `cd`s into
  the repo root `site/deb`, so `> "$rel/Packages"` with `rel` root-relative).
  Verified: `apt-get update` + `apt-cache policy` + a full `apt-get install`
  resolve and install the package from a `file:` repo of this exact layout.
- **2026-11-27 (apt 3.x skips the Packages index for an unsigned Release that
  carries a hash block):** the earlier bullet ("apt Release file needs a SHA256
  hash block") was wrong in the field — adding the `SHA256:` block made apt 3.x
  (Ubuntu 26.04 / Pop!_OS clients, apt 3.2.1) SILENTLY skip the `Packages`
  index fetch: `apt-get update` fetched the `Release`, never the index, and
  `apt-cache policy` showed no candidate despite every file being present and
  hash-valid on the server. A/B verified in a local `file:` repo on apt 3.2.1:
  ANY hash table present in an UNAUTHENTICATED `Release` (hand-rolled
  `SHA256:`, or `apt-ftparchive release` output, self-entry valid, absent, or
  stale) → index fetch skipped, every time; no hash table → index fetched,
  every time, `Candidate:` appears. `apt-ftparchive release` is also a dead end
  on its own: it always emits hash tables, and its self-`Release` hash line
  can never match the final file (`> Release` truncates the target before the
  digest is computed). So the `package-repo` build step now writes a PLAIN
  hash-less `Release` (Origin/Label/Suite/Codename/Architectures/Components/
  Description/Date), which is the format that reliably installed before the
  block was added. The resulting `W: No Hash entry in Release file` line is
  cosmetic-only while the source line uses a weak trust. **Done:** the
  repo is now GPG-signed (`InRelease` + `Release.gpg`, `signed-by` source
  line, real hash tables back) — see the "apt Release is GPG-signed" item
  in this section.
- **2026-11-27 (apt `Packages` `Filename:` must be relative to the repository
  root, not the component):** the `package-repo` build step originally ran
  `dpkg-scanpackages` from the component dir, so `Filename:` came out
  component-relative (`binary-<arch>/<deb>`). apt joins `Filename:` to the
  repository **root**, so it requested
  `…/deb/binary-<arch>/<deb>` (404) even though the file was present and the
  index was fetched. `apt-get update` + `apt-cache policy` both succeeded
  (which is why it slipped through) but `apt-get install` 404'd on the deb.
  Fix: run `dpkg-scanpackages` from the `site/deb` repo root over the
  root-relative arch dir, so `Filename:` =
  `dists/ctrlirl/<component>/binary-<arch>/<deb>`. Verified end-to-end in a
  local `file:` repo: `apt-get install` downloads the deb and `dpkg -l` shows
  the package installed.
- **2026-11-27 (apt Release is GPG-signed; `[trusted=yes]` is gone):** the
  `Build apt repo (main)` step now emits a real Release with
  `apt-ftparchive -c /tmp/apt-release.conf release` (metadata from
  `APT::FTPArchive::Release::*`; `Architectures`/`Components` auto-detected
  from the `Packages` files) and signs it: `gpg --clearsign → InRelease` +
  `gpg --detach-sign → Release.gpg`. `apt-ftparchive` lives in the
  **`apt-utils`** package (not in `apt` or `dpkg-dev` — the runner image
  ships `apt` without it), so the `Install apt packaging tools` step
  explicitly installs `apt-utils dpkg-dev gnupg`; without it the step dies
  with `apt-ftparchive: command not found`. Signing is the proper fix for the apt
  3.x hash-skip gotcha above — an *authenticated* Release **with** its hash
  table is consumed normally; only *unsigned*-with-hash releases are
  skipped. The release key is `CtrlIRL-Coder Release <release@ctrlirl.local>`,
  fingerprint **`23BDBB59E5D5C21E327B5620E10DFC1CECEDAAA9`** (RSA-4096,
  expires 2036). The **private** key lives in the GitHub secret
  **`CTRLIRL_GPG_KEY`** (armored), imported by the `Import GPG release key`
  step in both the push and release-run `package-repo` jobs; the **public**
  key is exported to `site/deb/ctrlirl.asc` →
  `https://majklovec.github.io/ctrlirl-coder/deb/ctrlirl.asc`. Consumers use
  a `signed-by` keyring:
  ```sh
  sudo mkdir -p /etc/apt/keyrings
  curl -fsSL https://majklovec.github.io/ctrlirl-coder/deb/ctrlirl.asc \
    -o /etc/apt/keyrings/ctrlirl.asc
  echo "deb [signed-by=/etc/apt/keyrings/ctrlirl.asc] \
    https://majklovec.github.io/ctrlirl-coder/deb ctrlirl main" \
    | sudo tee /etc/apt/sources.list.d/ctrlirl-coder.list
  sudo apt-get update && sudo apt-get install ctrlirl-coder
  ```
  The signed `InRelease` carries `Insecure-Allowed: yes`, so the untrusted
  `*.github.io` host passes the apt 3.x signed-URI security check without a
  CA pin — the same `signed-by` line works everywhere.
- **2026-11-27 (Pages deploy: duplicate `github-pages` artifact on job-rerun):**
  `actions/deploy-pages` resolves the site by an artifact **name** in the current
  run. Re-running the `Deploy to GitHub Pages` job in isolation
  (`gh run rerun --failed`) re-uploads a second `github-pages` artifact, and the
  action then fails with `Multiple artifacts named "github-pages" ... count is 2`
  — and every further rerun makes it worse. The deploy also occasionally hits a
  transient `Deployment failed, try again later` on the Pages API side. Both are
  fixed by a **fresh run** (a new commit → new run id → a single `github-pages`
  artifact); do not job-rerun the Pages step.
- **2026-11-27 (main component: Release-job proof, no step outputs):**
  the latest-release scan read the version job's `Detect release tag`
  step `outputs.new_tag` via the REST API, which returns `outputs: null`
  for steps — the check never passed and `main` stayed empty. The
  Release job itself only runs (and attaches assets) when a tag was cut,
  so its `conclusion: success` is the release proof; the scan now keys
  on that alone. A dist-job flake must not hold `main` back (whole-run
  success is not required).
- **2026-11-27 (Pages main-ci missing this-run debs):** the
  `ctrlirl-coder-repo` artifact's root is the repo working dir (upload
  LCA of `dist/*.deb` + `repo/`), so this run's debs live at
  `repo-src/dist/`, not `repo-src/`. The split step's `find -maxdepth 1`
  found nothing and main-ci silently contained only the arm64 CI deb.
  The find now scans all depths, excluding `repo-src/repo/` (the latest
  release's debs, which go to main only).
- **2026-11-27 (CI first-frame flakes: serialize dist jobs + retry
  filter test):** the `dist-amd64` and `dist-arm64` jobs built GStreamer
  stacks concurrently on separate 2-vCPU runners while the Release job
  also ran its full build, and the first-frame tests kept timing out at
  30 s (`TestRotateTransparentCorners`) even at 30 s. `dist-arm64` now
  `needs` `dist-amd64` (skips when that one is skipped), so at most two
  GStreamer-heavy jobs run at once, and the filter test re-arms its
  finite pipeline for a second 30 s attempt before failing.
- **2026-11-27 (TestAppsinkPull flake on 2-vCPU runners):** the
  live-paced (`is-live=true` 30×30fps) pull test can yield 0 frames when
  the test process is descheduled while the finite source runs out — the
  drop=true appsink flushes buffers the puller never drained. The run body
  is now retried up to 3 times with backoff before failing; non-flaky
  failures (pipeline parse/play) still fail immediately.
- **2026-11-27 (release assets + apt repo fixes):** four follow-up bugs
  from the first green release run: (1) **artifact name shadowing** — the
  dist jobs and the Release job both uploaded `ctrlirl-coder-amd64` /
  `ctrlirl-coder-arm64` in the same run, so the Release job's
  `download-artifact` merged the *dist* amd64 artifact (CI-versioned deb)
  + the `ctrlirl-coder-debs` repo artifact and attached the wrong files
  (2 of 4 assets, missing arm64 entirely). Renamed: Release uploads
  `ctrlirl-coder-rel-{amd64,arm64}` (debs) + `ctrlirl-coder-tar-{amd64,arm64}`
  (tarballs); its download pattern is
  `ctrlirl-coder-rel-*,ctrlirl-coder-tar-*`; the repo artifact is now
  `ctrlirl-coder-repo`. (2) **malformed deb version** —
  `build-deb.sh --version v1.2.2` produced `0.v1.2.2-1` (the `v` failed the
  digit check, gaining a `0.` prefix); the script now strips a leading
  `v` before sanitising, so tag builds are
  `ctrlirl-coder_1.2.2-1_<arch>.deb`. (3) **split step set -e exit** —
  the package-repo ci-mode split ended with
  `[ -d repo-src/repo ] && find ...`, which fails under `set -e` when no
  earlier release exists (first-ever green release); now an `if` block
  that mirrors main-ci into main when `repo/` is absent. (4) **main-ci
  had no arm64 deb** — the dist-arm64 job now uploads
  `ctrlirl-coder-ci-arm64` and package-repo downloads it into main-ci
  (debs are per-arch, so main-ci carries both architectures).
- **2026-11-27 (single CI workflow: version + dist + release + Pages):**
  `release.yml` deleted; everything lives in `ci.yml` now — one run per
  push instead of two overlapping runs (the Release run's dist build raced
  the CI run and got cancelled, leaving empty releases). Jobs:
  `version` (semantic-release, main pushes) · `pure-go` · `bindings` ·
  `dist-amd64`/`dist-arm64` (CI: full test suite + debs, main pushes) ·
  `release` (only when `new_tag` is set: tag ref checkout, full
  amd64+arm64 build/test, versioned debs + tarballs,
  `softprops/action-gh-release`) · `package-repo` (Pages apt repo +
  index.html + /bin; triggers on green CI pushes and successful
  `workflow_run` of this same workflow). Anti-duplication: dist jobs run
  a `skip-check` step — if an earlier run of this workflow for the same
  HEAD finished with a successful Release job, the CI dist build is
  skipped (the release already built+tested). Concurrency
  `cancel-in-progress` per ref keeps one in-flight run per branch.
- **2026-11-27 (post-consolidation CI fixes):** two follow-up bugs from
  the first integrated runs — (1) the apt retry loops were written with
  `&& break` on its own line, a bash syntax error (`syntax error near
  unexpected token '&&'`) that killed every install step; the `break` is
  now appended to the `apt-get install` line. (2) `github-script`'s
  typed client returns `{data: [...]}` from `listWorkflowRuns`, so the
  dist jobs' `skip-check` crashed with `TypeError: runs is not
  iterable`; the calls now destructure `.data` (the `package-repo` job
  already used `paginate`, which returns the bare array).
- **2026-11-27 (stale-push release cancellation):** rapid consecutive
  pushes cancel the earlier Release run's dist jobs mid-build
  (GitHub cancels superseded runs), so a tag can be cut with an empty
  release (happened to v1.2.0). Recovery: delete the release + tag and
  push a commit to re-cut. The cancel-prone window is ~the dist build
  time; the version job (tag + release draft) finishes in seconds, so
  only the asset upload gets cancelled.
- **2026-11-27 (Pages index.html with .deb list):** the
  `package-repo` job now writes `site/index.html` (static, styled) listing
  every published `.deb` (version / component / arch / size / download
  link) alongside the repo-add instructions and the component table.
  Version comes from `dpkg-deb -f` with a basename fallback; arch from the
  `binary-<arch>` directory. Generated in the same step set as the apt
  metadata, so it is always in sync with the deploy.
- **2026-11-27 (action version bumps + Pages apt repo revived):**
  `actions/checkout` v4→v7, `actions/setup-go` v5→v7 (Node 20 deprecation
  warnings on runners), artifact actions v7. The Pages apt repo came back
  (user re-requested it) as a single `package-repo` job in `ci.yml` — one
  Pages deploy must carry the whole site, so both components go out
  together:
    - **`main`** — release debs (tag version `1.x-y`), from the latest
      successful Release run that cut a tag (job outputs queried via
      `github.rest.actions.listJobsForWorkflowRun`).
    - **`main-ci`** — per-push CI debs (`0.9.<run-number>-1`) from the
      current green CI push.
  Triggers: green CI pushes to main **and** successful Release workflow runs
  (`workflow_run` in the CI workflow). Dist jobs build the debs again and
  upload a `ctrlirl-coder-debs` artifact; the job consumes them. Pages
  re-enabled via API (`build_type=workflow`). Both components monotonic →
  `apt upgrade` tracks newest of each.
- **2026-11-27 (semantic-release versioning + changelog + GitHub
  Release; Pages apt repo removed):** `version` → `dist-{amd64,arm64}` →
  `release` pipeline in `.github/workflows/release.yml`, modeled on the
  `app` repo's flow:
    - **`version`** (main pushes only): `npm i --no-save
      semantic-release@24 @semantic-release/changelog
      @semantic-release/git @semantic-release/github`, runs it with
      `.releaserc.json` (commit-analyzer, notes generator, changelog →
      `CHANGELOG.md`, git commit of the bump, `@semantic-release/github`
      for tag + release draft). Emits `new_tag` when the tag lands on
      HEAD. No `@semantic-release/npm` — no root `package.json`.
    - **`dist-amd64` / `dist-arm64`** (only on release): check out
      `${{ needs.version.outputs.new_tag }}`, **not** the trigger commit —
      semantic-release's `chore(release)` commit moves HEAD past the push
      (`[skip ci]` stops that commit re-triggering CI). Build/test/deb
      steps unchanged; deb version = tag (`v` stripped by
      `build-deb.sh`, `-1` revision appended); artifacts are the
      release payloads.
    - **`release`**: downloads both payload artifacts, attaches them to
      the semantic-release draft via
      `softprops/action-gh-release@v3` (body already = changelog).
      Release assets per arch: `.deb` + `ctrlirl-coder-<ver>-linux-<arch>.tar.gz`
      (binary + `libsrt.so.1.5` + `build.json`), so both compiled executables
      and the Debian packages are always on the release page.
  First release runs (v1.0.0, v1.0.1) hit a real test flake:
  `TestRotateTransparentCorners` (added externally for the rotate-alpha fix)
  timed out waiting for the first appsink frame on the 2 vCPU runner
  (0.21 s locally) — its 5 s deadline is now 30 s. Both empty releases were
  deleted; the fixed pipeline re-cut **v1.0.2** with all four assets:
  `ctrlirl-coder-1.0.2-linux-{amd64,arm64}.tar.gz` +
  `ctrlirl-coder_1.0.2-1_{amd64,arm64}.deb`.
  `CHANGELOG.md` seeded with a manual `0.0.0` entry; semantic-release
  prepends from the first tag onward. Conventional commits drive semver:
  `feat:` minor, `fix:` patch, `!`/`BREAKING CHANGE` major.
- **2026-11-26 (CI fixes: apt package, relative cgo paths, bun test flakiness):**
  Three CI failures on `ubuntu-latest` (Ubuntu 26.04 "resolute"):
    - **`gstreamer1.0-tools-good` no longer exists** in Ubuntu 26.04 (same as
      bookworm, noted above) — `apt-get` exit 100 killed the GStreamer tier. The
      gstreamer tier now installs only `gstreamer1.0-tools` (provides
      `gst-launch-1.0`, all the stream/integration scripts need).
    - **GStreamer *dev* headers missing from CI apt lines** (second run): the
      tiers installed `gstreamer1.0-dev` (the runtime lib, no `.pc` files)
      instead of the dev packages, so cgo's pkg-config probes failed with
      `Package 'gstreamer-1.0' not found` (amd64) and
      `Package 'gstreamer-app-1.0' not found` (arm64, which also lacked
      `libgstreamer-plugins-base1.0-dev:arm64`). Fixed to
      `libgstreamer1.0-dev` (+ `libgstreamer-plugins-base1.0-dev` on arm64),
      matching the Makefile header prerequisites.
    - **Makefile swallowed `go build` failures:** the `dist-amd64` (and `test`)
      recipes chained commands with `;`, so a failed `go build` fell through to
      the `cp` steps and the real error was masked by
      `cp: cannot create regular file 'bin/…'`. Recipes now run under
      `set -e`.
    - **`gstreamer1.0-x` missing on CI (third run):** the compositor/layer
      tests build text layers, and the `textoverlay` GStreamer element lives in
      `libgstpango.so` = package **`gstreamer1.0-x`** (not in base/good/ugly/
      bad) → `textoverlay element factory not available` on 6 tests. Added
      `gstreamer1.0-x` to the gstreamer tier apt line. `TestBrowserLineBounceSmoothness`
      (SwiftShader 1080p perf probe, 45 fps gate) rendered 42.5 fps on the
      2-vCPU runner, so it now skips when `CI` is set — the probe stays the
      regression gate on real hosts.
    - **Relative cgo paths broken arm64 cross-compile:** the CI
      `cross-arm64` job's inline go build used `BUNDLE=.build/ceralive-srt/arm64`
      with `CGO_CFLAGS="-I$BUNDLE/include"` / `CGO_LDFLAGS="-L$BUNDLE/lib …"`.
      cgo compiles in a temp dir, so the relative `-I/-L` resolved nowhere →
      `srt/srt.h: No such file or directory`. Fixed to `BUNDLE="$PWD/…"` in the
      workflow, and hardened the Makefile: `CERALIVE_CGO_CFLAGS` /
      `CERALIVE_CGO_LDFLAGS` now use `$(abspath $(BUNDLE_DIR))` so any consumer
      is immune. Verified: the exact CI cross-compile command now produces a
      valid `ELF 64-bit … ARM aarch64` binary.
    - **TS bindings `bun test` failures:** (a) the dry-run test's binary-missing
      guard used `Bun.file(binPath).exists()` — **a Promise, always truthy** —
      so the "skip when no binary" clause never fired and `Bun.spawn` threw
      `ENOENT` on runners without `bin/`. New synchronous `fileExists()`
      (node:fs `statSync`) in `bindings/src/process.ts`; the test skips in 0.15 ms
      without a binary. (b) the stub signal-lifecycle test raced CI's slow node
      start: SIGHUP landed before the stub's `process.on("SIGHUP")` was
      registered, node's default SIGHUP action killed the stub, and `sendTerm()`
      hit `ESRCH`. Tests now `waitForLine()` on the stub's `stub: up` /
      `stub: env=hello` lines (handlers registered) instead of fixed 200 ms
      sleeps.
- **2026-11-25 (deb install path → `/usr/local/bin`):** The `.deb` now installs the
  binary at `/usr/local/bin/ctrlirl-coder` (with the CERALIVE `libsrt.so.1.5` and
  `build.json` alongside it) instead of `/opt/ctrlirl-coder/`, and drops the old
  `/usr/bin/ctrlirl-coder` symlink — the `$ORIGIN` rpath resolves the bundled lib next
  to the binary in either location, and the systemd `ExecStart` updated to match.
  `make deb` now builds the host dist first (`deb: dist-$(HOST_DEB_ARCH)` prereq), and
  untagged-repo versions (bare git sha) get a `0.` prefix so dpkg accepts a digit-leading
  version.
- **2026-11-23 (deb packaging):** Added a self-contained Debian package builder —
  `scripts/build-deb.sh` (+ `Makefile` targets `deb` / `deb-amd64` / `deb-arm64`,
  `packaging/deps.txt`). The `.deb` bundles the Go
  binary **and** the CERALIVE `libsrt.so.1.5` together under `/usr/local/bin` so the
  binary's `$ORIGIN` rpath (and the `srt.EnsureSRTBackend()` lib-SHA guard) resolve with
  no system `libsrt`. Runtime deps are
  declared in `Depends` (GStreamer core/app + the good/ugly/bad/libav plugin sets,
  `libgnutls30`/`libnettle8` for the bundled SRT, Pulse/PipeWire for the browser layer,
  with a `libglib2.0-0 | libglib2.0-0t64` alternative for Debian vs Ubuntu ≥24.04). Ships
  `/etc/ctrlirl-coder/config.toml` (a dpkg **conffile**), `scene.example.json`, the
  pipeline templates under `/usr/share/ctrlirl-coder/pipeline/`, and a systemd unit.
  Version is `git describe`-derived with a `-1` revision; the binary is `strip`ped.
  `dist/` is gitignored. Also fixed a pre-existing TOML bug in `config.toml.example`
  (unquoted `sink`/`balancer`/`mode` values made it fail to parse) — it is the deb's
  default config, so a fresh install would not have started. Cross-arch support:
  `build-deb.sh --arch <arch>` cross-compiles arm64 natively on an amd64 host
  (`aarch64-linux-gnu-gcc` + arm64 GStreamer/gnutls/nettle dev packages; SRT bundle
  via `build-ceralive-srt.sh --arch arm64`), falling back to a Docker buildx
  image (emulated) when the toolchain is missing; `make deb` / `deb-amd64` /
  `deb-arm64`. Strip uses the target binutils (`aarch64-linux-gnu-strip`) because
  the host strip rejects cross-arch ELFs. Verified: `make deb` builds,
  `dpkg-deb -I/-c` shows correct control + layout, the extracted stripped amd64
  binary passes `--dry-run` (config + full pipeline) with the bundled lib resolved
  via `$ORIGIN`, and the arm64 package carries an ARM aarch64 binary + libsrt
  with `RUNPATH=$ORIGIN` and the matching bundle SHA.
  **Depends fix (2026-11-23):** the hardcoded `libgstapp-1.0-0` dep is
  unresolvable on Ubuntu ≥ 24.04 / Debian trixie (the gstapp library merged into
  `gstreamer1.0-plugins-base` there), so it is now the alternative
  `libgstapp-1.0-0|gstreamer1.0-plugins-base` (debs rebuilt, `dist/` updated).
- **2026-11-23 (Dockerfile fixes for the buildx cross path):** Surfaced while
  prototyping the arm64 deb via buildx (qemu). The emulated path is now a
  *fallback*: in-image cgo under qemu-user deadlocks (`go build` of go-glib spins
  forever; `cgo` can't even parse glib symbols), so arm64 debs are cross-compiled
  natively on the host (`scripts/build-deb.sh` prefers
  `aarch64-linux-gnu-gcc`). The Docker fixes below still apply to the runtime
  image / fallback path.
    - Bookworm package names: `gstreamer1.0-tools-good` does not exist (dropped); the
      pulse plugin is `gstreamer1.0-pulseaudio` there (`gstreamer1.0-plugins-pulse` is
      trixie-only — the deb's `Depends` lists both as an alternative); the libav plugin
      is `gstreamer1.0-libav` in `contrib` (no `gstreamer1.0-plugins-libav` package).
    - The bundled rpath must be set via command substitution: BuildKit pre-expands
      `$` in `RUN` (it turns `"\$ORIGIN"` into `$ORIGIN` for `/bin/sh`, fatal under
      `set -u`; `$$` arrives as `1`, not a literal dollar). The image build now
      computes `ORIGIN="$(printf '%s' '$')ORIGIN"` — the `$()` is left for the shell
      — and feeds `CGO_LDFLAGS=…-Wl,-rpath,${ORIGIN}…`; the binary ends up with the
      intended literal `$ORIGIN` RUNPATH.
    - **Root cause of the in-image go-glib cgo failure (bookworm):** the error
      `could not determine what C.GPathBuf refers to` is NOT an escaping or qemu
      problem — bookworm's `libglib2.0-dev` (glib 2.74) ships no `gpathbuf.h` at
      all (`grep -rl _GPathBuf /usr/include/glib-2.0/` is empty), while go-gst's
      generated cgo preamble references `GPathBuf`. It fails identically for native
      amd64 AND arm64 inside any bookworm container. The Dockerfile therefore
      builds on **`golang:1.26-trixie`** (glib 2.84, header present); the runtime
      image is `debian:trixie-slim`.
    - **Stage architecture:** `builddeps` (`--platform=$BUILDPLATFORM` golang,
      shared toolchain) → `build` (native, TARGETARCH) → `cross` (installs the
      aarch64 gcc + arm64 GStreamer/glib/gnutls/nettle dev packages in-image,
      cross-compiles SRT + the go binary natively) → runtime
      (`--platform=$TARGETPLATFORM`). `ARG SRC=build|cross` (global, before any
      `FROM`) selects the source stage via `FROM ${SRC} AS built`, because
      `COPY --from=` does not expand variables. `make docker-arm64` passes
      `--build-arg SRC=cross` on amd64 hosts; the arm64 go build sets
      `PKG_CONFIG_PATH=/usr/lib/aarch64-linux-gnu/pkgconfig` (cgo's `#cgo
      pkg-config:` directives hard-fail without it) and links with an explicit
      `-L/usr/lib/aarch64-linux-gnu` (the cross `ld` does not search the
      multiarch dir by default).
- **2026-11-23 (CERALIVE SRT fork):** Swapped the native SRT backend from stock Haivision
  libsrt to the [CERALIVE fork](https://github.com/CERALIVE/srt) (SRT 1.5.7, GnuTLS), built from a
  pinned source commit by `scripts/build-ceralive-srt.sh` into a private bundle. `internal/sinks/srt`
  is therefore **cgo** (the `haivision/srtgo` wrapper links `-lsrt`) — corrected the stale "pure-Go /
  no libsrt" claims in the README/AGENTS/phase table. Added the runtime guard
  `srt.EnsureSRTBackend()` (`internal/sinks/srt/verify.go`, exported + called from `run_gst.go` before
  the data path) which re-hashes the mapped `libsrt.so.1.5` against the **lib SHA-256 embedded via
  ldflags** and refuses to stream on mismatch (a bare `go build` has an empty identity and cannot
  start the SRT path). Wired the bundle + identity into `Makefile` (`bundle-amd64`, `dist-amd64`,
  `test`, `vet`), the `Dockerfile` (in-band amd64/arm64 build, ships the lib + GnuTLS, drops stock
  `libsrt`), `.gitignore`, and the CI tiers (pure-Go now tests the cgo-free package set only; the
  gstreamer + cross-arm64 tiers build the fork and link against it). Verified: full `make test`
  green (incl. SRTLA real-session) against the fork, and `test-stream` decodes 235 h264/aac frames
  with zero errors against a local `srtsrc` listener. Receiver-side `SRTO_*` fork options are
  present but left default-off; sender behaviour is unchanged.
- **2024-xx (SRT pivot):** Rewrote `internal/sinks/srt` from a cgo send client to a
  `StatsSource` abstraction (`Stats` + `SrtsinkStats` + `MockStatsSource`); deleted
  `client.go`/`client_cgo.go`/`client_stub.go`. Confirmed `srtsink` `stats` field names
  empirically. Updated `SPECS.md` throughout (contract table → `srtout`, §5.2/§5.3/§5.4/§5.6/§5.7/§5.8,
  §6 phases, §7 pitfalls, §9/§10/§12/§13). Pinned `go-gst v1.4.1` in `go.mod`. Added this file.
- **2026-11-21 (gstreamer tier complete):** Wrote `internal/gstutil` (`EnsureInit`, comment
  stripping, `SetStateSafe`), `internal/layer` (flat element chains with BGRA tail), and
  `internal/compositor` (command-goroutine pipeline, `AddLayer`/`RemoveLayer`/
  `UpdateLayerProps`, `SetBitrate` as `uint32` kbps, `SetOverlayText` via GType-matched
  textoverlay). Renamed the encoder property from `venc_bps` to `venc_kbps` (x264enc `bitrate`
  is kbit/s) in both pipeline templates and SPECS.md. **Root-caused the layered-pipeline
  "not-linked" failure:** GStreamer 1.28 `gst_bin_add` breaks pre-insertion pad links, so
  `BuildChain` now returns unlinked elements and `Chain.LinkChain()` runs after bin insert
  (§2.4). Pipeline stays NULL until `Start()` (a source-less compositor can't preroll). Both
  pipeline templates stream end-to-end against a local `srtsrc` listener with 0 CRITICALs;
  full test suite green in both tiers.
- **2026-11-21 (arm64 cross-compilation):** Both tiers now cross-compile to `linux/arm64`.
  Pure-Go tier: plain `GOOS=linux GOARCH=arm64`. GStreamer (cgo) tier: arm64 multiarch dev
  packages (`libglib2.0-dev:arm64`, `libgstreamer1.0-dev:arm64`) + `aarch64-linux-gnu-gcc` +
  `PKG_CONFIG_PATH=/usr/lib/aarch64-linux-gnu/pkgconfig` (headers are shared; only the
  compiler and `.pc` path are arch-specific). Wired into `Makefile` (`build-arm64`,
  `dist`, `docker-arm64`), CI (`cross-arm64` job with artifact upload),
  and the README. Verified: `dist/ctrlirl-coder-linux-arm64` runs under
  `qemu-aarch64-static -L /usr/aarch64-linux-gnu` and its `--dry-run` validation passes.
- **2026-11-21 (TOML config + dist cleanup):** Configuration format switched from INI to
  **TOML** (user-directed): `gopkg.in/ini.v1` → `github.com/BurntSushi/toml`, all `ini:"…"`
  struct tags → `toml:"…"`, `ctrlirl_coder.conf.example` → `config.toml` (table/key names
  unchanged — the original schema carries over verbatim). Updated: `internal/config`
  (loader + tests), `main.go` flags, `scripts/integration-smoke.sh`, `Dockerfile`,
  `README.md`, and `SPECS.md` (recorded revision + all references incl. the §13 TypeScript
  serializer contract, which now emits TOML). Also removed the pure-Go `-nogst` cross/dist
  variants (`make build-arm64-gst`, `…-nogst` dist names, CI step, README section) — the
  GStreamer tier is the only shipped binary; the pure-Go tier remains as the
  tag-less `build`/`test` path.
- **2026-11-21 (SRT streaming test script):** Added
  `scripts/test-stream.sh` (`make test-stream`) — streams the full pipeline to a local
  `srtsrc` listener on `127.0.0.1:7000` (configurable via `PORT`/`DURATION`/`MIN_BYTES`/
  `KEEP_TMP`) and asserts: PLAYING reached, zero CRITICAL/ERROR, graceful SIGINT shutdown,
  clean listener, and ≥ `MIN_BYTES` of H.264 captured. **Found + fixed a real receiver
  bug:** the SRT payload is raw H.264 NAL units that a bare `decodebin` cannot typefind
  ("Could not determine type of stream") — SRT _receivers_ must put `h264parse` first.
  Fixed the `integration-smoke.sh` listener the same way (it had no data assertion, so
  the broken receiver shape passed silently before). The bundled config was renamed to
  `config.toml`; all references (README, Dockerfile, SPECS, scripts) updated. Both
  scripts pass end-to-end (~2.5 MB captured in 10 s). _(Correction: the receiver
  does NOT need h264parse — put it on the receiver side and it reframes the stream
  as AVCC; capture raw: `srtsrc ! filesink`. The h264parse requirement is on the
  SEND side, see next entry.)_
- **2026-11-21 (OBS "no video" fix — SRT H.264 framing):** Root-caused why OBS
  showed no picture on `srt://localhost:7000` while the test scripts "passed"
  (bytes flowed, no errors). The sender pipeline lacked Annex-B framing:
  `x264enc` emits AVCC (4-byte length-prefixed) NALs and GStreamer's
  `srtsink` transmits them as-is; ffprobe on a capture showed
  "non-existing PPS" / "no frame!" garbage. Two traps found while fixing:
  (1) a bare `h264parse` is a **passthrough** — it negotiates `stream-format=avc`
  with `srtsink` (which accepts anything) and never converts; (2) on
  GStreamer ≥1.24 the caps field is **`stream-format=`** and the legacy
  `format=byte-stream` capsfilter is silently ignored. Fix in both bundled
  templates: `… ! x264enc name=venc_kbps … ! h264parse config-interval=1 !
video/x-h264,stream-format=byte-stream ! srtsink …` (`config-interval=1`
  re-emits SPS/PPS for mid-stream players). Verified: raw SRT capture of the
  app now starts with `00 00 01` start codes and ffprobe decodes 445 frames
  (320×240) from a 12 s capture. Hardened `scripts/test-stream.sh`: the
  listener now captures raw (`srtsrc ! filesink` — receiver-side h264parse
  would reframe to AVCC), and a **frame-count assertion** (`ffprobe
-count_frames`, `MIN_FRAMES` default `$DURATION/2`) proves the stream is
  decodable video, not just bytes. Both test scripts re-verified green.
  Docs: SPECS.md §2.1 (framing template requirement + gotcha), README
  (pipeline contract section + "viewing in OBS" note).
- **2026-11-21 (test-obs script):** Added `scripts/test-obs.sh`
  (`make test-obs`) — streams the **dist** binary
  `dist/ctrlirl-coder-linux-amd64` (rebuilt if missing) into an
  already-running OBS _SRT Input_ source listening on
  `127.0.0.1:7000` (configurable via `PORT`/`DURATION`). Unlike
  `test-stream.sh` it spawns no listener: it asserts a UDP listener is
  present on :PORT (with setup instructions if not), rewrites the
  template's srtsink URI, streams for `DURATION` s, and checks
  PLAYING / zero CRITICAL / graceful shutdown. Verified against a
  gst-launch stand-in listener (25 MB in 10 s).
- **2026-11-21 (SRT back to a Go-owned socket + two "no video" bugs fixed):**
  The in-pipeline `srtsink` pivot (entry above) was **reverted** to the
  original CtrlIRLCoder/belacoder architecture the operator re-confirmed:
  **Go owns the SRT socket** and pulls the encoded bytes off a terminal
  `identity name=identity ! fakesink` **`handoff` byte-tap**
  (`internal/gstpull` + `internal/sinks/srt` `Client`). Rationale (product
  owner): the adaptive controller runs every 20 ms and needs low-level
  SRT feedback — `srt_bstats` RTT, `SRTO_SNDDATA` send-buffer occupancy,
  packet loss, throughput — that an in-pipeline `srtsink` cannot be
  polled at that cadence, and it keeps the socket free for `srtla`
  bonding. Consequences: the pipeline templates end in
  `… ! identity name=identity ! fakesink` (no `srtsink`), `internal/sinks/srt`
  is a real send client (`haivision/srtgo`) again, and
  `SendBufferBytes` is now **live** (from `ByteSndBuf`/`SRTO_SNDDATA`),
  so the adaptive balancer's buffer-pressure term is active (see §4 #4,
  now resolved).
  Two latent bugs found + fixed while verifying the loop end-to-end
  against a local `srtsrc` listener:
    1. **`identity` never emitted `handoff`.** GStreamer's `identity`
       fires its `handoff` signal only when `signal-handoffs=true`
       (default `false`); nothing set it, so the puller's MPSC ring stayed
       empty, **zero bytes reached the SRT socket, and OBS saw no video**
       even though the pipeline reached PLAYING with 0 errors. Fix:
       `contract.go` now sets `signal-handoffs=true` on the byte-tap
       authoritatively (independent of the template text). Verified:
       `sent_total` climbs 0 → 11k packets and a raw capture is a
       well-formed 2.2 MB / 7.8 s **MPEG-TS with decodable H.264 + AAC**
       (clean decode to EOS, no errors).
    2. **Write-path throughput tracker dead on first call.**
       `sendRateTracker.mbps()` returned on the first call _without_
       priming `lastT`, so it could never start measuring and reported
       0 Mbps forever. SRT's own `mbpsSendRate` also reads ~0 on low-RTT
       / loopback links, so the balancer's `RTT_TO_BS` (packets-in-flight
       cap on the `bs_th2` buffer threshold) collapsed to ~0 and the
       adaptive branch sat in perpetual **"heavy"** → the target fell a
       steady −700 kbps/cycle (6000→0) on a perfectly healthy link.
       Fix: the first `mbps()` call primes the `lastT`/`lastB` baseline
       (verified in isolation: tracker now reports the real ~10 Mbps).
       Together the two fixes make `srt://localhost:7000` carry a real,
       decodable stream into OBS again (the OBS "no video" symptom).
       Diagnostic `adaptive` per-tick log downgraded to `Debug`.
- **2026-11-21 (byte-tap deadlock fix + pipeline template rewrite):** Two coupled bugs found and
  fixed. 1. **`handoff` buffer extraction deadlocked under load; SIGTERM teardown hung.** The first
  design kept tap buffers in a fixed-size **C MPSC ring** guarded by `pthread_mutex`/`pthread_cond`,
  with the GStreamer streaming thread running the ring producer _inside the cgo `handoff`
  callback_. A blocking cgo callback is unsafe: the Go runtime can migrate/reclaim the OS
  thread parked in a native futex, so the wakeup is lost permanently — the producer wedged in
  the critical section holding the mutex and both the consumer and the main-thread teardown
  blocked on it forever. Symptom: ~128-buffer burst then a silent stall (data dried up), plus
  a SIGTERM that never returned. **Fix:** `internal/gstpull` is now **pure `go-gst`** — the
  `handoff` callback is strictly **non-blocking** (copy bytes → `select { case ch <- b: default:
dropped++ }`, unmap the buffer _before_ the send) and a dedicated **drain goroutine** reads a
  bounded `chanSize = 4096` channel and calls `srt.Client.Write` (the _only_ safe place to
  block; it is a Go citizen, not the streaming thread). No native sync remains on the cgo
  boundary. The exact `handoff` callback signature is `func(el *gst.ElementInstance, buf
*gst.Buffer)` (go-gst's marshaller asserts `nCbParams == nTotalParams`; a `*gst.Element`
  param-0 or a 3-arg form panics). `gobject` (`go-glib v0.0.1`) is now a direct dep for
  `SignalHandle`. Verified: 30 s soak with continuous `bytes_sent`, `loss 0%`, and a **~0.1 s
  SIGTERM exit**; a raw capture is a well-formed MPEG-TS (h264 1080p + AAC) that decodes with
  **0 errors**. Added a lightweight 1 Hz `tap diag` log line (produced/dropped/queued/
  bytes_sent/last_frame) for observability. 2. **Pipeline template rewrites + OBS video fix.** `example-full.pipeline` now mirrors the
  operator's canonical shape: `smpte 1920x1080@30 → videoconvert → textoverlay (overlay) →
   videoconvert (I420) → x264enc (6000k, superfast, zerolatency) → h264parse → voaacenc (128k)
   → aacparse → mpegtsmux → identity → fakesink`. The **H.264 framing fix** for OBS is in place:
  `h264parse config-interval=1` + an explicit `video/x-h264,stream-format=byte-stream` caps filter
  (Annex-B; GStreamer ≥1.24 uses `stream-format=`, the legacy `format=byte-stream` is ignored).
  Parity vs the original `./ctrlirl_coder` (same MPEG-TS H.264 1080p + AAC container; steady-state
  trailing bitrate in the same class, both converging to the 500 kbps floor) is recorded in
  §6.
  Docs: new §2.5 (cgo/threading gotcha + `handoff` signature), §6 verification + parity, and this
  entry. `scripts/capturelog.sh`'s now-dead `kill -USR1` ring-dump line removed (the cgo dumper is
  gone with the ring).
- **2026-11-21 (appsink data path — full CtrlIRLCoder/belacoder parity):** Replaced the interim
  `identity`+`handoff` byte-tap stand-in with the **original `appsink` pull path**, matching
  `ctrlirl_coder`/`belacoder.c` verbatim. `libgstreamer-app` (dev + runtime) is now a build/runtime
  prerequisite (it ships with `gstreamer1.0-plugins-base`).
    1. **`internal/gstpull` rewritten as an appsink puller (cgo).** New `pull_cgo.go` binds
       `libgstreamer-app` (`#cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0`); `pull.go` drives
       `gst_app_sink_pull_sample` on a **dedicated Go goroutine** (`Run(ctx, send)`). No cgo
       callback on a GStreamer streaming thread remains — the pull _is_ the drain, so the whole
       cgo-ring/handoff deadlock class is structurally gone. The pipeline terminal is now
       **`appsink name=appsink`** (was `identity name=identity ! fakesink`); both templates updated.
    2. **Appsink stats surfaced.** The pull loop tracks frames pulled/sent/dropped, bytes,
       throughput (kbit/s, 500 ms window), negotiated framerate (read once from the appsink's
       sink-pad caps — the appsink's **`sink`** pad is the static pad carrying the negotiated video
       caps), and last-PTS. `contract.go` now verifies the terminal is an `appsink` (GType) and
       forces **`drop=true`** (back-pressure drops frames; the pull goroutine never wedges the
       pipeline).
    3. **Frame-production stall wired into the adaptive controller.** `gstpull.Puller.Stalled()`
       reports "no encoded frame for ≥ `general.frame_stall_ms`" (default **3000 ms**, mirroring
       ctrlirl_coder's `frame_liveness_threshold_ms`). `internal/controller` gains a `Liveness`
       interface; the 500 ms `balancerLoop` drops the encoder to `MinBitrate` while stalled (the
       zombie-encode emergency the original watchdog did). `config` gains
       `general.frame_stall_ms` (default 3000).
    4. **Tests + build.** `internal/gstpull/pull_test.go` (E2E `videotestsrc → appsink` pull: frame
       count, bytes, throughput, framerate, PTS, drop-free exit on EOS; plus stall detection).
       `controller_test.go` updated for the new `New(src, liveness, applier, cfg)` signature +
       `TestStalledAppsinkDropsToMin`. `Dockerfile` build stage adds
       `libgstreamer1.0-dev` + `libgstreamer-plugins-base1.0-dev`; `Makefile` prerequisite comment
       updated. `CGO_ENABLED=1 go build ./...` / `go vet ./...` / `go test ./...` all green.
       Docs: SPECS.md + this file rewritten from the identity-byte-tap design to the appsink design
       (contract table, §2.2/§2.5, §5.6/§5.8, §7, §12, package table, deviations).
- **2026-11-21 (unified build output dir: bin/):** All build artifacts now land in a single
  `bin/` directory — the `dist/` output location is gone. `Makefile`: removed the `DIST`
  variable; `dist-amd64` (and `make test-obs`) now write
  `bin/ctrlirl-coder-linux-amd64`; `clean` removes only `bin`. CI `cross-arm64` job builds to
  `bin/ctrlirl-coder-linux-arm64` and uploads from there. `scripts/test-obs.sh` default
  `BINARY` is `bin/ctrlirl-coder-linux-amd64`. `.gitignore` drops `dist/` (`bin/` was
  already ignored). README cross-compile section updated to the `bin/` paths.
- **2026-11-21 (scene filters — FILTERS.md implemented):** Per-layer `filters`
  array in the scene schema, built from a closed registry
  (`internal/scene/filters.go`, pure Go): video kinds `videoscale`,
  `videocrop`, `videobalance`, `videobox`, `videoflip`, `videorotate`,
  `effectv`; audio kinds `volume`, `equalizer` (resolves to
  `equalizer-{3,4,5,10}bands`, band gains `band0`…`bandN-1`), `audiodynamic`
  (`mode` 0=compressor/1=expander — the upstream enum has exactly two
  values), `pitch`. Scene load
  validates kind, format-vs-layer (audio filters only on `browser` layers —
  the only type that **accepts** audio filters; video media sources carry an
  unfiltered audio chain too, see the 2026-11-23 change log), param
  names/types, and per-kind cross-param invariants.
  `internal/layer/filters.go` realizes specs as
  chain elements named `<id>-fNN-<kind>`: video filters splice between the
  source and the `videoconvert` tail (all four layer types); audio filters
  sit between the `audioconvert` and the chain tail (shared `audiomixer`
  mix) in the browser audio chain.
  Element/unit adaptations (GStreamer 1.26, verified in Docker):
  `effectv` is not registered by stock Debian (bookworm AND trixie)
  plugin registries (the .so loads under gst-inspect's direct-file
  fallback only) — kept in the registry, fails cleanly at chain build
  on such hosts; `videorotate` → element `rotate` (degrees → radians,
  plugins-bad),
  `videoflip` `direction` → `video-direction` GEnum (none=0,
  horizontal=4, vertical=5; no "both" value in 1.26), equalizer band
  gains are element props `band0`…`bandN-1`, `audiodynamic` has only
  `mode`/`characteristics`/`ratio`/`threshold` (threshold normalized
  0..1), `pitch` exposes `pitch`/`tempo`/`rate` (each 0.1..10).
  `videoscale` emits a trailing `capsfilter` carrying the target
  width/height (the element only conforms to the caps it's given). Scene
  diff: a filter change emits `RemoveLayer` + `AddLayer` for that layer
  (running chains cannot be spliced; the remove→add ordering re-establishes
  z-order on re-attach). The compositor is untouched — filters ride the
  existing chain add/teardown paths. Tests: scene validation + diff
  rebuild (pure Go) and layer chain construction (Docker/CI where the G
  plugins are present; skips on bare dev-header hosts). Docs:
  `scene.example.json`, README scene section, SPECS §5.4/§5.5.

- **Runtime remote control (JSON-RPC 2.0 over Unix socket):** `internal/rpc`
  (new). A Unix-socket server speaks newline-delimited JSON-RPC 2.0:
  `system.ping`/`system.status`, `scene.get`/`names`/`load`/`switch`/`save`,
  `layer.add`/`remove`/`set`, `filter.add`/`remove`/`set`. A `Controller`
  owns the scene state shared with SIGHUP reloads; every mutation is a
  transaction: clone → transform → re-validate through `scene.Parse` →
  `scene.Diff` → apply ops one at a time (remove → update → add), with
  automatic rollback of the already-applied ops (inverted, in reverse) on
  any failure. Enabled by `--rpc-sock <path>` (flag) or `[control]
rpck_path` (config); no socket when unset. `scene.Op` restructured to
  carry the full `Layer` (previously add-ops in a rebuild re-created layers
  without their filters — a latent bug). `scene.Diff` rebuild condition
  widened: type/source/text/text-options/filters changes all emit
  remove+add (chain-affecting), plain pad-property changes stay in-place
  updates — previously a source/text change emitted a pad-only update that
  silently no-oped. `scripts/test-obs.sh` now rebuilds the binary when any
  Go source is newer than it (stale-binary guard — the cause of the
  "filters not applied" report). Docker-verified: filter chain props
  (videocrop top/bottom=100, videobalance brightness/saturation, rotate
  angle=pi/4) land on the live elements of the test-obs scene. Docs:
  `RPC.md` (new), README remote-control section, `ctrlirl_coder.conf.example`
  `[control]`.

- **Filter chain preconvert (black-screen fix):** a `videoconvert`
  (`<id>-fconv`) is now inserted in front of every non-empty video filter
  list (`layer.buildFilters`). Decoders emit I420/Y420; `rotate` (and the
  RGB-family-only parts of other bad-plugin filters) do not accept them,
  so the source→filter link failed with a caps mismatch. The failed link
  left the layer's compositor pad buffer-less, and the compositor — which
  needs a frame on every pad before it emits — went fully black (whole
  scene, not just the layer). Verified in Docker: I420→rotate link fails,
  I420→videoconvert→rotate renders. Docs: `FILTERS.md` chain-layout note.

- **`videorotate` transparent corners (black-background fix):** the
  `rotate` element's `geometrictransform` base clears its output frame with
  `memset(0, ...)` before mapping each pixel. That clear is transparent
  only when the output format carries an alpha channel — in an opaque
  format (RGB/BGR, the one the preconvert negotiates) it yields solid black
  in the un-covered corners/edges after a rotation, so a rotated overlay
  composited as a black box. `layer.buildFilters` now pins `rotate`'s
  output to BGRA with a `capsfilter` (`<id>-fNN-alpha`) right after it, so
  the un-mapped area is alpha=0. A trailing `videoconvert`
  (`<id>-fNN-postconv`) follows only when a later filter in the list needs
  the format back (YUV-only filters such as `effectv` drop the alpha, so
  corners stay black in that order). Verified by
  `TestRotateTransparentCorners`: a non-square source rotated 90° through
  the real `buildFilters` chain yields alpha=0 at all four corners and an
  opaque center; without the BGRA pin the corners read as non-zero
  (opaque). Docs: `FILTERS.md` `videorotate` row.

- **Scene-switch shutdown hang (SIGINT ignored after switching away from a
  live layer):** `RemoveLayer` froze the pipeline to READY for the duration
  of the teardown and restored PLAYING afterwards. Root cause (gdb-free
  SIGQUIT dump): a per-element `gst_element_set_state(NULL)` on a live
  `uridecodebin` (HLS) source blocked forever in C draining its in-flight
  network read; the stuck call owns the single command goroutine, so the
  SIGINT `cp.Stop()` queued behind it never ran. Repro: register scene a
  (HLS+browser) + b (2 testsrc), switch to b, back to a, SIGINT → hang.
  After the fix the same sequence exits cleanly (3/3).

- **RPC `scene.load` register-only mode:** `"switch": false` stores the
  scene under `name` (`rpc.Controller.Register`) for later
  `scene.switch` without diffing the running scene (result
  `"registered"`, zero applier ops). `scripts/test-obs-interactive.ts`
  uses it to pre-register both scenes, then the **space** key toggles
  `scene.switch` between the full test scene and a two-test-video scene;
  arrow keys move the `hls` layer, PgUp/PgDn step its `videorotate`
  filter ±15° via `filter.set`. Docs: `RPC.md` scene methods.

- **Viewer lagged one press behind on scene switch — app side was
  never the cause:** wall-clock measurement (app SRT caller → ffmpeg
  SRT _listener_ viewer, signalstats YAVG per decoded frame) showed the
  new scene's content on the wire **33 ms** after `scene.switch`
  (compositor pad swap is one frame; x264 IDR on switch; `appsink
diag` bytes linear through the switch, no stall). The 0.7–1.5 s
  "lag" lived in the _viewer_: an SRT listener that starts before the
  caller connects absorbs ~5 s of stream at connect and drains it
  slightly faster than realtime, so every switch made in that window
  lands ~1 s late (and 0.2 s once caught up). GOP size was ruled out
  (key-int-max 60/30/10 all measured identical; captured TS has an IDR
  at the switch frame, zero PTS gaps). Two app-side changes stayed
  because they are still correct: (1) `scene.Batcher`
  (`SuspendStream`/`ResumeStream`) so a multi-op `scene.load`/switch
  freezes the pipeline to READY **once** for the whole batch instead of
  once per removed layer — one re-preroll, not N; (2) the interactive
  script now logs rejections from the RPC socket (`rpc rejected: …`)
  instead of silently ignoring all responses. A `CTRLIRL_NO_FREEZE=1`
  env var disables the READY freeze for diagnostics. Real-viewer
  (OBS) switching latency is the viewer's own input buffering (~0.2 s
  steady-state), not the app.
- **2026-11-22 (REFACTOR.md complete):** Executed the multi-phase refactor
  (commits `f3be79b..8d8d773`): package reorganization
  (`internal/layer`, `internal/compositor`, `internal/scene`, `internal/rpc`,
  `internal/gstutil`), Chain-based layer pipeline with the
  `DynamicSourceIndex` design, SRT write path, control-loop, browser frame
  path, and cmd cleanups. **No more build-tag split** — the GStreamer
  packages are plain cgo and the whole repo builds/tests with GStreamer dev
  libraries (SPECS §7.7, README updated). Bug fixes folded in: A, B, D, E,
  F, G, H (see REFACTOR.md "Visible behavior changes") plus the
  `Chain.DynamicSourceIndex` zero-value regression (commit `8d8d773`): the
  index's zero value (`0`) made `LinkChain` skip the first link of every
  static chain, so layered pipelines produced 0 frames; static chains now
  set `-1` explicitly, with a regression test
  (`internal/layer/chain_test.go`). Bug I (Pulse null-sink leak on
  browser-layer teardown) is reported, not fixed.

- **2026-11-23 (TypeScript bindings — §13 complete):** Added
  `bindings/typescript` (`@ctrlirl/ctrlirl-coder`), a Bun/Node package mirroring
  the Go backend: Zod schemas for the TOML config and scene JSON, exact TOML/JSON
  serializers (`buildCtrlIRLCoderConfig`/`buildScene`), `buildCtrlIRLCoderArgs` (long
  flags, no shell), `PipelineBuilder` (jetson/rk3588/n100/generic ×
  test/v4l2/file, `venc_bps`/`venc_kbps` + `appsink name=appsink` contract with
  `validateContract`), `SceneBuilder` (Go geometry/zorder invariants), and
  `spawnCtrlIRLCoder` (Bun-first spawn with Node fallback, SIGHUP/SIGTERM, streamed
  stdout/stderr). 34 `bun test` cases green — including a dry-run round-trip
  against the real compiled Go binary — with a clean `tsc --noEmit`; new CI
  `bindings` job (oven-sh/setup-bun + frozen-lockfile install + test +
  typecheck). The spec §13 text is partially stale; where it disagrees with the
  Go backend the _backend wins_ (documented in the README): long CLI flags,
  TOML table layout per `internal/config`, text layers via `text` +
  `text-options`, and `CTRLIRL_LOG_LEVEL` env instead of a flag.

- **2026-11-23 (SRTLA session and OBS browser test):** Replaced the fabricated SRT data packets
  with a real SRT client bridged through an ephemeral loopback UDP relay. SRT handles handshake,
  feedback and retransmission; SRTLA schedules intact datagrams only on registered uplinks.
  Receiver timeouts now depend on incoming traffic, feedback is accepted only from the active
  receiver address, NAK range expansion is bounded, shutdown serializes against housekeeping,
  and reload resolves the new destination and preserves existing uplinks when setup fails.
  Added real-SRT
  tests covering single/bonded links, reload, encrypted sessions, stream IDs, retransmission
  after packet loss, and the reference receiver. The interactive browser test targets OBS
  SRTLA on port 5000, checks session and packet counters, and requires operator confirmation
  of video and audio. The live OBS test passed with operator confirmation of both videos and
  tones. An OBS source monitor measured 440 Hz and 880 Hz after switching layers and verified
  the volume control; the hidden layer's volume filter mutes its audio. A final smoke run
  after receiver-address and shutdown hardening also passed its transport checks.

- **2026-11-23 (sinks refactor + `sink` base + direct-SRT SRTLA + `sink` selector; superseded by the session fix above):**
  Restructured the output layer under `internal/sinks/`. Moved `internal/srt` →
  `internal/sinks/srt` and `internal/srtla` → `internal/sinks/srtla` (git mv, history
  preserved). Extracted a **cgo-free base package `internal/sinks`** defining the
  shared contracts — `Stats` (+`IsValid`), `Sink` (`Write`/`Stats`/`Close`),
  `StatsSource`, `Applier`, the `EncoderUnits`/`BitratePropertyValue` unit helpers, and
  `MockStatsSource`/`MockProfile` — so `internal/config`, `internal/balancer` and
  `internal/controller` depend only on `sink`, never the cgo `srt` package (they stay
  `CGO_ENABLED=0`-buildable). Both `sinks/srt.Client` and `sinks/srtla.Sender` now
  implement `sink.Sink` and return `sink.Stats` directly; the old per-sink `srtla.Summary`
  struct and its srtla→srt boundary mapping are gone. Re-architected the **SRTLA sender to
  be a direct sink** (no `listen_addr` UDP proxy): `Write` SRT-packetizes the payload
  (monotonic sequence, 1316-byte chunks via `BuildDataPacket`) and fans each packet to the
  best uplink, a 4096-slot retransmit ring backs NAK re-sends, and SRT control packets are
  consumed for their congestion effect (no local SRT peer). Added the top-level
  **`sink = "srt"|"srtla"` selector** to `internal/config` (default `srt`, validated),
  `config.toml`, `ceracoder.conf.example` and the TS bindings (`sink` field on
  `CtrlIRLCoderConfig` + `[srtla]` emitted only when `sink = "srtla"`); `enabled`/
  `listen_addr` are removed. `cmd/ctrlirl-coder/run_gst.go` builds the chosen `sink.Sink`
  per `cfg.Sink` and wires `puller.Run(ctx, sink.Write)`; `startController` now takes a
  `sink.StatsSource` (sink-agnostic). Full `go build ./...`, `go vet`, `make test`
  (`CGO_ENABLED=1`, race-clean) and the cgo-free subset under `CGO_ENABLED=0` green;
  `bun test` + `tsc --noEmit` green.

- **2026-11-23 (SRTLA — §14 protocol/config/bindings):** Started the SRTLA
  sender (spec §14). Derived the **authoritative wire format from the reference
  implementation** (`irlserver/srtla`, forked from BELABOX) and recorded the
  deviation from the spec's §14.3.1 sketch (no 8-byte header; bare SRT data
  packets; REG types `0x9200/01/02`; 256-byte group ID; legacy 2-byte +
  extended 38-byte keepalives) — see §4 item 7. Added `internal/sinks/srtla`
  (`protocol.go` pack/parse for type/seq, REG1/2/3, legacy + extended
  keepalives, SRTLA ACK, SRT ACK/NAK loss lists; `config.go` sender `Config` +
  `Normalized`) with 8 unit tests pinning the exact byte layout — all green.
  Wired the `[srtla]` table through `internal/config` (`SRTLAParams` +
  `ToSRTLAConfig`, defaults + enabled-only validation) and documented it in
  both `config.toml` and `ceracoder.conf.example`. Mirrored it in the TS
  bindings (`SrtlaSchema` + `serializeCtrlIRLCoderConfig` emits `[srtla]`
  only when enabled) — 37 `bun test` cases + `tsc --noEmit` green. Sender
  (UDP proxy + schedulers), `Controller` wiring and `AggregateStats` remain.

- **2026-11-23 (SRTLA — §14 sender core):** Built the `Sender` and its
  schedulers on top of the Phase-1 protocol. `uplink.go` is a faithful port of
  the reference `conn_t`: per-uplink UDP socket bound to the source IP,
  `window`/`in_flight`/256-slot sequence ring, NAK window shrink (−100, floor
  1), SRTLA per-packet ACK drain (+window growth), cumulative SRT ACK
  (`sn < ack` clears the ring), and a 1s rolling NAK-burst counter for the
  enhanced penalty. `scheduler.go` adds the `Scheduler` interface with a
  `classicScheduler` (reference's `window/(inFlight+1)`, skip timed-out) and an
  `enhancedScheduler` (spec §14.3.3: RTT/loss/NAK-burst/stall terms × the
  classic ratio, 10% score hysteresis, 50% stall floor, EWMA-smoothed weight).
  `sender.go` wires the UDP listener (fan-out of SRT data to the best uplink,
  forward SRT ACK/NAK/data back to the local client), the NGP→REG1→REG2→REG3
  registration handshake (group-ID generation + REG2 broadcast), the 1s
  housekeeping (keepalives, timeout re-registration, 10s global failover over
  the resolved receiver chain), and `stats.go` (`Sender.Summary()` — EWMA
  send/NAK rates, loss %, avg uplink RTT, in-flight send-buffer bytes), kept
  cgo-free per §4 item 8. Unit + e2e coverage: registration handshake,
  REG2-ID-mismatch refusal, window math, NAK/ACK drain, classic/enhanced
  selection, `Summary` fields, and a full `Run()` UDP data-passthrough test —
  all race-clean, `CGO_ENABLED=0` build + tests green. `Controller` wiring and
  a `srtla_rec` integration test remain.

- **2026-11-25 (Browser layer uses the CDP screencast stream; fixes choppy
  animation):** The old capture path polled `Page.captureScreenshot` at most
  once per output frame, and Chrome 156 headless has a hard ~50 ms floor per
  1080p PNG capture (measured: 50.0 ms p50, unchanged with
  `optimizeForSpeed`/`fromSurface`) — a ~20 fps ceiling, so animated browser
  layers played at ~20 fps even on a 60 fps scene. Probing this build proved
  the old assumption in `chrome.go` wrong: `Page.startScreencast` IS alive —
  60–240 fps streams (240 fps PNG at 1080p, ~45 KB/frame) with the alpha
  channel preserved (RGBA PNG) when the transparent background override from
  §2026-11-23 is set. The browser feeder now consumes the screencast stream
  as its frame source: `cdpsession` gains `startScreencast`/
  `stopScreencast`/`SetStreamHandler` and `newCDPSession` waits for
  `Page.loadEventFired` before returning (Chrome 156 delivers zero screencast
  frames if the stream starts before the page paints — measured 0 fps
  immediate vs 60.0 fps after settle). Two Chrome quirks bit: the session
  must be attached to the browser target's page (already the case) and the
  frame ack must NOT be sent through the blocking `call()` from the read
  loop — it deadlocks the socket (the ack's response can only be read by the
  very loop that is blocked on it), so the ack is a fire-and-forget write and
  its response is dropped by the read loop. The old ≤fps `captureScreenshot`
  poll is kept as an automatic fallback: a 1.5 s stream-silence watchdog
  tears the stream down and starts the ≤fps poll (plus a 2 s tail-freeze
  wake that also covers static pages where the stream idles), and a stream
  that stalls mid-show recovers the same way. The push loop now dedupes
  identical consecutive frames (static page → no redundant buffers, 0-byte
  tail is a no-op instead of a stall). `TestBrowser60FPSFeedSmooth` now runs
  with the screencast path active (stream=true); it failed at ~20 fps tail
  under the old polling and passes with a ≥48 fps tail and zero >1 s gaps.
  `scripts/test-obs-browser.ts` unchanged — the 60 fps ball.html animation
  now streams at 60 fps to OBS instead of ~20.

- **2026-11-25 (Browser layer: stream stays the sole source; silence ≠
  failure):** The screencast commit's two watchdogs misread a *static* page
  as a broken stream and tore the feed apart. The screencast stream only
  sends frames on repaint, so any page that sat static for ≥1.5 s (a title
  card, an idle donation overlay) tripped them: the tail-flow probe woke the
  `captureScreenshot` poll to keep the tail live, and once woken that poll
  ran *forever alongside the working stream* (no return path); the stream
  watchdog also permanently downgraded to polling `1.5 s after the last
  frame`. Two frame sources racing on one `latestFrame` box took the 1080p
  feed from 60 → ~45 fps with dozens of dropped animation frames after the
  page resumed (measured: 75 skipped in 5 s; when only the ~50ms poll won,
  ~19 fps). Fix in `runBrowser` (`internal/layer/browser/browser.go`):
  - **The stream is the sole frame source for the layer's lifetime.** The
    watchdog is now *startup-only*: it fires only if the stream delivers no
    frame within the 1.5 s window (a Chrome build where `Page.startScreencast`
    is genuinely dead) and then falls back to polling; the first frame
    disarms it. A silent stream from then on is a static page, not a fault
    (silence is indistinguishable from a dead-by-other-cause stream — the
    CDP socket liveness is the only real signal, and that → `sess.Stopped()`
    → the layer exits, which is a scene-level event, not a capture fallback).
  - **The tail-flow probe and its poll wake are gone** (no dead code kept):
    they existed only to re-capture a static page, which the always-push
    ticker now makes unnecessary.
  - **The push ticker re-pushes the freshest frame every tick, even on a
    static page** (the `bytes.Equal` dedupe is removed). A live SRT stream
    dies if a (single) source layer goes silent, so the compositor/encoder
    must always see a steady `1/fps`. Re-pushing costs only a decode of the
    same PNG and is the same cost as the pre-screencast polling — no
    regression, but pipeline liveness is guaranteed (a single-layer scene
    like `test-obs-browser.ts` stalls the whole compositor if the browser
    chain goes quiet).
  - **appsrc is now single-frame**: `max-buffers=1` + `leaky-type=downstream`
    (drop-oldest on overflow), replacing `block=false`+`max-bytes` which does
    *not* drop (it just fails the push). Freshest frame always wins; no
    backlog, 1-frame latency. Set via `gstutil.SetNumericProperty` (the
    enum routes through `g_value_set_enum`).
  Regression tests (`browser_fps_test.go`) now run at the true 1080p60
  resolution and assert *motion continuity* — a 16-bit frame counter painted
  into the page is read out of each decoded tail buffer, so a *skipped*
  animation frame (id jumps >1) and a *repeated* one (id unchanged) are
  caught, not just a buffer-count drop. `TestBrowser60FPSFeedSmooth`
  (continuous) and the new `TestBrowser60FPSRecoversAfterStatic` (the page
  renders 1.5 s, holds 3 s, resumes; the measurement window is pure resumed
  animation) both assert ≥80% of 60fps, no >1 s gap, and <5% dropped motion
  frames. Both pass at 60.2 fps, 0 skipped, 0 repeated (the pre-fix code
  failed the resume case at ~45fps / 75 skips).
