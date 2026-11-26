import { describe, expect, test } from "bun:test";
import { rmSync } from "node:fs";
import { SceneBuilder } from "../src/scene";
import { SceneSchema } from "../src/schemas";

describe("SceneBuilder", () => {
    test("builds a two-layer scene matching scene.example.json shape", () => {
        const { scene, json } = new SceneBuilder()
            .setCanvas(1280, 720)
            .addVideoLayer({
                id: "main",
                source: "/dev/video0",
                x: 0,
                y: 0,
                w: 1280,
                h: 720,
                zorder: 1,
                filters: [
                    { kind: "videocrop", params: { top: 24, bottom: 24 } },
                    {
                        kind: "videobalance",
                        params: { brightness: 0.05, saturation: 1.1 },
                    },
                ],
            })
            .addImageLayer({
                id: "logo",
                filePath: "logo.png",
                x: 16,
                y: 16,
                w: 128,
                h: 128,
                zorder: 10,
                alpha: 0.9,
            })
            .build();

        // Must validate against the schema (Go scene.Load parity).
        SceneSchema.parse(scene);
        expect(scene.name).toBe("main");
        expect(scene.canvas).toEqual({ width: 1280, height: 720 });
        expect(scene.layers).toHaveLength(2);
        expect(scene.layers[0].type).toBe("video");
        expect(scene.layers[0].filters).toHaveLength(2);
        // image source gets the file:// prefix
        expect(scene.layers[1].source).toBe("file://logo.png");
        expect(scene.layers[1].properties.alpha).toBe(0.9);
        // json is parseable
        expect(JSON.parse(json)).toEqual(scene);
    });

    test("text layer uses the `text` field (not a source prefix)", () => {
        const { scene } = new SceneBuilder()
            .addTextLayer({
                id: "ticker",
                text: "Hello CtrlIRL",
                x: 0,
                y: 600,
                w: 1280,
                h: 60,
                textOptions: { "font-desc": "Monospace 24", color: "#ffffff" },
            })
            .build();
        expect(scene.layers[0].type).toBe("text");
        expect(scene.layers[0].text).toBe("Hello CtrlIRL");
        expect(scene.layers[0]["text-options"]).toEqual({
            "font-desc": "Monospace 24",
            color: "#ffffff",
        });
    });

    test("browser layer carries url + framerate/quality", () => {
        const { scene } = new SceneBuilder()
            .addBrowserLayer({
                id: "web",
                url: "https://example.com",
                x: 640,
                y: 480,
                w: 640,
                h: 240,
                framerate: 30,
                quality: 75,
            })
            .build();
        expect(scene.layers[0].type).toBe("browser");
        expect(scene.layers[0].source).toBe("https://example.com");
        expect(scene.layers[0].properties.framerate).toBe(30);
        expect(scene.layers[0].properties.quality).toBe(75);
    });

    test("testcode layer is accepted by the Go-compatible scene schema", () => {
        const scene = SceneSchema.parse({
            name: "testcode",
            layers: [
                {
                    id: "timestamp",
                    type: "testcode",
                    source: "",
                    properties: { width: 1280, height: 720 },
                },
            ],
        });
        expect(scene.layers[0].type).toBe("testcode");
    });

    test("auto-assigns unique zorders when omitted", () => {
        const { scene } = new SceneBuilder()
            .addVideoLayer({ id: "a", source: "/dev/video0" })
            .addVideoLayer({ id: "b", source: "/dev/video1" })
            .addImageLayer({ id: "c", filePath: "x.png", x: 0, y: 0, w: 10, h: 10 })
            .build();
        const z = scene.layers.map((l) => l.properties.zorder);
        expect(new Set(z).size).toBe(3);
    });

    test("rejects layers overflowing the canvas", () => {
        expect(() =>
            new SceneBuilder()
                .setCanvas(1280, 720)
                .addImageLayer({
                    id: "big",
                    filePath: "big.png",
                    x: 1200,
                    y: 0,
                    w: 500,
                    h: 100,
                }),
        ).toThrow(/exceeds canvas/);
    });

    test("rejects negative geometry", () => {
        expect(() =>
            new SceneBuilder().addVideoLayer({
                id: "neg",
                source: "/dev/video0",
                x: -10,
            }),
        ).toThrow(/exceeds canvas/);
    });

    test("writeTo writes the scene JSON", async () => {
        const path = `${import.meta.dir}/../.tmp/scene-writeto.json`;
        const { scene } = await new SceneBuilder()
            .addVideoLayer({
                id: "main",
                source: "/dev/video0",
                w: 1280,
                h: 720,
            })
            .writeTo(path);
        const f = await Bun.file(path);
        const parsed = JSON.parse(await f.text());
        expect(parsed).toEqual(scene);
        expect(parsed).toEqual(
            expect.objectContaining({ name: "main", layers: [expect.anything()] }),
        );
        rmSync(path);
    });
});
