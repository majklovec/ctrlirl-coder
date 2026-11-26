// test-obs-browser.ts — stream one scene with a single browser layer
// (headless Chrome overlay of scripts/ball.html at 30 fps — ball.html paints
// on requestAnimationFrame and the 30 fps capture takes every other page
// frame, so the motion stays a constant 2 steps per captured frame) to OBS
// over SRTLA. Pair with a 30fps OBS project (Settings → Video → 30).
//
// Usage (OBS SRTLA input must already listen on 127.0.0.1:$PORT):
//   bun scripts/test-obs-browser.ts
//
// Env overrides: PORT (5000), SRTLA_HOST (127.0.0.1),
// SRTLA_UPLINK_IPS (127.0.0.1, comma-separated), BINARY, CONFIG, PIPELINE,
// SCENE.
//
// Keys: q stops the app (Ctrl-C also). Non-TTY stdin: "quit" line.

import {
    existsSync,
    mkdtempSync,
    readFileSync,
    rmSync,
    writeFileSync,
} from "node:fs";
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
const PIPELINE =
    process.env.PIPELINE ?? "pipeline/example-layered-1080p30.pipeline";
const SCENE = process.env.SCENE ?? "";

const TMP = mkdtempSync(join(tmpdir(), "test-obs-browser-"));
const APP_LOG = join(TMP, "app.log");

function fail(msg: string): never {
    console.error(`FAIL: ${msg}`);
    cleanup();
    process.exit(1);
}

function cleanup(): void {
    rmSync(TMP, { recursive: true, force: true });
}

// --- 0. Scene: one browser layer ----------------------------------------------
let scenePath = SCENE;

function page(): { path: string; url: string } {
    // ball.html lives next to this script in scripts/. Anchor to the script's
    // own directory (not the CWD) so the file:// URL resolves no matter where
    // `bun scripts/test-obs-browser.ts` is invoked from.
    const path = join(dirname(fileURLToPath(import.meta.url)), "ball.html");
    return { path, url: pathToFileURL(path).href };
}

if (!scenePath) {
    scenePath = join(TMP, "test-scene.json");
    const scene = {
        name: "test-obs-browser",
        canvas: { width: 1920, height: 1080 },
        layers: [
            {
                id: "browser",
                type: "browser",
                source: page().url,
                properties: {
                    zorder: 1,
                    xpos: 0,
                    ypos: 0,
                    width: 1920,
                    height: 1080,
                    alpha: 1.0,
                    framerate: 30,
                },
            },
        ],
    };
    writeFileSync(scenePath, JSON.stringify(scene, null, 2));
}

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
console.log("    q = stop (Ctrl-C also).");

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
    ],
    {
        cwd: root,
        stdout: logFile,
        stderr: logFile,
    },
);
const tail = Bun.spawn(["tail", "-f", APP_LOG], { stderr: "inherit" });

let stopping = false;
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

// Forward INT/TERM so Ctrl-C stops the app too.
process.on("SIGINT", stop);
process.on("SIGTERM", stop);

// --- 3. Quit key ---------------------------------------------------------------
const stdin = process.stdin;
if (stdin.isTTY) {
    stdin.setRawMode(true);
    stdin.resume();
    stdin.on("data", (buf: Buffer) => {
        if (buf.toString("latin1").includes("q")) {
            console.error("\nstopping…");
            stop();
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
            if (cmd === "quit") {
                console.error("\nstopping…");
                stop();
            }
        }
    });
}

// --- 4. Wait for the app to exit, then assert ----------------------------------
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
