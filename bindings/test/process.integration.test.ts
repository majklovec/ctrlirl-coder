// Integration tests: real Go binary (dry-run round-trip) + signal
// lifecycle against a stub binary. Skipped when the Go binary is absent.

import { describe, expect, test } from "bun:test";
import { chmodSync, mkdirSync, writeFileSync } from "node:fs";
import { buildCtrlIRLCoderConfig } from "../src/config";
import { PipelineBuilder } from "../src/pipeline";
import {
    fileExists,
    spawnCtrlIRLCoder,
    writeConfig,
    writeScene,
} from "../src/process";
import { SceneBuilder } from "../src/scene";

const tmp = `${import.meta.dir}/../.tmp`;
const binPath =
    process.env.CTRLIRL_CODER_BIN ??
    `${import.meta.dir}/../../bin/ctrlirl-coder-linux-amd64`;
const haveBin = fileExists(binPath);

// Wait for a line in `out` (the child is up and its signal handlers are
// registered) or fail after ~5s. Plain sleep races CI's slow node start, so
// SIGHUP can land before the stub's handler exists (default action: exit).
async function waitForLine(
    out: string[],
    line: string,
    timeoutMs = 5000,
): Promise<void> {
    const start = Date.now();
    while (!out.includes(line)) {
        if (Date.now() - start > timeoutMs)
            throw new Error(`timed out waiting for "${line}" (got: ${out.join(" | ")})`);
        await Bun.sleep(20);
    }
}

function stubPath(): string {
    const p = `${tmp}/signal-stub.mjs`;
    writeFileSync(
        p,
        [
            "#!/usr/bin/env node",
            'console.log("stub: up");',
            'console.log("stub: env=" + (process.env.STUB_ENV_PROBE ?? ""));',
            'process.on("SIGHUP", () => console.log("stub: hup"));',
            'process.on("SIGTERM", () => { console.log("stub: term"); process.exit(0); });',
            "setInterval(() => {}, 1000);",
            "",
        ].join("\n"),
    );
    chmodSync(p, 0o755);
    return p;
}

describe("process control", () => {
    test("spawn + SIGHUP + SIGTERM lifecycle (stub binary)", async () => {
        mkdirSync(tmp, { recursive: true });
        const out: string[] = [];
        const proc = spawnCtrlIRLCoder({
            binaryPath: stubPath(),
            onStdout: (l) => out.push(l),
            onExit: (c) => {
                procExitCode = c;
            },
        });
        expect(proc.pid).toBeGreaterThan(0);
        // Wait until the stub is up and its signal handlers are registered.
        await waitForLine(out, "stub: up");
        proc.sendHup();
        await waitForLine(out, "stub: hup");
        proc.sendTerm();
        const code = await proc.wait();
        expect(code).toBe(0);
        expect(out).toContain("stub: up");
        expect(out).toContain("stub: hup");
        expect(out).toContain("stub: term");
        expect(procExitCode).toBe(0);
    });

    let procExitCode = -1;

    test("writeConfig + writeScene + spawnCtrlIRLCoder dry-run (Go binary)", async () => {
        if (!haveBin) return;
        mkdirSync(tmp, { recursive: true });

        const cfgPath = `${tmp}/int-cfg.toml`;
        const scenePath = `${tmp}/int-scene.json`;
        const { config, scene } = (() => {
            const { config } = buildCtrlIRLCoderConfig({
                srt: { uri: "srt://127.0.0.1:9101", latency: 120 },
                general: { min_bitrate: 500, max_bitrate: 6000 },
            });
            const { scene } = new SceneBuilder()
                .setCanvas(1280, 720)
                .addVideoLayer({
                    id: "main",
                    source: "/dev/video0",
                    w: 1280,
                    h: 720,
                    zorder: 1,
                })
                .addImageLayer({
                    id: "logo",
                    filePath: "logo.png",
                    x: 16,
                    y: 16,
                    w: 128,
                    h: 128,
                    zorder: 10,
                })
                .build();
            return { config, scene };
        })();

        await writeConfig(cfgPath, config);
        await writeScene(scenePath, scene);

        const pipelinePath = `${tmp}/int-pipeline.pipeline`;
        PipelineBuilder.build({
            hardware: "generic",
            source: "test",
            writeTo: pipelinePath,
        });

        const logs: string[] = [];
        const proc = spawnCtrlIRLCoder({
            binaryPath: binPath,
            configFile: cfgPath,
            pipelineFile: pipelinePath,
            sceneFile: scenePath,
            dryRun: true,
            onStdout: (l) => logs.push(l),
            onStderr: (l) => logs.push(l),
        });
        const code = await proc.wait();

        // Spec §13.12 #3/#4: generated TOML + scene JSON + pipeline must be
        // accepted by the Go backend with exit 0.
        expect(code).toBe(0);
        expect(logs.join("\n")).toContain("dry-run: OK");
    });

    test("env propagation reaches the child", async () => {
        mkdirSync(tmp, { recursive: true });
        const out: string[] = [];
        const proc = spawnCtrlIRLCoder({
            binaryPath: stubPath(),
            env: { STUB_ENV_PROBE: "hello" },
            onStdout: (l) => out.push(l),
        });
        // Wait for the stub's env probe line: proves env reached the child
        // AND its SIGTERM handler is registered before signalling.
        await waitForLine(out, "stub: env=hello");
        proc.sendTerm();
        const code = await proc.wait();
        expect(code).toBe(0);
        expect(out).toContain("stub: env=hello");
    });
});
