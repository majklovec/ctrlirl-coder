// PipelineBuilder — single-layer gst-launch-style pipeline templates with the
// mandatory contract elements:
//
//   - terminal `appsink name=appsink` (Go pulls it, sends SRT itself)
//   - an encoder named `venc_bps` (bitrate in bps) or `venc_kbps` (kbps)
//
// The contract is validated after build; a template missing either element
// throws. There is intentionally NO in-pipeline SRT sink — the destination
// comes from the TOML config [srt] uri.

import { writeFileSync } from "./process";
import type { Hardware, Source } from "./types";

export interface PipelineBuildOptions {
    hardware: Hardware;
    source: Source;
    overrides?: { resolution?: string; framerate?: number };
    // v4l2: device node (default /dev/video0). file: input file path.
    device?: string;
    writeTo?: string;
}

const HARDWARES: Hardware[] = ["jetson", "rk3588", "n100", "generic"];

// Encoder element per hardware. The element NAME carries the bitrate-unit
// contract ("venc_bps" = bps, "venc_kbps" = kbit/s); the `bitrate` value is
// the initial target (the Go adaptive balancer updates it at runtime).
function encoder(hardware: Hardware, bitrateKbps: number): string {
    switch (hardware) {
        case "jetson":
            return (
                `nvenc.h264 preset-level=1 tune=1 ` +
                `venc-bps=${bitrateKbps * 1000} name=venc_bps`
            );
        case "rk3588":
            return (
                `mpph264enc max-bitrate=${bitrateKbps} name=venc_kbps`
            );
        case "n100":
            return (
                `qsvenc h264 bitrate=${bitrateKbps * 1000} name=venc_bps`
            );
        case "generic":
        default:
            return (
                `x264enc speed-preset=superfast tune=zerolatency ` +
                `bitrate=${bitrateKbps} key-int-max=30 name=venc_kbps`
            );
    }
}

// Source head (video). `test` is a synthetic source with a matching audio
// chain; v4l2/file are video-only.
function videoHead(source: Source, resolution: string, device: string): string {
    const [w, h] = resolution.split("x");
    const caps = `video/x-raw,width=${w},height=${h}`;
    switch (source) {
        case "test":
            return `videotestsrc is-live=true pattern=smpte ! ${caps}`;
        case "v4l2":
            return `v4l2src device=${device || "/dev/video0"} is-live=true ! ${caps}`;
        case "file":
            return `filesrc location=${device} ! decodebin ! ${caps}`;
    }
}

const AUDIO_HEAD =
    "audiotestsrc is-live=true wave=sine freq=440 ! " +
    "audio/x-raw,rate=48000,channels=2 ! audioconvert ! " +
    "voaacenc bitrate=128000 ! aacparse";

export class PipelineBuilder {
    static build(opts: PipelineBuildOptions): {
        pipeline: string;
        path?: string;
    } {
        if (!(HARDWARES as readonly string[]).includes(opts.hardware)) {
            throw new Error(`unknown hardware: ${opts.hardware}`);
        }
        const resolution = opts.overrides?.resolution ?? "1280x720";
        const framerate = opts.overrides?.framerate ?? 30;

        const lines: string[] = [];
        const device =
            opts.source === "v4l2" || opts.source === "file"
                ? (opts.device ??
                  (opts.source === "v4l2" ? "/dev/video0" : "input.mp4"))
                : "";
        // video chain
        lines.push(
            `${videoHead(opts.source, resolution, device)}` +
                `,framerate=${framerate}/1 ! videoconvert ` +
                `! video/x-raw,format=I420 ! queue ` +
                `! ${encoder(opts.hardware, 6000)} ` +
                `! h264parse config-interval=1 ! mux.`,
        );
        if (opts.source === "test") {
            lines.push(`${AUDIO_HEAD} ! mux.`);
        }
        lines.push("mpegtsmux name=mux");
        lines.push("  ! appsink name=appsink");

        const pipeline = lines.join("\n");
        validateContract(pipeline);

        let path: string | undefined;
        if (opts.writeTo) {
            writeFileSync(opts.writeTo, pipeline);
            path = opts.writeTo;
        }
        return { pipeline, path };
    }

    static listHardwareTypes(): string[] {
        return [...HARDWARES];
    }

    static listSources(hardware: string): string[] {
        if (!HARDWARES.includes(hardware as Hardware)) {
            throw new Error(`unknown hardware: ${hardware}`);
        }
        return ["test", "v4l2", "file"];
    }
}

// Contract validation: the pipeline must end in a terminal
// `appsink name=appsink` and carry an encoder named venc_bps / venc_kbps.
// (The in-pipeline srtsink is forbidden by design — the Go process owns the
// SRT socket.)
export function validateContract(pipeline: string): void {
    if (!/\bappsink\s+name=appsink\b/.test(pipeline)) {
        throw new Error(
            "pipeline contract violated: missing terminal `appsink name=appsink`",
        );
    }
    if (!/\bvenc_bps\b/.test(pipeline) && !/\bvenc_kbps\b/.test(pipeline)) {
        throw new Error(
            "pipeline contract violated: missing venc_bps/venc_kbps",
        );
    }
}
