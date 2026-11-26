import { describe, expect, test } from "bun:test";
import { rmSync } from "node:fs";
import { PipelineBuilder, validateContract } from "../src/pipeline";
import type { Hardware, Source } from "../src/types";

const HARDWARES: Hardware[] = ["jetson", "rk3588", "n100", "generic"];
const SOURCES: Source[] = ["test", "v4l2", "file"];

describe("PipelineBuilder", () => {
    test("lists hardware types and sources", () => {
        expect(PipelineBuilder.listHardwareTypes()).toEqual(HARDWARES);
        for (const hw of HARDWARES) {
            expect(PipelineBuilder.listSources(hw)).toEqual(SOURCES);
        }
    });

    test("every hardware x source combination satisfies the contract", () => {
        for (const hw of HARDWARES) {
            for (const src of SOURCES) {
                const { pipeline } = PipelineBuilder.build({
                    hardware: hw,
                    source: src,
                });
                // The contract guard must not throw.
                validateContract(pipeline);
                expect(pipeline).toMatch(/\bappsink\s+name=appsink\b/);
                expect(
                    /\bvenc_bps\b/.test(pipeline) ||
                        /\bvenc_kbps\b/.test(pipeline),
                ).toBe(true);
                // No in-pipeline SRT sink (the Go process owns the socket).
                expect(pipeline).not.toContain("srtsink");
            }
        }
    });

    test("overrides apply resolution and framerate", () => {
        const { pipeline } = PipelineBuilder.build({
            hardware: "generic",
            source: "test",
            overrides: { resolution: "1920x1080", framerate: 60 },
        });
        expect(pipeline).toContain("width=1920,height=1080");
        expect(pipeline).toContain("framerate=60/1");
    });

    test("hardware-specific encoders are used", () => {
        expect(PipelineBuilder.build({ hardware: "jetson", source: "test" }).pipeline).toContain("nvenc.h264");
        expect(PipelineBuilder.build({ hardware: "rk3588", source: "test" }).pipeline).toContain("mpph264enc");
        expect(PipelineBuilder.build({ hardware: "n100", source: "test" }).pipeline).toContain("qsvenc");
        expect(PipelineBuilder.build({ hardware: "generic", source: "test" }).pipeline).toContain("x264enc");
    });

    test("encoder carries the unit contract in its name", () => {
        // venc_bps => bitrate expressed in bps
        const jetson = PipelineBuilder.build({ hardware: "jetson", source: "test" }).pipeline;
        expect(jetson).toMatch(/venc-bps=\d+ name=venc_bps/);
        // venc_kbps => bitrate expressed in kbit/s
        const generic = PipelineBuilder.build({ hardware: "generic", source: "test" }).pipeline;
        expect(generic).toMatch(/bitrate=\d+ key-int-max=30 name=venc_kbps/);
    });

    test("writeTo writes the file and returns the path", async () => {
        const path = `${import.meta.dir}/../.tmp/pipeline-writeto.pipeline`;
        const { path: p2 } = PipelineBuilder.build({
            hardware: "generic",
            source: "test",
            writeTo: path,
        });
        expect(p2).toBe(path);
        const f = await Bun.file(path);
        expect(await f.text()).toMatch(/appsink name=appsink/);
        rmSync(path);
    });

    test("rejects unknown hardware", () => {
        // Intentionally outside the Hardware union — the runtime guard is
        // what we're exercising.
        const raspi = "raspi" as unknown as Hardware;
        expect(() =>
            PipelineBuilder.build({ hardware: raspi, source: "test" }),
        ).toThrow("unknown hardware");
        expect(() => PipelineBuilder.listSources(raspi)).toThrow(
            "unknown hardware",
        );
    });

    test("v4l2 and file sources are video-only (no audio chain)", () => {
        for (const src of ["v4l2", "file"] as Source[]) {
            const { pipeline } = PipelineBuilder.build({
                hardware: "generic",
                source: src,
            });
            expect(pipeline).not.toContain("audiotestsrc");
            expect(pipeline).not.toContain("voaacenc");
        }
        const { pipeline } = PipelineBuilder.build({
            hardware: "generic",
            source: "test",
        });
        expect(pipeline).toContain("audiotestsrc");
    });
});

describe("validateContract", () => {
    test("throws on missing terminal appsink", () => {
        expect(() =>
            validateContract(
                "videotestsrc ! x264enc name=venc_kbps ! fakesink",
            ),
        ).toThrow("missing terminal `appsink name=appsink`");
    });

    test("throws on missing venc_bps/venc_kbps", () => {
        expect(() =>
            validateContract(
                "videotestsrc ! x264enc ! mpegtsmux ! appsink name=appsink",
            ),
        ).toThrow("missing venc_bps/venc_kbps");
    });

    test("accepts a valid pipeline", () => {
        expect(() =>
            validateContract(
                "videotestsrc ! x264enc bitrate=6000 name=venc_kbps ! mpegtsmux name=mux ! appsink name=appsink",
            ),
        ).not.toThrow();
    });
});
