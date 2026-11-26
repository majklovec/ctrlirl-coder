// Config & scene serializers for @ctrlirl/ctrlirl-coder.
//
// serializeCtrlIRLCoderConfig emits TOML matching the Go config.Load() schema
// (internal/config/config.go): a top-level `sink` selector plus [general],
// [srt], [srtla], [adaptive], [aimd], [overlay], [pipeline], [scene],
// [control]. Optional keys are omitted entirely so the Go defaults apply.
// [srtla] is emitted only when sink = "srtla".

import {
    type CtrlIRLCoderConfig,
    type CtrlIRLCoderConfigInput,
    CtrlIRLCoderConfigSchema,
    type Scene,
    type SceneInput,
    SceneSchema,
} from "./schemas";

export type { CtrlIRLCoderConfig, Scene };

export function buildCtrlIRLCoderConfig(
    partial: CtrlIRLCoderConfigInput = {},
): {
    config: CtrlIRLCoderConfig;
    ini: string;
} {
    const config = CtrlIRLCoderConfigSchema.parse(partial);
    const ini = serializeCtrlIRLCoderConfig(config);
    return { config, ini };
}

function tomlStr(v: string): string {
    // Basic string: escape backslashes and double quotes.
    return `"${v.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
}

function line(key: string, value: string | number | boolean): string {
    if (typeof value === "string") return `${key} = ${tomlStr(value)}`;
    if (typeof value === "boolean")
        return `${key} = ${value ? "true" : "false"}`;
    return `${key} = ${value}`;
}

export function serializeCtrlIRLCoderConfig(
    config: CtrlIRLCoderConfig,
): string {
    const out: string[] = [];

    out.push(line("sink", config.sink));
    out.push("");
    out.push("[general]");
    out.push(line("min_bitrate", config.general.min_bitrate));
    out.push(line("max_bitrate", config.general.max_bitrate));
    out.push(line("balancer", config.general.balancer));
    out.push(line("frame_stall_ms", config.general.frame_stall_ms));
    out.push("");

    out.push("[srt]");
    out.push(line("uri", config.srt.uri));
    out.push(line("latency", config.srt.latency));
    if (config.srt.streamid !== undefined && config.srt.streamid !== "") {
        out.push(line("streamid", config.srt.streamid));
    }
    if (config.srt.passphrase !== undefined && config.srt.passphrase !== "") {
        out.push(line("passphrase", config.srt.passphrase));
    }
    if (config.srt.max_bandwidth_kbps > 0) {
        out.push(line("max_bandwidth_kbps", config.srt.max_bandwidth_kbps));
    }
    out.push("");

    if (config.sink === "srtla") {
        out.push("[srtla]");
        if (config.srtla.receiver_host !== undefined && config.srtla.receiver_host !== "") {
            out.push(line("receiver_host", config.srtla.receiver_host));
        }
        out.push(line("receiver_port", config.srtla.receiver_port));
        if (config.srtla.uplink_ips.length > 0) {
            const ips = config.srtla.uplink_ips.map((ip) => `"${ip}"`).join(", ");
            out.push(`uplink_ips = [${ips}]`);
        }
        out.push(line("mode", config.srtla.mode));
        out.push(line("conn_timeout_ms", config.srtla.conn_timeout_ms));
        out.push("");
    }

    out.push("[adaptive]");
    out.push(line("incr_step", config.adaptive.incr_step));
    out.push(line("decr_step", config.adaptive.decr_step));
    out.push(line("incr_interval", config.adaptive.incr_interval));
    out.push(line("decr_interval", config.adaptive.decr_interval));
    out.push("");

    out.push("[aimd]");
    out.push(line("incr_step", config.aimd.incr_step));
    out.push(line("decr_mult", config.aimd.decr_mult));
    out.push(line("incr_interval", config.aimd.incr_interval));
    out.push(line("decr_interval", config.aimd.decr_interval));
    out.push("");

    out.push("[overlay]");
    out.push(line("enabled", config.overlay.enabled));
    if (config.overlay.format !== undefined && config.overlay.format !== "") {
        out.push(line("format", config.overlay.format));
    }
    out.push(line("interval_ms", config.overlay.interval_ms));
    out.push("");

    if (config.pipeline.template) {
        out.push("[pipeline]");
        out.push(line("template", config.pipeline.template));
        out.push("");
    }
    if (config.scene.config_path) {
        out.push("[scene]");
        out.push(line("config_path", config.scene.config_path));
        out.push("");
    }
    if (config.control.addr) {
        out.push("[control]");
        if (config.control.addr) out.push(line("addr", config.control.addr));
        out.push("");
    }
    return out.join("\n");
}

export function buildScene(partial: SceneInput = {}): {
    scene: Scene;
    json: string;
} {
    // SceneSchema defaults (canvas, name, per-layer properties) fill gaps.
    const scene = SceneSchema.parse(partial);
    return { scene, json: JSON.stringify(scene, null, 2) };
}
