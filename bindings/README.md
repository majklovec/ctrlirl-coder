# @ctrlirl/ctrlirl-coder

TypeScript / Bun bindings for **CtrlIRLCoder-Go** — the GStreamer multi-layer
SRT streaming backend. Mirrors the original `@ceralive/ctrlirl_coder` API
one-to-one (same export names, compatible signatures) and adds
`SceneBuilder` for the multi-layer compositor. No shell wrappers: all
process control, config generation and signal delivery live in this
package.

- Package name: `@ctrlirl/ctrlirl-coder` (distinct from
  `@ceralive/ctrlirl_coder` so both backends coexist during migration).
- Runtime: Bun-first (`bun test`, `Bun.spawn`), Node >= 20 compatible
  (`child_process` fallback; `Bun` feature-detected at call time).

## Install & test

```sh
cd bindings/typescript
bun install
bun test          # unit + integration (Go binary round-trip if present)
bun run typecheck # strict tsc --noEmit
```

The integration test (`test/process.integration.test.ts`) accepts the
generated TOML config, scene JSON and pipeline template with
`ctrlirl-coder --dry-run` (exit 0) when
`bin/ctrlirl-coder-linux-amd64` exists, or when `CTRLIRL_CODER_BIN` points
at a binary.

## Quick start

```ts
import {
    buildCtrlIRLCoderConfig,
    SpawnOptions,
    spawnCtrlIRLCoder,
    writeConfig,
    writeScene,
    PipelineBuilder,
    SceneBuilder,
} from "@ctrlirl/ctrlirl-coder";

// 1) Config (TOML accepted by the Go config.Load() unchanged)
const { config, ini } = buildCtrlIRLCoderConfig({
    srt: { uri: "srt://127.0.0.1:9101", latency: 120 },
    general: { min_bitrate: 500, max_bitrate: 6000, balancer: "adaptive" },
});
await writeConfig("/tmp/ctrlirl_coder.toml", config); // writes the TOML

// 2) Pipeline (single-layer, original-style) or scene (multi-layer).
PipelineBuilder.build({
    hardware: "generic", // jetson | rk3588 | n100 | generic
    source: "test", // test | v4l2 | file
    writeTo: "/tmp/p.pipeline",
});
await new SceneBuilder()
    .setCanvas(1280, 720)
    .addVideoLayer({ id: "main", source: "/dev/video0", w: 1280, h: 720 })
    .addImageLayer({
        id: "logo",
        filePath: "logo.png",
        x: 16,
        y: 16,
        w: 128,
        h: 128,
        zorder: 10,
    })
    .addTextLayer({
        id: "ticker",
        text: "CtrlIRL",
        x: 0,
        y: 660,
        w: 1280,
        h: 60,
        zorder: 20,
    })
    .addBrowserLayer({
        id: "web",
        url: "https://example.com",
        x: 640,
        y: 480,
        w: 640,
        h: 240,
        zorder: 5,
    })
    .writeTo("/tmp/scene.json");

// 3) Process control (no shell)
const proc = spawnCtrlIRLCoder({
    binaryPath: "./bin/ctrlirl-coder-linux-amd64",
    configFile: "/tmp/ctrlirl_coder.toml",
    sceneFile: "/tmp/scene.json",
    onStderr: (line) => console.error(line),
});
proc.sendHup(); // SIGHUP: reload config + re-diff scene
proc.sendTerm(); // SIGTERM: graceful shutdown
await proc.wait();
```

## API

### `schemas.ts` — Zod schemas

`CtrlIRLCoderConfigSchema` (TOML config), `SceneSchema` (scene JSON),
`SceneLayerSchema`, `FilterSpecSchema`, `FILTER_KINDS`. These mirror the
Go backend's `internal/config` and `internal/scene` types exactly — same
tables, keys and constraints.

### `config.ts`

- `buildCtrlIRLCoderConfig(partial?) → { config, ini }` — validate + serialize.
- `serializeCtrlIRLCoderConfig(config) → string` — TOML matching the Go
  `[general]/[srt]/[adaptive]/[aimd]/[overlay]/[pipeline]/[scene]/[control]`
  tables. Optional keys are omitted so Go defaults apply.
- `buildScene(partial?) → { scene, json }`.

### `args.ts`

`buildCtrlIRLCoderArgs(opts) → string[]` — emits `--config`, `--pipeline`,
`--scene`, `--dry-run`, `--api-addr`. The legacy `-b` flag is never
emitted. If both `pipelineFile` and `sceneFile` are given, **`--scene`
takes precedence** (scene composition overrides a flat pipeline). Log
level is not a flag: set `CTRLIRL_LOG_LEVEL` via `SpawnOptions.env`.

### `pipeline.ts`

- `PipelineBuilder.build({ hardware, source, overrides?, device?, writeTo? })`
  — hardware-specific encoders (`nvenc.h264` / `mpph264enc` / `qsvenc` /
  `x264enc`), always ending in the mandatory `appsink name=appsink`
  terminal with an encoder named `venc_bps`/`venc_kbps` (the contract the
  Go adaptive balancer discovers by name). Validated before return —
  throws on contract violations.
- `PipelineBuilder.listHardwareTypes()`,
  `PipelineBuilder.listSources(hardware)`.
- `validateContract(pipeline)` — the exported guard.

The pipeline carries **no SRT sink**: the Go process owns the SRT socket
and the destination comes from `[srt] uri` in the TOML config.

### `scene.ts`

`SceneBuilder` — `setCanvas`, `addVideoLayer`, `addImageLayer`,
`addTextLayer` (uses the scene `text` field + `text-options`),
`addBrowserLayer`, `build()`, `writeTo(path)`. Enforces the Go invariants
up front: unique auto-assigned zorders and no layer rect overflowing the
canvas.

### `process.ts`

- `spawnCtrlIRLCoder(opts) → CtrlIRLCoderProcess` — Bun-first (`Bun.spawn`),
  Node fallback (`child_process.spawn`); feature-detected at call time.
- `CtrlIRLCoderProcess`: `pid`, `kill(signal?)`, `sendHup()`, `sendTerm()`,
  `wait()`.
- `writeConfig(path, config)`, `writeScene(path, scene)`,
  `writeFile(path, content)`, `writeFileSync(path, content)`.

Binary resolution: `opts.binaryPath` → `$CTRLIRL_CODER_BIN` → `ctrlirl_coder-go`
on PATH.

## Scene (multi-layer)

Scene JSON is accepted by the Go `scene.Load()`. Layer types: `video`,
`image` (local path, `file://` added automatically), `text`
(`text` field + `text-options` pango options), `browser` (page capture
with optional `framerate`/`quality`). Per-layer `filters` from the Go
registry: `videoscale`, `videocrop`, `videobalance`, `videobox`,
`videoflip`, `videorotate`, `effectv` (video); `volume`, `audiodynamic`,
`pitch` (audio).

Canvas rule: no layer rect may extend past the canvas (layers without
explicit size are canvas-sized). Z-order rule: zorders must be unique;
omitted zorders get the next free value.

