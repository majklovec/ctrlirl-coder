// Process control for @ceralive/ctrlirl_coder-go. No shell wrappers: all file
// writing and signal delivery happen here, Bun-first with a Node fallback.

import { spawn as nodeSpawn } from "node:child_process";
import { mkdirSync, writeFileSync as nodeWriteFileSync } from "node:fs";
import { dirname } from "node:path";
import { buildCtrlIRLCoderArgs, type CtrlIRLCoderArgsOptions } from "./args";
import { buildCtrlIRLCoderConfig, type CtrlIRLCoderConfig } from "./config";
import type { Scene } from "./schemas";

export interface SpawnOptions extends CtrlIRLCoderArgsOptions {
    binaryPath?: string;
    env?: Record<string, string>;
    onStdout?: (line: string) => void;
    onStderr?: (line: string) => void;
    onExit?: (code: number) => void;
}

export interface CtrlIRLCoderProcess {
    pid: number;
    kill(signal?: NodeJS.Signals): void;
    sendHup(): void;
    sendTerm(): void;
    wait(): Promise<number>;
}

// Feature-detected at call time, never captured at module scope.
function isBun(): boolean {
    return typeof (globalThis as { Bun?: unknown }).Bun !== "undefined";
}

export function spawnCtrlIRLCoder(
    opts: SpawnOptions = {},
): CtrlIRLCoderProcess {
    const bin = opts.binaryPath ?? resolveBinary();
    const args = buildCtrlIRLCoderArgs(opts);
    const env = { ...process.env, ...(opts.env ?? {}) };

    return isBun()
        ? spawnBun(bin, args, env, opts)
        : spawnNode(bin, args, env, opts);
}

function resolveBinary(): string {
    return process.env.CTRLIRL_CODER_BIN ?? "ctrlirl_coder-go";
}

async function writeConfig(
    path: string,
    config: CtrlIRLCoderConfig,
): Promise<void> {
    const { ini } = buildCtrlIRLCoderConfig(config);
    await writeFile(path, ini);
}

async function writeScene(path: string, scene: Scene): Promise<void> {
    await writeFile(path, JSON.stringify(scene, null, 2));
}

export async function writeFile(path: string, content: string): Promise<void> {
    mkdirSync(dirname(path), { recursive: true });
    if (isBun()) {
        const Bun = (globalThis as { Bun: typeof import("bun") }).Bun;
        await Bun.write(path, content);
        return;
    }
    const fs = await import("node:fs/promises");
    await fs.writeFile(path, content, "utf8");
}

// Synchronous variant (used by PipelineBuilder.build, which returns the
// written path). node:fs works under both runtimes.
export function writeFileSync(path: string, content: string): void {
    mkdirSync(dirname(path), { recursive: true });
    nodeWriteFileSync(path, content, "utf8");
}

type Chunk = Uint8Array | string;

/** NodeJS.ReadableStream that is also a Web ReadableStream (Bun child streams). */
interface PipedStream {
    getReader?(): ReadableStreamDefaultReader<Uint8Array>;
}

async function* webStreamToAsyncIter(
    stream: ReadableStream<Uint8Array>,
): AsyncIterableIterator<Chunk> {
    const reader = stream.getReader();
    try {
        for (;;) {
            const { done, value } = await reader.read();
            if (done) return;
            yield value;
        }
    } finally {
        reader.releaseLock();
    }
}

// Shared line-by-line piping for both runtimes: Bun child streams are Web
// ReadableStreams (drained via getReader), node streams are Readables
// (async-iterable directly). Chunks are UTF-8 bytes — decode with
// TextDecoder (Uint8Array.toString() is a comma list, not text).
//
// IMPORTANT: both stdout and stderr must ALWAYS be drained, even when no
// callback is set — an undrained pipe wedges the child with SIGPIPE.
async function pipeLines(
    stream: PipedStream | NodeJS.ReadableStream | undefined,
    cb: ((line: string) => void) | undefined,
): Promise<void> {
    if (!stream) return;
    const iter: AsyncIterable<Chunk> =
        typeof (stream as PipedStream).getReader === "function"
            ? webStreamToAsyncIter(
                  stream as unknown as ReadableStream<Uint8Array>,
              )
            : (stream as unknown as AsyncIterable<Chunk>);
    const decoder = new TextDecoder();
    let buf = "";
    for await (const chunk of iter) {
        if (!cb) continue;
        buf += typeof chunk === "string" ? chunk : decoder.decode(chunk);
        let idx: number;
        while ((idx = buf.indexOf("\n")) >= 0) {
            const line = buf.slice(0, idx).replace(/\r$/, "");
            buf = buf.slice(idx + 1);
            cb(line);
        }
    }
}

function spawnBun(
    bin: string,
    args: string[],
    env: Record<string, string | undefined>,
    opts: SpawnOptions,
): CtrlIRLCoderProcess {
    const Bun = (globalThis as { Bun: typeof import("bun") }).Bun;
    // Explicit stdio pipes: the default leaves stderr unconnected, which
    // drops the child's log output.
    const child = Bun.spawn([bin, ...args], {
        env,
        stdio: ["pipe", "pipe", "pipe"],
    });

    void pipeLines(child.stdout, opts.onStdout);
    void pipeLines(child.stderr, opts.onStderr);
    const exited = child.exited;
    void exited.then((code) => opts.onExit?.(code));
    const pid = child.pid;

    return {
        pid,
        kill(signal: NodeJS.Signals = "SIGTERM") {
            child.kill(signal);
        },
        sendHup() {
            process.kill(pid, "SIGHUP");
        },
        sendTerm() {
            process.kill(pid, "SIGTERM");
        },
        wait: () => exited,
    };
}

function spawnNode(
    bin: string,
    args: string[],
    env: Record<string, string | undefined>,
    opts: SpawnOptions,
): CtrlIRLCoderProcess {
    const child = nodeSpawn(bin, args, { env, stdio: "pipe" });

    void pipeLines(child.stdout, opts.onStdout);
    void pipeLines(child.stderr, opts.onStderr);
    const exited = new Promise<number>((resolve) => {
        child.on("exit", (code: number | null) => {
            const c = code ?? 1;
            opts.onExit?.(c);
            resolve(c);
        });
    });
    const pid = child.pid ?? 0;

    return {
        pid,
        kill(signal: NodeJS.Signals = "SIGTERM") {
            child.kill(signal);
        },
        sendHup() {
            process.kill(pid, "SIGHUP");
        },
        sendTerm() {
            process.kill(pid, "SIGTERM");
        },
        wait: () => exited,
    };
}

export { writeConfig, writeScene };
