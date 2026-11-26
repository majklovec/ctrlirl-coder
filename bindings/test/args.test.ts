import { describe, expect, test } from "bun:test";
import { buildCtrlIRLCoderArgs } from "../src/args";

describe("buildCtrlIRLCoderArgs", () => {
    test("empty options produce no args", () => {
        expect(buildCtrlIRLCoderArgs({})).toEqual([]);
    });

    test("emits the Go binary's long flags", () => {
        const args = buildCtrlIRLCoderArgs({
            configFile: "cfg.toml",
            pipelineFile: "p.pipeline",
            sceneFile: "s.json",
        });
        expect(args).toEqual([
            "--config",
            "cfg.toml",
            "--pipeline",
            "p.pipeline",
            "--scene",
            "s.json",
        ]);
    });

    test("never emits the legacy -b flag", () => {
        const args = buildCtrlIRLCoderArgs({
            configFile: "c",
            pipelineFile: "p",
            sceneFile: "s",
            dryRun: true,
            apiAddr: "127.0.0.1:8899",
        });
        for (const a of args) expect(a).not.toBe("-b");
        expect(args).toContain("--dry-run");
        expect(args).toEqual([
            "--config",
            "c",
            "--pipeline",
            "p",
            "--scene",
            "s",
            "--dry-run",
            "--api-addr",
            "127.0.0.1:8899",
        ]);
    });

    test("omitted options omit their flags", () => {
        const args = buildCtrlIRLCoderArgs({ sceneFile: "s.json" });
        expect(args).toEqual(["--scene", "s.json"]);
    });
});
