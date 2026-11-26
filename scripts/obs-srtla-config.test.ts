import { expect, test } from "bun:test";
import { mkdtempSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { buildSRTLATestConfig } from "./obs-srtla-config";

test("selects SRTLA and preserves unrelated config and scheduler settings", () => {
    const source = `
sink = "srt"
[srt]
uri = "srt://example.com:7000"
latency = 120
[srtla]
receiver_host = "old.example.com"
receiver_port = 9000
uplink_ips = ["10.0.0.1"]
mode = "classic"
conn_timeout_ms = 7000
[general]
max_bitrate = 5000
`;
    const result = Bun.TOML.parse(
        buildSRTLATestConfig(source, "127.0.0.1", 5000, ["127.0.0.1", "127.0.0.2"]),
    );
    expect(result).toMatchObject({
        sink: "srtla",
        srt: { uri: "srt://example.com:7000", latency: 120 },
        srtla: {
            receiver_host: "127.0.0.1",
            receiver_port: 5000,
            uplink_ips: ["127.0.0.1", "127.0.0.2"],
            mode: "classic",
            conn_timeout_ms: 7000,
        },
        general: { max_bitrate: 5000 },
    });
});

test("adds a missing SRTLA table and sink selector", () => {
    const result = Bun.TOML.parse(
        buildSRTLATestConfig("[general]\nmin_bitrate = 500\n", "localhost", 5010, ["127.0.0.1"]),
    );
    expect(result).toMatchObject({
        sink: "srtla",
        general: { min_bitrate: 500 },
        srtla: { receiver_host: "localhost", receiver_port: 5010, uplink_ips: ["127.0.0.1"] },
    });
});

test("rejects malformed configuration instead of silently replacing it", () => {
    expect(() => buildSRTLATestConfig('srtla = "not a table"\n', "localhost", 5000, ["127.0.0.1"]))
        .toThrow("config [srtla] must be a table");
    expect(() => buildSRTLATestConfig("invalid = [\n", "localhost", 5000, ["127.0.0.1"]))
        .toThrow();
});

test("browser test rejects an invalid port and cleans its temporary directory", async () => {
    const dir = mkdtempSync(join(tmpdir(), "obs-srtla-config-test-"));
    try {
        const script = join(dirname(fileURLToPath(import.meta.url)), "test-obs-interactive-browser.ts");
        const proc = Bun.spawn([process.execPath, script], {
            env: { ...process.env, TMPDIR: dir, PORT: "70000" },
            stdout: "pipe",
            stderr: "pipe",
        });
        const stderr = await new Response(proc.stderr).text();
        expect(await proc.exited).toBe(1);
        expect(stderr).toContain("invalid SRTLA port: 70000");
        expect(readdirSync(dir)).toEqual([]);
    } finally {
        rmSync(dir, { recursive: true, force: true });
    }
});
