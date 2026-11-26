import { describe, expect, test } from "bun:test";
import {
    buildCtrlIRLCoderConfig,
    buildScene,
    serializeCtrlIRLCoderConfig,
} from "../src/config";
import { CtrlIRLCoderConfigSchema, SceneSchema } from "../src/schemas";

describe("buildCtrlIRLCoderConfig", () => {
    test("applies defaults from the Go config package", () => {
        const { config } = buildCtrlIRLCoderConfig();
        expect(config.general).toEqual({
            min_bitrate: 500,
            max_bitrate: 6000,
            balancer: "adaptive",
            frame_stall_ms: 3000,
        });
        expect(config.srt.uri).toBe("srt://127.0.0.1:9000");
        expect(config.srt.latency).toBe(2000);
        expect(config.aimd.decr_mult).toBe(0.5);
        expect(config.overlay.enabled).toBe(false);
    });

    test("emits TOML with the Go table layout", () => {
        const { ini } = buildCtrlIRLCoderConfig({
            general: { balancer: "aimd", max_bitrate: 12000 },
            srt: {
                uri: "srt://10.0.0.1:9101",
                latency: 120,
                streamid: "ctrlirl_coder",
                passphrase: 's3 "cr"',
            },
            overlay: { enabled: true },
            pipeline: { template: "pipeline/full.pipeline" },
        });

        expect(ini).toContain("[general]");
        expect(ini).toContain('balancer = "aimd"');
        expect(ini).toContain("max_bitrate = 12000");
        expect(ini).toContain("[srt]");
        expect(ini).toContain('uri = "srt://10.0.0.1:9101"');
        expect(ini).toContain("latency = 120");
        expect(ini).toContain('streamid = "ctrlirl_coder"');
        // TOML basic-string escaping for quotes.
        expect(ini).toContain('passphrase = "s3 \\"cr\\""');
        expect(ini).toContain("[overlay]");
        expect(ini).toContain("enabled = true");
        expect(ini).toContain("[pipeline]");
        expect(ini).toContain('template = "pipeline/full.pipeline"');
        // Optional sections stay out.
        expect(ini).not.toContain("[scene]");
        expect(ini).not.toContain("[control]");
    });

    test("omits optional srt keys", () => {
        const { ini } = buildCtrlIRLCoderConfig();
        expect(ini).not.toContain("streamid");
        expect(ini).not.toContain("passphrase");
        expect(ini).not.toContain("max_bandwidth_kbps");
    });

    test("default sink is srt; [srtla] omitted from TOML", () => {
        const { config, ini } = buildCtrlIRLCoderConfig();
        expect(config.sink).toBe("srt");
        expect(config.srtla.mode).toBe("enhanced");
        expect(config.srtla.uplink_ips).toEqual([]);
        expect(ini).toContain('sink = "srt"');
        expect(ini).not.toContain("[srtla]");
    });

    test("emits [srtla] section when sink = srtla", () => {
        const { config, ini } = buildCtrlIRLCoderConfig({
            sink: "srtla",
            srtla: {
                receiver_host: "srtla.example.com",
                receiver_port: 5001,
                uplink_ips: ["192.168.1.10", "10.0.0.5"],
                mode: "classic",
            },
        });
        expect(config.sink).toBe("srtla");
        expect(ini).toContain('sink = "srtla"');
        expect(ini).toContain("[srtla]");
        expect(ini).not.toContain("listen_addr");
        expect(ini).toContain('receiver_host = "srtla.example.com"');
        expect(ini).toContain("receiver_port = 5001");
        expect(ini).toContain('uplink_ips = ["192.168.1.10", "10.0.0.5"]');
        expect(ini).toContain('mode = "classic"');
        expect(ini).toContain("conn_timeout_ms = 5000");
    });

    test("sink = srtla without a receiver is rejected", () => {
        expect(() =>
            buildCtrlIRLCoderConfig({ sink: "srtla", srtla: { uplink_ips: ["1.2.3.4"] } }),
        ).toThrow();
    });

    test("srtla round-trip parse -> serialize -> parse is stable", () => {
        const { config, ini } = buildCtrlIRLCoderConfig({
            sink: "srtla",
            srtla: {
                receiver_host: "srtla.example.com",
                uplink_ips: ["1.2.3.4"],
            },
        });
        const ini2 = serializeCtrlIRLCoderConfig(
            CtrlIRLCoderConfigSchema.parse(config),
        );
        expect(ini2).toBe(ini);
    });

    test("rejects max_bitrate < min_bitrate", () => {
        expect(() =>
            buildCtrlIRLCoderConfig({
                general: { min_bitrate: 8000, max_bitrate: 1000 },
            }),
        ).toThrow();
    });

    test("rejects unknown balancer", () => {
        expect(() =>
            CtrlIRLCoderConfigSchema.parse({
                general: { balancer: "pid" },
            }),
        ).toThrow();
    });

    test("rejects aimd decr_mult outside [0,1)", () => {
        expect(() =>
            CtrlIRLCoderConfigSchema.parse({ aimd: { decr_mult: 1.5 } }),
        ).toThrow();
    });

    test("full round-trip parse -> serialize -> parse is stable", () => {
        const { config, ini } = buildCtrlIRLCoderConfig({
            srt: { uri: "srt://127.0.0.1:9101" },
            adaptive: { incr_step: 40, decr_step: 200 },
        });
        const ini2 = serializeCtrlIRLCoderConfig(
            CtrlIRLCoderConfigSchema.parse(config),
        );
        expect(ini2).toBe(ini);
    });
});

describe("buildScene", () => {
    test("defaults canvas and name", () => {
        const { scene } = buildScene({
            layers: [
                {
                    id: "bg",
                    type: "video",
                    source: "/dev/video0",
                    properties: { zorder: 1 },
                },
            ],
        });
        expect(scene.name).toBe("main");
        expect(scene.canvas).toEqual({ width: 1280, height: 720 });
    });

    test("rejects unknown layer type", () => {
        expect(() =>
            SceneSchema.parse({
                layers: [
                    { id: "x", type: "webcam", source: "", properties: {} },
                ],
            }),
        ).toThrow();
    });
});
