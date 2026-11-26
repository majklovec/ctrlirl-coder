// test-obs-interactive-srtla.ts — same setup as test-obs.sh, but the 'hls'
// layer is moved by the USER with arrow keys (10 px per keypress) over the
// app's shared HTTP API (POST /rpc, --api-addr). Streams to OBS over SRTLA.
//
// Usage (OBS SRTLA input must already listen on 127.0.0.1:$PORT):
//   bun scripts/test-obs-interactive-srtla.ts
//
// Env overrides: PORT (5000), SRTLA_HOST (127.0.0.1),
// SRTLA_UPLINK_IPS (127.0.0.1, comma-separated), BINARY, CONFIG, PIPELINE,
// SCENE, PAGE_URL.
//
// Keys: ← → ↑ ↓ move the hls layer (10 px), r reset to 0,0, q stop the app.
// Non-TTY stdin: line commands "left" "right" "up" "down" "reset" "quit".

import {
    existsSync,
    mkdtempSync,
    readFileSync,
    rmSync,
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

// Canvas is 1280x720. The hls layer is smaller so it can actually move:
// geometry validation rejects any rect that would exceed the canvas.
const CANVAS_W = 1280;
const CANVAS_H = 720;
const HLS_W = 1180;
const HLS_H = 640;
const PIPELINE = process.env.PIPELINE ?? "pipeline/example-layered.pipeline";
const SCENE = process.env.SCENE ?? "";
const PAGE_URL = process.env.PAGE_URL ?? "https://seznam.cz";
const STEP = 10;

const TMP = mkdtempSync(join(tmpdir(), "test-obs-interactive-"));
// Shared HTTP API listener (JSON-RPC /rpc + /metrics).
const API_ADDR = process.env.API_ADDR ?? "0.0.0.0:8899";
const APP_LOG = join(TMP, "app.log");

function fail(msg: string): never {
    console.error(`FAIL: ${msg}`);
    cleanup();
    process.exit(1);
}

function cleanup(): void {
    rmSync(TMP, { recursive: true, force: true });
}

// --- 0. Scene ----------------------------------------------------------------
let sceneA: unknown = null;
let scenePath = SCENE;

function page(): {
    path: string;
    url: string;
} {
    // ball.html lives next to this script in scripts/. Anchor to the script's
    // own directory (not the CWD) so the file:// URL resolves no matter where
    // `bun scripts/test-obs-interactive-srtla.ts` is invoked from.
    const path = join(dirname(fileURLToPath(import.meta.url)), "ball.html");

    return { path, url: pathToFileURL(path).href };
}

if (!scenePath) {
    scenePath = join(TMP, "test-scene.json");
    sceneA = {
        name: "test-obs-interactive",
        canvas: { width: 1280, height: 720 },
        layers: [

            {
                id: "hls",
                type: "video",
                source: "https://devstreaming-cdn.apple.com/videos/streaming/examples/img_bipbop_adv_example_fmp4/master.m3u8",
                // Not canvas-sized: the arrow keys move it live, and
                // a canvas-wide rect can never move (geometry
                // validation rejects rects exceeding the canvas).
            },
        ],
    };
    writeFileSync(scenePath, JSON.stringify(sceneA, null, 2));
}

// Second scene: two test videos only — the space key toggles between
// sceneA (full test scene) and sceneB via RPC scene.switch.
const sceneB = {
    name: "test-obs-scene-b",
    canvas: { width: 1280, height: 720 },
    layers: [
        {
            id: "vid-a",
            type: "video",
            source: "",
            properties: {
                zorder: 1,
                xpos: 0,
                ypos: 0,
                width: 640,
                height: 360,
                alpha: 1.0,
            },
        },
        {
            id: "vid-b",
            type: "video",
            source: "",
            properties: {
                zorder: 2,
                xpos: 640,
                ypos: 360,
                width: 640,
                height: 360,
                alpha: 1.0,
            },
        },
        {
            id: "browser-red",
            type: "browser",
            source: page().url,
            properties: {
                zorder: 10,
                xpos: 0,
                ypos: 0,
                width: 640,
                height: 480,
                alpha: 1.0,
                framerate: 30,
            },
        },
        {
            id: "test",
            type: "testcode",
            properties: {
                zorder: 100,
                xpos: 0,
                ypos: 0,
                width: 50,
                height: 59,
            },
        },
    ],
};

// With a user SCENE file, sceneA is that file (register it as-is).
if (SCENE) sceneA = JSON.parse(readFileSync(scenePath, "utf8"));

const resolveRoot = (p: string) => (p.startsWith("/") ? p : join(root, p));
for (const p of [CONFIG, PIPELINE, scenePath, BINARY]) {
    if (!existsSync(resolveRoot(p))) fail(`not found: ${p}`);
}
// The browser layer loads ball.html over file://. A missing file renders a
// blank page (the layer is "present" but invisible), so fail fast here.
if (!existsSync(page().path)) fail(`browser page not found: ${page().path}`);
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
    `    hls layer is at the bottom-left (xpos=0). Arrow keys move it 10 px.`,
);
console.log("    r = reset, PgUp/PgDn = rotate 15°, space = switch scene");
console.log("    (a = full test scene, b = two test videos), q = stop.");

// App logs to a file (asserts read it) and are tailed live to the terminal.
const logFile = Bun.file(APP_LOG, { write: true, truncate: true });
const app = Bun.spawn(
    [
        join(root, BINARY),
        "--config",
        configTmp,
        "--pipeline",
        join(root, PIPELINE),
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
const tail = Bun.spawn(["tail", "-f", APP_LOG], { stderr: "inherit" });

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

// Register both scenes once the API answers (register-only, no switch), so
// the space key can toggle between them with scene.switch.
let registered = false;
function registerScenes(): void {
    if (registered) return;
    registered = true;
    rpc("scene.load", { name: "a", json: sceneA, switch: false });
    rpc("scene.load", { name: "b", json: sceneB, switch: false });
}

async function pingLoop(): Promise<void> {
    while (!stopping && !rpcOk) {
        const resp = await rpcPost("system.ping", {});
        if (resp.includes("pong")) {
            rpcOk = true;
            registerScenes();
            return;
        }
        await new Promise((r) => setTimeout(r, 300));
    }
}
void pingLoop();

function stop(): void {
    if (stopping) return;
    stopping = true;
    try {
        process.stdin.setRawMode?.(false);
    } catch {
        /* non-TTY */
    }
    app.kill("SIGINT");
}

// --- 4. Arrow-key mover ---------------------------------------------------------
let x = 0;
let y = 0;

function move(dx: number, dy: number): void {

}

// Arrow keys arrive as 3-char escape sequences (ESC, "[", letter), often
// split across data events — accumulate and parse, never per-char switch.
function resetLayer(): void {
    x = 0;
    y = 0;
    rpc("layer.set", {
        id: "hls",
        properties: {
            zorder: 1,
            xpos: 0,
            ypos: 0,
            width: HLS_W,
            height: HLS_H,
            alpha: 1.0,
        },
    });
    process.stderr.write(`\rhls: xpos=${x} ypos=${y}    `);
}

// hls layer rotation via the videorotate filter (index 2 in the generated
// scene); PgUp/PgDn step it by 15°. Starts at the scene default 45°.
let angle = 45;
function rotateLayer(delta: number): void {
    angle = (((angle + delta) % 360) + 360) % 360;
    rpc("filter.set", {
        id: "hls",
        index: 2,
        kind: "videorotate",
        params: { angle },
    });
    process.stderr.write(`\rhls: xpos=${x} ypos=${y} angle=${angle}    `);
}

// Space toggles between scene a (full test scene) and scene b (two test
// videos) via RPC scene.switch (both registered at connect time).
let curScene = "a";
function switchScene(): void {
    curScene = curScene === "a" ? "b" : "a";
    rpc("scene.switch", { name: curScene });
    const label =
        curScene === "a" ? "a — full test scene" : "b — two test videos";
    process.stderr.write(`\rscene: ${label}    `);
}

const stdin = process.stdin;
if (stdin.isTTY) {
    stdin.setRawMode(true);
    stdin.resume();
    let kb = "";
    stdin.on("data", (buf: Buffer) => {
        kb += buf.toString("latin1");
        while (kb.length > 0) {
            if (kb[0] === "\u001b" && kb[1] === "[") {
                // CSI: final byte is 0x40..0x7e, may arrive in later events
                // (arrows are 3 chars, PgUp/PgDn "ESC [ 5~" are 4).
                let end = -1;
                for (let i = 2; i < kb.length && i < 8; i++) {
                    const cc = kb.charCodeAt(i);
                    if (cc >= 0x40 && cc <= 0x7e) {
                        end = i + 1;
                        break;
                    }
                }
                if (end < 0) return; // sequence split across events
                const seq = kb.slice(0, end);
                kb = kb.slice(end);
                if (seq === "\u001b[A")
                    move(0, -STEP); // up
                else if (seq === "\u001b[B")
                    move(0, STEP); // down
                else if (seq === "\u001b[C")
                    move(STEP, 0); // right
                else if (seq === "\u001b[D")
                    move(-STEP, 0); // left
                else if (seq === "\u001b[5~")
                    rotateLayer(15); // PgUp
                else if (seq === "\u001b[6~") rotateLayer(-15); // PgDn
            } else {
                const c = kb[0];
                kb = kb.slice(1);
                if (c === "r") resetLayer();
                else if (c === " ")
                    switchScene(); // space
                else if (c === "q") {
                    console.error("\nstopping…");
                    stop();
                }
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
            if (cmd === "left") move(-STEP, 0);
            else if (cmd === "right") move(STEP, 0);
            else if (cmd === "up") move(0, -STEP);
            else if (cmd === "down") move(0, STEP);
            else if (cmd === "reset") resetLayer();
            else if (cmd === "pageup") rotateLayer(15);
            else if (cmd === "pagedown") rotateLayer(-15);
            else if (cmd === "space") switchScene();
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

stopping = true;
tail.kill();

const log = existsSync(APP_LOG) ? readFileSync(APP_LOG, "utf8") : "";
const crit = log
    .split("\n")
    .filter((l) => /CRITICAL|ERROR/i.test(l))
    .filter((l) => !l.includes("gst_mini_object_unref"));

const results: [string, boolean][] = [
    [
        code === 0 || code === null,
        `app exited (code=${code === null ? "signal" : code})`,
    ],
    [/pipeline playing/.test(log), "pipeline reached PLAYING"],
    [/srt: connected/i.test(log), "SRT connected to OBS"],
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
