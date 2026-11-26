# Plan: optimize, refactor, deduplicate ctrlirl-coder

## Problem

~6.5k lines of non-test Go (cgo: GStreamer + libsrt). Code works (build, vet,
`go test ./...`, `go test -race ./...` all green at baseline) but:

- Hot paths copy and allocate more than needed (appsink→SRT, browser frames).
- A few per-buffer / per-tick operations do redundant work.
- Heavy duplication: RPC handlers, compositor attach/rollback, layer builders,
  cgo helpers, op dispatch in `scene` vs `rpc`.
- Dead code, stale comments (srtsink / build tags era), 8 files not gofmt'd.
- No benchmarks, no compositor tests → nothing to prove a change is safe/faster.

## Approach

Measure first, then change, then measure again. Characterization tests go in
before the code they protect is touched. One commit per todo so every step is
revertable and bisectable.

## Decisions (confirmed with user)

| Topic            | Decision                                                                                              |
| ---------------- | ----------------------------------------------------------------------------------------------------- |
| Existing WIP     | Commit as checkpoint on `main` first, then one commit per todo. No push.                              |
| Bugs found       | Fix A–H. Report I (Pulse sink leak / sink-input column) only — needs a live audio session to confirm. |
| Hot path         | Zero-copy appsink→SRT (`unsafe.Slice` over mapped `GstBuffer`).                                       |
| External surface | Not frozen. Changes limited to the list in "Visible behavior changes".                                |
| Websocket dep    | Swap `nhooyr.io/websocket` → `github.com/coder/websocket`.                                            |

## Ground rules

- Re-read `git status` before the checkpoint commit: the tree changed during
  planning (`scripts/.dbg-tob.sh` appeared, then vanished). Checkpoint includes
  tracked modifications + `internal/gstretstamp/` (the build needs it). Any
  scratch/debug script stays untracked.
- Log lines that scripts grep stay verbatim: `pipeline playing`,
  `srt: connected`, `appsink diag` + `frames=`, `scene: layer attached` +
  `id=`, `rpc: applied`, `layer: text overlay`, `layer: browser overlay`,
  `shutting down`. No new `level=ERROR` lines on healthy runs (scripts fail on
  `CRITICAL|ERROR`).
- Balancer constants and decision logic in `adaptive.go` / `aimd.go` are the
  contract with the original ctrlirl_coder: identifiers may be renamed, values and
  branch order may not change.
- SPECS §7.1 holds: every GStreamer mutation stays on the compositor command
  goroutine.
- No flag, TOML key, scene JSON field or JSON-RPC method/param renames (nothing
  to gain; `rpck_path` is documented in five places and stays).

## Todos

### Phase 0 — safety net

1. **checkpoint-wip** — Commit current WIP as-is (`wip: checkpoint before refactor`).
2. **bench-baseline** — Add `Benchmark*` (`-benchmem`) for: `gstpull` pull→send
   loop (test pipeline `fakesrc sizetype=fixed ! appsink`), browser frame decode
   (CDP response → JPEG bytes) and `pushBuffer`, `scene` clone + `Diff`, `rpc`
   `modify` round trip, `srt` write-path bookkeeping (rate tracker + state read).
   Record `-count=8` output to session `files/bench-before.txt`.
3. **compositor-tests** — `internal/compositor` has zero tests. Add integration
   tests on a real pipeline (`compositor ! … ! appsink`, test-pattern layers):
   add / remove / update props / duplicate id / remove unknown / suspend-resume
   nesting / Stop idempotence / commands after Stop return `ErrStopped`.

### Phase 1 — mechanical hygiene (no behavior change)

4. **hygiene** —
    - `gofmt` the 8 unformatted files.
    - Remove dead code: `clampInt`, `normF`, `gstutil.Link`, unused
      `gstutil.Pipeline` wrappers (`Bus`, `GetByName*`, `Add`, `Remove`),
      `cdpsession.ResizeViewport`, `Puller.stallLogged`, `srtInitOnceMu`,
      `isRetryable` (always true) and its dead branch, `curScene` dead store,
      identical `os.Stat` if/else in `video_source.go`, `sanitizeProps` (cannot
      see int/uint after `splitNumericProps`), doubled `TrimPrefix("srt://")`,
      always-nil `err` returns (`pipelineContract`, `pushBuffer`, `Register`).
    - `minInt64`/`clampInt64` → builtin `min`/`max`; `ErrClosed` → `errors.New`.
    - Typos: `bitrade*` → `bitrate*`. staticcheck S1017.
    - Stale comments: srtsink / `SrtsinkStats` / `//go:build gstreamer` /
      "identity byte-tap" references that no longer match the code; duplicated
      doc block on `Client.Write`.
    - `nhooyr.io/websocket` → `github.com/coder/websocket` (`go mod tidy`).

### Phase 2 — hot paths

5. **gstpull-zero-copy** — appsink → SRT, per buffer today: 6 cgo calls, 1 alloc,
   1 memcpy.
    - One C call pulls + maps and fills a small frame struct (sample, data, size,
      pts); one C call unmaps + unrefs. Go hands
      `unsafe.Slice(data, size)` straight to `send`. Result: 2 cgo calls, 0
      allocs, 0 copies.
    - `Send` contract documented: callee must not retain `b` after return.
      Verify in the srtgo source that `Write` copies synchronously
      (`srt_sendmsg2`); if it does not, fall back to one reused grow-only Go
      buffer (0 alloc, 1 copy).
    - Bug C: framerate caps lookup currently repeats on every buffer whenever
      caps carry no framerate (mpegts terminal — the normal case). Try on the
      first few buffers, then stop.
    - Cache the raw sink pointer once; `Run` works on a local snapshot so
      `Detach` no longer races it. Window counters become plain fields owned by
      the `Run` goroutine; only published values stay atomic.
6. **srt-write-path** — per buffer today: 2 mutex lock/unlock pairs + `time.Now()`.
    - Live socket behind `atomic.Pointer` (nil = down); lifecycle transitions
      (`Dial`, `markDown`, `Close`) keep the mutex.
    - `sendRateTracker.bytes` → `atomic.Uint64`.
    - Retry deadline computed only on the first transient error.
    - One `backoff(attempt)` helper shared by `Start` and `housekeeping`.
    - Bug F: pass `[srt] max_bandwidth_kbps` through to `ClientConfig`; convert
      to the unit `SRTO_MAXBW` expects (bytes/s) after confirming in srtgo.
7. **browser-frame-path**
    - 7.1. Dealbreaker: JPEG Has No Transparency (Alpha Channel)
      Your video chain uses appsrc(image/jpeg) ! jpegdec ! ... ! capsfilter(BGRA).
      • The problem: JPEG does not support an alpha channel. Even if videoconvert converts it to BGRA, every pixel will have alpha = 255 (100% opaque). When you composite this layer over your main stream, the webpage's background will completely cover the underlying video instead of letting transparent donation alerts float on top.
      • The fix: Switch Page.captureScreenshot format from "jpeg" to "png" (with omitBackground: true in CDP Emulation.setDefaultBackgroundColorOverride so the page background is transparent), and replace jpegdec with pngdec.
      • Performance note: PNG encoding/decoding is heavier on CPU than JPEG, so keep an eye on CPU usage at 30fps, or drop the alert layer to 15–24 fps if needed.
    - 7.2. Audio Routing Race Condition (routeAudio)
      Donation alerts only emit audio sporadically (when someone donates).
      • The problem: In Chromium/PipeWire, a browser tab often does not create a PulseAudio sink-input stream until it actually plays sound for the first time. Your routeAudio() function polls for 15 seconds right after launch and then gives up. If no donation happens in the first 15 seconds, Chrome's audio stream won't exist yet—and when an alert finally triggers 20 minutes later, it will route to the default audio device instead of your ctrlirl* null sink.
      • The fix: 1. Launch Chrome with PULSE_SINK=ctrlirl*<id> in its environment variables (cmd.Env)—PulseAudio and PipeWire-Pulse both honor PULSE_SINK at the process level, which routes all future streams from that Chrome instance automatically without needing pactl polling. 2. Keep routeAudio() running continuously on a slower ticker (or subscribing to pactl subscribe events) as a fallback rather than timing out after 15 seconds.
    - 7.3. Page.captureScreenshot Latency & Jitter
      • The problem: Polling Page.captureScreenshot over CDP WebSockets every 33ms (30fps) involves: Chromium compositor readback → PNG/JPEG encode → Base64 encode over JSON-RPC → Go Base64 decode → GStreamer decode. Under load, a screenshot call can take >33ms, causing overlapping requests or frame jitter.
      • The fix:
      • Ensure you do not pile up concurrent capture() calls if a single frame takes longer than interval (skip the tick if a capture is already in flight).
      • Alternatively, CDP's Page.startScreencast pushes frames asynchronously from Chromium only when the page actually repaints (saving massive CPU when no donation alert is currently animating on screen!). Note that if you use startScreencast, appsrc won't receive frames during idle periods, so your compositor/mixer must handle sparse input (or repeat the last frame in Go on the 33ms ticker).
    - 7.4. Autoplay & Headless Flags
      Make sure your newCDPSession launches Chrome with:
      • --autoplay-policy=no-user-gesture-required (so alert audio isn't blocked without a click).
      • --disable-background-timer-throttling and --disable-renderer-backgrounding (otherwise headless Chrome throttles CSS animations and JS timers after a few seconds, making donation alerts stutter or freeze).

— per frame today: websocket read → `map[string]any`
decode → base64 decode → `C.CBytes` → `GstBuffer` memcpy (5 frame-sized
allocations, ~30×/s per browser layer). - CDP envelope decodes id/error only; result decoded into a typed target
(`struct{ Data []byte }` — `encoding/json` base64-decodes in place). Page
events no longer get fully decoded. Benchmark picks between
`json.RawMessage` and two-pass decode. - `pushBuffer`: pass the Go slice directly (legal under cgo rules for the
duration of the call — same pattern `gstpull` already uses), allocate +
copy + push in one C call. Drops `C.CBytes` malloc/copy/free. - Typed push result instead of `(int, error)` with dead error branch. 8. **retstamp-cleanup** — Bug B: per-frame `g_printerr` → `GST_LOG_OBJECT` under
a debug category (free when disabled). Event function returns `gboolean`.
Register the src pad template with the class. 9. **control-loop** — - `compositor.SetBitrate`: skip the command round trip + `g_object_setv` when
the property value equals the last successfully applied one (controller
re-applies the same value every 500 ms today). - `SetOverlayText`: skip when text unchanged. - `UpdateLayerProps`: write only properties whose value changed. - `Adaptive.Next`: guard the 11-attr `slog.Debug` with `Enabled`. - `trackRTT`: single-writer → plain atomic stores, no CAS loops. - Bug D: overlay loop keeps running (placeholder text) on
`ErrNotConnected`; exits only on `ErrClosed` / ctx.

### Phase 3 — deduplicate and restructure

10. **gstutil-shared** — One home for binding workarounds:
    `gstutil.Ptr(el)` (duplicated `elementPointer` in `gstpull` + `layer`),
    `gstutil.SetNumericProperty` (moves out of `layer/browser_cgo.go`; the
    compositor's encoder bitrate setter should not import a browser file),
    `gstutil.IsFactoryType(el, factory)` (replaces identical `isAppsink` /
    `isTextOverlay`).
11. **compositor-refactor** —
    - `do` = `doCtx(context.Background(), …)`; result wait also honors `doneCh`.
    - `checkContract`: table of optional named elements instead of repeated
      lookup/log blocks; single `appsink` lookup.
    - One `attachChain(bin, chain, target, padTemplate)` used for both the video
      chain (compositor `sink_%u`) and audio chain (mux `sink_%d`), with a single
      rollback path instead of hand-repeated cleanup in each error branch.
    - Bug E: duplicate-id check moves onto the command goroutine and runs
      before the chain is built; every failure path closes the layer.
    - Unexport `Command`.
12. **layer-refactor** —
    - Embedded base type for `ID` / `Type` / `DefaultProperties` / `Close`
      (repeated in 4 layer types).
    - `surfaceChain(l, head...)` = head + video filters + BGRA tail (repeated in
      4 builders); drop the `fail` closures that double the `layer:` prefix.
    - `Chain`: dynamic source stored as index; one `linkRange` helper instead of
      three copies of the link loop.
    - `makeElement`: single pass over props (3 maps → 1).
    - `buildFilters`: one registry lookup per spec, no shadowed `n`, element
      naming index made explicit.
    - Text options: converter funcs instead of five empty-struct types; sorted
      names in error messages.
    - `sourceChain`: drop the `"factory:name"` string encoding.
13. **scene-refactor** —
    - Typed constants for layer types and filter formats (string literals are
      scattered over `scene`, `layer`, `rpc`).
    - `(*Scene).Clone()` deep copy (preserves nil-vs-empty maps) and exported
      `Finalize` (normalize + validate).
    - `scene.ApplyOp` single op dispatch; `ApplyDiff` builds on it.
    - `maps.Equal` instead of `reflect.DeepEqual` (also stops a nil-vs-empty
      `text-options` map from forcing a chain rebuild).
    - `indexByID` nil check before dereference; `validate*` lose unused params;
      `EffectiveParams` computed once per filter; error location string built
      only on error; small typed accessors in registry `Check` funcs.
14. **rpc-refactor** —
    - Method table (`map[string]handler`) instead of the switch.
    - Generic `decode[T]`, `invalidParams`, and `edit(describe, tx)` helpers —
      removes the decode/validate/modify/wrap boilerplate repeated in 9
      handlers; `withLayer(id, fn)` for the 4 "find layer or error" copies.
    - Controller clones via `scene.Clone` instead of JSON marshal + full parse
      (today: 2 marshals + 2 parses per edit; 1 + 1 for `system.status`).
    - Bug A: `Load` unlock on the error path (`defer`), shared `resolveName`
      with `Register`.
    - Rollback ordering via stable sort by op kind (replaces three filter loops);
      uses `scene.ApplyOp`.
    - Bug G: `scene.save` with unknown name → error. `scene.names` sorted.
    - `json.Encoder` for responses; bounded request line length.
15. **cmd-refactor** —
    - Split the 220-line `run` into `attachScene`, `startRPC`, `startDataPath`,
      `logAppsinkStats`, `reload`, `signalLoop`.
    - Scene and pipeline text loaded once (today: scene parsed twice, pipeline
      file read twice).
    - Bug H: `--dry-run` validates the template whether it comes from
      `--pipeline` or `[pipeline] template`.
    - `pipelineContract`: ordered table (deterministic output), no map iteration.

### Phase 4 — verify and document

16. **verify** —
    - `make vet`, `make test`, `go test -race ./...`, staticcheck + deadcode clean.
    - Benchmarks again → `files/bench-after.txt`; `benchstat` before/after table
      in the final report. Any optimization without a measurable win gets
      reverted unless it is also a readability win.
    - `make test-integration` and `make test-stream` (gst-launch,
      srt-live-transmit, ffprobe are present on this machine).
    - `make test-obs` needs a running OBS → left for the user; called out in the
      report.
17. **docs** — Update only passages describing changed things: SPECS §5.2
    (gstutil helpers), §5.3 (attach flow), §7.7 (build-tag text is stale — single
    cgo build), README/RPC.md for `scene.save` error + sorted names,
    `ctrlirl_coder.conf.example` note for `max_bandwidth_kbps`. Final report lists
    bug I with reproduction steps.

## Visible behavior changes (complete list)

- `--dry-run` also checks a config-referenced pipeline template (H).
- `scene.save` unknown name → JSON-RPC error instead of a `null` file (G).
- `scene.names` / `system.status.scenes` sorted.
- Stats overlay shows the placeholder while SRT is down instead of freezing (D).
- `max_bandwidth_kbps` takes effect (F).
- Adding a layer with an existing id fails before anything is built (E).
- Some error strings lose a doubled `layer: layer:` prefix.
- retstamp no longer prints per-frame lines to stderr (B).

## Not doing

- Renaming `CompositorPipeline`, moving packages, changing flags/TOML/JSON/RPC names.
- Sharing test fakes across packages (3 small fakes, different interfaces).
- Bug I (Pulse) — reported only.
- SRTLA, TS bindings, `ctrlirl_coder/`, shell scripts.

## Risks

| Risk                                                        | Mitigation                                                                    |
| ----------------------------------------------------------- | ----------------------------------------------------------------------------- |
| Zero-copy slice outlives the mapped buffer                  | `Send` contract + srtgo source check + race tests; fallback to reused buffer. |
| Compositor attach/rollback regression (previously untested) | Todo 3 tests land before todo 11.                                             |
| Skip-if-unchanged bitrate hides a failed set                | Cache updated only after a successful set.                                    |
| Scripts depend on log text                                  | Grepped lines frozen (see ground rules); integration scripts run in verify.   |
| User keeps editing the tree concurrently                    | Re-check `git status` before each commit; stop and ask on unexpected changes. |
