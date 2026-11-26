// Zod schemas for the CtrlIRLCoder-Go configuration (TOML) and scene (JSON).
//
// These mirror the Go backend's own schema (internal/config, internal/scene)
// exactly: the same table names, key names and constraints, so that values
// serialized here are accepted by config.Load() / scene.Load() unchanged.

import { z } from "zod";

// --- Config (TOML) ---

export const GeneralSchema = z.object({
    min_bitrate: z.number().int().min(1).default(500),
    max_bitrate: z.number().int().min(1).default(6000),
    balancer: z.enum(["adaptive", "fixed", "aimd"]).default("adaptive"),
    frame_stall_ms: z.number().int().min(0).default(3000),
});

export const SRTSchema = z.object({
    uri: z.string().default("srt://127.0.0.1:9000"),
    latency: z.number().int().min(0).default(2000),
    streamid: z.string().optional(),
    passphrase: z.string().optional(),
    max_bandwidth_kbps: z.number().int().min(0).default(0),
});

export const SrtlaSchema = z.object({
    // Required only when sink = "srtla" (enforced by the Go side); kept
    // optional here so an unused [srtla] table parses without a receiver.
    receiver_host: z.string().optional(),
    receiver_port: z.number().int().min(0).default(5000),
    uplink_ips: z.array(z.string()).default([]),
    mode: z.enum(["enhanced", "classic"]).default("enhanced"),
    conn_timeout_ms: z.number().int().min(0).default(5000),
});

export const AdaptiveSchema = z.object({
    incr_step: z.number().int().min(0).default(30),
    decr_step: z.number().int().min(0).default(100),
    incr_interval: z.number().int().min(0).default(500),
    decr_interval: z.number().int().min(0).default(200),
});

export const AimdSchema = z.object({
    incr_step: z.number().int().min(0).default(100),
    decr_mult: z.number().min(0).max(0.999).default(0.5),
    incr_interval: z.number().int().min(0).default(1000),
    decr_interval: z.number().int().min(0).default(100),
});

export const OverlaySchema = z.object({
    enabled: z.boolean().default(false),
    format: z.string().optional(),
    interval_ms: z.number().int().min(0).default(1000),
});

export const PipelineSchema = z.object({
    template: z.string().optional(),
});

export const SceneTableSchema = z.object({
    config_path: z.string().optional(),
});

export const ControlSchema = z.object({
    addr: z.string().optional(),
});

const ConfigShape = z
    .object({
        sink: z.enum(["srt", "srtla"]).default("srt"),
        general: GeneralSchema,
        srt: SRTSchema,
        srtla: SrtlaSchema,
        adaptive: AdaptiveSchema,
        aimd: AimdSchema,
        overlay: OverlaySchema,
        pipeline: PipelineSchema,
        scene: SceneTableSchema,
        control: ControlSchema,
    })
    .superRefine((cfg, ctx) => {
        if (cfg.general.max_bitrate < cfg.general.min_bitrate) {
            ctx.addIssue({
                code: "custom",
                path: ["general", "max_bitrate"],
                message: "max_bitrate must be >= min_bitrate",
            });
        }
        if (cfg.sink === "srtla") {
            if (!cfg.srtla.receiver_host) {
                ctx.addIssue({
                    code: "custom",
                    path: ["srtla", "receiver_host"],
                    message: "srtla.receiver_host is required when sink = \"srtla\"",
                });
            }
            if (cfg.srtla.uplink_ips.length === 0) {
                ctx.addIssue({
                    code: "custom",
                    path: ["srtla", "uplink_ips"],
                    message: "srtla.uplink_ips must not be empty when sink = \"srtla\"",
                });
            }
        }
    });

export type CtrlIRLCoderConfig = z.infer<typeof ConfigShape>;

// Loose input type: every table and field is optional (Zod defaults fill
// in the rest). `buildCtrlIRLCoderConfig` takes this, returns CtrlIRLCoderConfig.
export type CtrlIRLCoderConfigInput = Partial<{
    sink: "srt" | "srtla";
    general: Partial<z.infer<typeof GeneralSchema>>;
    srt: Partial<z.infer<typeof SRTSchema>>;
    srtla: Partial<z.infer<typeof SrtlaSchema>>;
    adaptive: Partial<z.infer<typeof AdaptiveSchema>>;
    aimd: Partial<z.infer<typeof AimdSchema>>;
    overlay: Partial<z.infer<typeof OverlaySchema>>;
    pipeline: Partial<z.infer<typeof PipelineSchema>>;
    scene: Partial<z.infer<typeof SceneTableSchema>>;
    control: Partial<z.infer<typeof ControlSchema>>;
}>;

// A partial config leaves tables missing. Pre-fill absent tables with
// {} (before parsing, so per-field defaults still apply — a `.default({})`
// on the table would suppress them in Zod v4) and let the shape validate
// the rest.
export const CtrlIRLCoderConfigSchema: z.ZodType<CtrlIRLCoderConfig> =
    z.preprocess((input) => {
        if (input === null || typeof input !== "object") return input;
        const src = input as Record<string, unknown>;
        return {
            sink: src.sink ?? "srt",
            general: src.general ?? {},
            srt: src.srt ?? {},
            srtla: src.srtla ?? {},
            adaptive: src.adaptive ?? {},
            aimd: src.aimd ?? {},
            overlay: src.overlay ?? {},
            pipeline: src.pipeline ?? {},
            scene: src.scene ?? {},
            control: src.control ?? {},
        };
    }, ConfigShape) as z.ZodType<CtrlIRLCoderConfig>;

// --- Scene (JSON) ---

export const LayerPropertiesSchema = z.object({
    zorder: z.number().int().default(0),
    xpos: z.number().int().default(0),
    ypos: z.number().int().default(0),
    width: z.number().int().optional(),
    height: z.number().int().optional(),
    alpha: z.number().min(0).max(1).default(1.0),
    framerate: z.number().int().optional(), // browser layers
    quality: z.number().int().optional(), // browser layers
});

// The Go filter registry (internal/scene/filters.go).
export const FILTER_KINDS = [
    "videoscale",
    "videocrop",
    "videobalance",
    "videobox",
    "videoflip",
    "videorotate",
    "effectv",
    "volume",
    "audiodynamic",
    "pitch",
] as const;

export type FilterKind = (typeof FILTER_KINDS)[number];

export const FilterSpecSchema = z.object({
    kind: z.enum(FILTER_KINDS),
    params: z.record(z.string(), z.union([z.number(), z.string()])).default({}),
});

export const SceneLayerSchema = z.object({
    id: z.string().min(1),
    type: z.enum(["video", "image", "text", "browser", "testcode"]),
    source: z.string(),
    text: z.string().optional(), // text layers
    "text-options": z.record(z.string(), z.string()).optional(),
    // Full-object default (Zod v4 suppresses field defaults on {} defaults);
    // Go scene.normalize fills any remaining gaps.
    properties: LayerPropertiesSchema.default({
        zorder: 0,
        xpos: 0,
        ypos: 0,
        alpha: 1.0,
    }),
    filters: z.array(FilterSpecSchema).optional(),
});

export const SceneSchema = z.object({
    name: z.string().default("main"),
    // Full-object default: a bare `.default({})` would suppress the
    // per-field defaults (Zod v4).
    canvas: z
        .object({
            width: z.number().int().min(1).default(1280),
            height: z.number().int().min(1).default(720),
        })
        .default({ width: 1280, height: 720 }),
    layers: z.array(SceneLayerSchema),
});

export type Scene = z.infer<typeof SceneSchema>;
export type SceneLayer = z.infer<typeof SceneLayerSchema>;
export type FilterSpec = z.infer<typeof FilterSpecSchema>;

// Loose input types: SceneBuilder passes partial layers (zorder/xpos/ypos/
// alpha filled by defaults). `Partial` is shallow, so nest properties
// explicitly.
export type SceneLayerInput = Omit<Partial<SceneLayer>, "properties"> & {
    properties?: Partial<NonNullable<SceneLayer["properties"]>>;
};
export type SceneInput = Omit<Partial<Scene>, "layers"> & {
    layers?: SceneLayerInput[];
};
