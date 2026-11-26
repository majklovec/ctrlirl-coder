// test-obs-interactive-browser.ts — tests TWO concurrent browser-source
// layers. Each browser layer is a persistent headless Chrome whose page is
// a local HTML file with a <video> tag looping a generated color video:
// layer "browser-red" plays a red video rectangle with a 440 Hz tone,
// "browser-blue" a blue one with an 880 Hz tone. Both audio tracks ride
// the browser layer's Pulse null-sink monitor chain (one per layer) and
// share one audio track in the SRTLA stream. The space key toggles which
// layer is visible (alpha 1 <-> 0 via RPC layer.set); the hidden layer
// keeps running in its own Chrome but its volume filter is muted. Each
// layer also has a volume filter; a/v step the visible layer's volume by
// 0.5 (0..2) via RPC filter.set.
//
// Usage (OBS SRTLA input must already listen on 127.0.0.1:$PORT):
//   bun scripts/test-obs-interactive-browser.ts
//
// Env overrides: PORT (5000), SRTLA_HOST (127.0.0.1),
// SRTLA_UPLINK_IPS (127.0.0.1, comma-separated), BINARY, CONFIG,
// PIPELINE, API_ADDR.
// Requires: ffmpeg (generates the two test videos in a temp dir).
//
// Keys: space = switch red/blue, a = volume up, v = volume down,
// c = confirm video/audio playback in OBS, q = stop the app.
// Non-TTY stdin: "switch", "vol-up", "vol-down", "confirm", "quit".

import {
    existsSync,
    mkdtempSync,
    readFileSync,
    rmSync,
    statSync,
    writeFileSync,
} from "node:fs";
import http from "node:http";
import { isIPv4 } from "node:net";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import { buildSRTLATestConfig } from "./obs-srtla-config";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

const PORT_TEXT = process.env.PORT ?? "5000";
const PORT = Number(PORT_TEXT);
const SRTLA_HOST = (process.env.SRTLA_HOST ?? "127.0.0.1").trim();
const SRTLA_UPLINK_IPS = (process.env.SRTLA_UPLINK_IPS ?? "127.0.0.1")
    .split(",")
    .map((ip) => ip.trim());
const BINARY = process.env.BINARY ?? "bin/ctrlirl-coder-linux-amd64";
const CONFIG = process.env.CONFIG ?? "config.toml";

// Canvas is 1280x720. Both browser layers occupy the full canvas; the
// <video> rectangle sits centered inside the page.
const CANVAS_W = 1280;
const CANVAS_H = 720;
const PIPELINE = process.env.PIPELINE ?? "pipeline/example-layered.pipeline";

const TMP = mkdtempSync(join(tmpdir(), "test-obs-interactive-browser-"));
// Shared HTTP API listener (JSON-RPC /rpc + /metrics).
const API_ADDR = process.env.API_ADDR ?? "127.0.0.1:8899";
const APP_LOG = join(TMP, "app.log");

function fail(msg: string): never {
    console.error(`FAIL: ${msg}`);
    cleanup();
    process.exit(1);
}

function cleanup(): void {
    rmSync(TMP, { recursive: true, force: true });
}

if (
    !/^\d+$/.test(PORT_TEXT) ||
    !Number.isInteger(PORT) ||
    PORT < 1 ||
    PORT > 65535
) {
    fail(`invalid SRTLA port: ${PORT_TEXT}`);
}
if (!SRTLA_HOST) fail("SRTLA_HOST must not be empty");
if (SRTLA_UPLINK_IPS.some((ip) => !isIPv4(ip))) {
    fail("SRTLA_UPLINK_IPS must contain comma-separated IPv4 addresses");
}

const resolveRoot = (p: string) => (p.startsWith("/") ? p : join(root, p));
for (const p of [CONFIG, PIPELINE, BINARY]) {
    if (!existsSync(resolveRoot(p))) fail(`not found: ${p}`);
}

// --- 0. Test videos + pages ----------------------------------------------------
// Two 2 s color videos (red / blue), h264 + an AAC sine tone (440 / 880
// Hz, one per color), looped by the <video> tag. The tones let you tell
// which browser layer's audio chain is in the stream. A white square
// sweeps across each frame (red: horizontal, blue: vertical) so the
// <video> visibly plays.
async function genVideo(
    name: string,
    color: string,
    freq: number,
): Promise<void> {
    // 280 px/s over 560 px = one sweep per 2 s loop; the other axis is
    // fixed mid-frame. Commas in the mod() expression are escaped.
    const box =
        color === "red"
            ? `drawbox=x='mod(t*280\\,560)':y=140:w=80:h=80:color=white:t=fill`
            : `drawbox=x=140:y='mod(t*280\\,200)':w=80:h=80:color=white:t=fill`;
    const out = join(TMP, name);
    const proc = Bun.spawn(
        [
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            "error",
            "-y",
            "-f",
            "lavfi",
            "-i",
            `color=c=${color}:s=640x360:d=2:r=30,${box}`,
            "-f",
            "lavfi",
            "-i",
            `sine=frequency=${freq}:duration=2`,
            "-c:v",
            "libx264",
            "-pix_fmt",
            "yuv420p",
            "-c:a",
            "aac",
            "-shortest",
            out,
        ],
        { stdio: ["ignore", "ignore", "inherit"] },
    );
    const code = await proc.exited;
    if (code !== 0 || !existsSync(out) || statSync(out).size === 0) {
        fail(`ffmpeg failed to produce ${name} (exit code ${code})`);
    }
}

await genVideo("red.mp4", "red", 440); // A
await genVideo("blue.mp4", "blue", 880); // A + octave

function page(
    videoName: string,
    color: string,
    freq: number,
): {
    path: string;
    url: string;
} {
    const border = color === "red" ? "#ff0000" : "#0000ff";
    const html = `<!doctype html>
<style>body{margin:0;width:100vw;height:100vh;box-sizing:border-box;
background:#111;color:#fff;display:flex;flex-direction:column;
align-items:center;justify-content:center;gap:24px;font-family:sans-serif}
h1{margin:0;font-size:40px}</style>
<h1>${color.toUpperCase()} ${freq} Hz</h1>
<video src="${videoName}" width="640" height="360" autoplay loop playsinline
style="border:4px solid ${border}"></video>`;
    const path = join(TMP, `${videoName}.html`);
    writeFileSync(path, html);
    return { path, url: pathToFileURL(path).href };
}

const redPage = page("red.mp4", "red", 440);
const bluePage = page("blue.mp4", "blue", 880);

// Scene: two browser layers in the same slot. Blue starts hidden (alpha 0);
// space toggles the alphas. Both Chromes stay alive the whole run — that is
// the point of the test (two concurrent browser sources). Each layer carries
// a volume filter (index 0) adjusted with a/v via RPC filter.set.
const volumeFilter = (v: number) => [{ kind: "volume", params: { volume: v } }];
const sceneA = {
    name: "test-obs-browser",
    canvas: { width: CANVAS_W, height: CANVAS_H },
    layers: [
        {
            id: "browser-red",
            type: "browser",
            source: redPage.url,
            properties: {
                zorder: 1,
                xpos: 0,
                ypos: 0,
                width: CANVAS_W,
                height: CANVAS_H,
                alpha: 1.0,
                framerate: 30,
            },
            filters: volumeFilter(1.0),
        },
        {
            id: "browser-blue",
            type: "browser",
            source: bluePage.url,
            properties: {
                zorder: 2,
                xpos: 0,
                ypos: 0,
                width: CANVAS_W,
                height: CANVAS_H,
                alpha: 0.0,
                framerate: 30,
            },
            filters: volumeFilter(1.0),
        },
    ],
};
const scenePath = join(TMP, "browser-scene.json");
writeFileSync(scenePath, JSON.stringify(sceneA, null, 2));

// --- 1. Config: select SRTLA and point it at the OBS input ---------------------
const configTmp = join(TMP, "config.toml");
try {
    const cfgSrc = readFileSync(resolveRoot(CONFIG), "utf8");
    writeFileSync(
        configTmp,
        buildSRTLATestConfig(cfgSrc, SRTLA_HOST, PORT, SRTLA_UPLINK_IPS),
    );
} catch (err) {
    fail(
        `cannot prepare SRTLA config: ${err instanceof Error ? err.message : String(err)}`,
    );
}

// --- 2. Launch the app ---------------------------------------------------------
console.log(
    `=== streaming via SRTLA to OBS on ${SRTLA_HOST}:${PORT} (binary=${BINARY}) ===`,
);
console.log(`    uplinks: ${SRTLA_UPLINK_IPS.join(", ")}`);
console.log(
    `    two browser layers: red video (440 Hz) + blue video (880 Hz).`,
);
console.log(
    "    space = switch video, a/v = visible layer volume, c = confirm OBS playback, q = stop.",
);
console.log(
    "    Confirm both videos and audio in OBS; SRTLA registration alone does not prove playback.",
);

// App logs to a file (asserts read it) and are tailed live to the terminal.
writeFileSync(APP_LOG, "");
const logFile = Bun.file(APP_LOG);
const app = Bun.spawn(
    [
        resolveRoot(BINARY),
        "--config",
        configTmp,
        "--pipeline",
        resolveRoot(PIPELINE),
        "--scene",
        scenePath,
        "--api-addr",
        API_ADDR,
    ],
    {
        cwd: root,
        stdout: logFile,
        stderr: logFile,
    },
);
const tail = Bun.spawn(["tail", "-f", APP_LOG], {
    stdout: "inherit",
    stderr: "inherit",
});

// Forward INT/TERM so Ctrl-C stops the app too.
let stopping = false;
const onSig = () => stop();
process.on("SIGINT", onSig);
process.on("SIGTERM", onSig);

// --- 3. RPC over the shared HTTP API -----------------------------------------
// POST /rpc, one JSON-RPC 2.0 object per request. The app starts listening
// The app starts listening ~1 s after start, so
// the readiness loop retries system.ping until it answers.
let rpcOk = false;

// rpcPost sends one request and resolves with the response body ("" on
// transport failure — fire-and-forget callers ignore it).
function rpcPost(method: string, params: unknown): Promise<string> {
    if (!rpcOk) return Promise.resolve(""); // API not up yet; drop
    const body = JSON.stringify({
        jsonrpc: "2.0",
        id: Date.now(),
        method,
        params,
    });
    return new Promise((resolve) => {
        const u = new URL(`http://${API_ADDR}/rpc`);
        const req = http.request(
            {
                hostname: u.hostname,
                port: u.port,
                path: u.pathname,
                method: "POST",
                headers: { "Content-Type": "application/json" },
                timeout: 2000,
            },
            (res) => {
                let data = "";
                res.setEncoding("utf8");
                res.on("data", (c) => (data += c));
                res.on("end", () => resolve(data));
            },
        );
        req.on("error", () => resolve(""));
        req.on("timeout", () => {
            req.destroy();
            resolve("");
        });
        req.end(body);
    });
}

function rpc(method: string, params: unknown): void {
    void rpcPost(method, params).then((resp) => {
        if (resp) {
            try {
                const msg = JSON.parse(resp) as { error?: unknown };
                if (msg.error)
                    console.log(`rpc rejected: ${resp.slice(0, 200)}`);
            } catch {
                /* non-JSON response (e.g. 204) — nothing to report */
            }
        }
    });
}

async function pingLoop(): Promise<void> {
    while (!stopping && !rpcOk) {
        let up = false;
        try {
            const u = new URL(`http://${API_ADDR}/rpc`);
            const body = JSON.stringify({
                jsonrpc: "2.0",
                id: 0,
                method: "system.ping",
                params: {},
            });
            up = await new Promise<boolean>((resolve) => {
                const req = http.request(
                    {
                        hostname: u.hostname,
                        port: u.port,
                        path: u.pathname,
                        method: "POST",
                        headers: { "Content-Type": "application/json" },
                        timeout: 1000,
                    },
                    (res) => {
                        let data = "";
                        res.setEncoding("utf8");
                        res.on("data", (c) => (data += c));
                        res.on("end", () => resolve(data.includes("pong")));
                    },
                );
                req.on("error", () => resolve(false));
                req.on("timeout", () => {
                    req.destroy();
                    resolve(false);
                });
                req.end(body);
            });
        } catch {
            up = false;
        }
        if (up) {
            rpcOk = true;
            console.log("rpc connected (HTTP)");
            return;
        }
        await new Promise((r) => setTimeout(r, 300));
    }
}
void pingLoop();

interface SinkMetrics {
    packets: number;
    acknowledgements: number;
    bytes: number;
}

function readSinkMetrics(): Promise<SinkMetrics> {
    return new Promise((resolve, reject) => {
        const u = new URL(`http://${API_ADDR}/metrics`);
        const req = http.request(
            {
                hostname: u.hostname,
                port: u.port,
                path: u.pathname,
                method: "GET",
                timeout: 2000,
            },
            (res) => {
                let data = "";
                res.setEncoding("utf8");
                res.on("data", (chunk) => (data += chunk));
                res.on("error", reject);
                res.on("end", () => {
                    if (res.statusCode !== 200) {
                        reject(
                            new Error(
                                `metrics returned HTTP ${res.statusCode}`,
                            ),
                        );
                        return;
                    }
                    const values = new Map<string, string>();
                    for (const line of data.split("\n")) {
                        const [name, value] = line.trim().split(/\s+/, 2);
                        if (name && value !== undefined)
                            values.set(name, value);
                    }
                    const names = [
                        "sink_pkts_total",
                        "sink_acks_total",
                        "bytes_sent",
                    ];
                    const numbers = names.map((name) => {
                        const value = values.get(name);
                        return value === undefined ? NaN : Number(value);
                    });
                    if (
                        numbers.some(
                            (value) => !Number.isFinite(value) || value < 0,
                        )
                    ) {
                        reject(
                            new Error(
                                `metrics missing or invalid: ${names.join(", ")}`,
                            ),
                        );
                        return;
                    }
                    resolve({
                        packets: numbers[0],
                        acknowledgements: numbers[1],
                        bytes: numbers[2],
                    });
                });
            },
        );
        req.on("error", reject);
        req.on("timeout", () =>
            req.destroy(new Error("metrics request timed out")),
        );
        req.end();
    });
}

let metricsAtStop: Promise<SinkMetrics | null> | undefined;
function stop(): void {
    if (stopping) return;
    stopping = true;
    try {
        process.stdin.setRawMode?.(false);
    } catch {
        /* non-TTY */
    }
    metricsAtStop = readSinkMetrics()
        .catch((err: unknown) => {
            console.error(
                `FAIL: cannot read sink metrics: ${err instanceof Error ? err.message : String(err)}`,
            );
            return null;
        })
        .finally(() => {
            app.kill("SIGINT");
        });
}

// --- 4. Red/blue switch ----------------------------------------------------------
// Space toggles which browser layer is visible: alpha 1 on the shown one,
// 0 on the hidden one. Both Chromes keep running; the hidden layer's volume
// filter is muted until it is visible again.
let showRed = true;
let playbackConfirmed = false;

function confirmPlayback(): void {
    playbackConfirmed = true;
    console.log("\nOBS video and audio playback confirmed by operator.");
}

function toggleLayer(): void {
    showRed = !showRed;
    rpc("layer.set", {
        id: "browser-red",
        properties: {
            zorder: 1,
            xpos: 0,
            ypos: 0,
            width: CANVAS_W,
            height: CANVAS_H,
            alpha: showRed ? 1.0 : 0.0,
        },
    });
    rpc("layer.set", {
        id: "browser-blue",
        properties: {
            zorder: 2,
            xpos: 0,
            ypos: 0,
            width: CANVAS_W,
            height: CANVAS_H,
            alpha: showRed ? 0.0 : 1.0,
        },
    });
    process.stderr.write(`\rvisible: ${showRed ? "RED" : "BLUE"} video    `);
}

// a/v step the visible layer's volume filter (index 0). The compositor mutes
// the hidden layer's filter without changing its configured volume.
const VOLS: Record<string, number> = {
    "browser-red": 1.0,
    "browser-blue": 1.0,
};
function adjustVolume(delta: number): void {
    const id = showRed ? "browser-red" : "browser-blue";
    VOLS[id] = Math.max(0, Math.min(2, VOLS[id] + delta));
    rpc("filter.set", {
        id,
        index: 0,
        kind: "volume",
        params: { volume: VOLS[id] },
    });
    process.stderr.write(`\r${id}: volume=${VOLS[id].toFixed(2)}    `);
}

const stdin = process.stdin;
if (stdin.isTTY) {
    stdin.setRawMode(true);
    stdin.resume();
    stdin.on("data", (buf: Buffer) => {
        const kb = buf.toString("latin1");
        for (const c of kb) {
            if (c === " ")
                toggleLayer(); // space
            else if (c === "a")
                adjustVolume(0.5); // volume up
            else if (c === "v")
                adjustVolume(-0.5); // volume down
            else if (c === "c") confirmPlayback();
            else if (c === "q") {
                console.error("\nstopping…");
                stop();
            }
        }
    });
} else {
    // Non-TTY: line commands for smoke testing.
    let buf = "";
    stdin.resume();
    stdin.on("data", (chunk: Buffer) => {
        buf += chunk.toString();
        let i: number;
        while ((i = buf.indexOf("\n")) >= 0) {
            const cmd = buf.slice(0, i).trim().toLowerCase();
            buf = buf.slice(i + 1);
            if (cmd === "switch" || cmd === "space") toggleLayer();
            else if (cmd === "vol-up") adjustVolume(0.5);
            else if (cmd === "vol-down") adjustVolume(-0.5);
            else if (cmd === "confirm") confirmPlayback();
            else if (cmd === "quit") {
                console.error("\nstopping…");
                stop();
            }
        }
    });
}

// --- 5. Wait for the app to exit, then assert -----------------------------------
// Bun leaves exitCode null when the process died by signal; our graceful
// SIGINT path exits 0, but a signal death is acceptable too.
await app.exited;
const code = app.exitCode ?? null;
const metrics = metricsAtStop ? await metricsAtStop : null;

stopping = true;
tail.kill();
await tail.exited;

const log = existsSync(APP_LOG) ? readFileSync(APP_LOG, "utf8") : "";
const crit = log
    .split("\n")
    .filter((l) => /CRITICAL|ERROR/i.test(l))
    .filter((l) => !l.includes("gst_mini_object_unref"));

const logLines = log.split("\n");
const results: [boolean, string][] = [
    [
        code === 0 || code === null,
        `app exited (code=${code === null ? "signal" : code})`,
    ],
    [/pipeline playing/.test(log), "pipeline reached PLAYING"],
    ...SRTLA_UPLINK_IPS.map((ip): [boolean, string] => [
        logLines.some(
            (line) =>
                line.includes("srtla connection established") &&
                line.split(/\s+/).includes(`ip=${ip}`),
        ),
        `SRTLA uplink ${ip} registered with OBS`,
    ]),
    [/srt: connected/i.test(log), "SRT session established through SRTLA"],
    [
        metrics !== null && metrics.packets > 0,
        `SRTLA packets sent (${metrics?.packets ?? "unavailable"})`,
    ],
    [
        metrics !== null && metrics.acknowledgements > 0,
        `SRTLA packets acknowledged (${metrics?.acknowledgements ?? "unavailable"})`,
    ],
    [
        metrics !== null && metrics.bytes > 0,
        `encoded bytes sent (${metrics?.bytes ?? "unavailable"})`,
    ],
    [playbackConfirmed, "OBS video and audio playback confirmed by operator"],
    [
        (log.match(/browser overlay/g) ?? []).length >= 2,
        "both browser layers started",
    ],
    [crit.length === 0, "no CRITICAL/ERROR in app log"],
];
let all = true;
for (const [ok, name] of results) {
    console.log(`${ok ? "PASS" : "FAIL"}: ${name}`);
    if (!ok) all = false;
}
if (crit.length) console.error(crit.join("\n"));

cleanup();
process.exit(all ? 0 : 1);
