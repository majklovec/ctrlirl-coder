// SceneBuilder — multi-layer scene descriptors for the Go compositor
// (internal/scene). The generated JSON validates against SceneSchema and is
// accepted by scene.Load() with zero errors.
//
// Geometry rule (Go validateGeometry): no layer rect may extend past the
// canvas; a layer without explicit width/height is canvas-sized. The builder
// enforces the same invariant up front.
//
// Z-order rule (Go §7.6): zorder values must be unique; the builder
// auto-assigns the next free zorder when one is omitted.

import { buildScene } from "./config";
import type { Scene } from "./schemas";
import { SceneSchema, type SceneLayer, type FilterSpec } from "./schemas";
import { writeFile } from "./process";

interface LayerOptions {
    id: string;
    x?: number;
    y?: number;
    w?: number;
    h?: number;
    zorder?: number;
    alpha?: number;
    filters?: FilterSpec[];
}

export class SceneBuilder {
    private layers: SceneLayer[] = [];
    private canvas = { width: 1280, height: 720 };

    setCanvas(width: number, height: number): this {
        this.canvas = { width, height };
        return this;
    }

    private nextZorder(): number {
        const used = new Set(this.layers.map((l) => l.properties.zorder));
        let z = 1;
        while (used.has(z)) z++;
        return z;
    }

    private pushLayer(layer: SceneLayer): this {
        // Fail fast on the same geometry invariant the Go binary enforces.
        const p = layer.properties;
        const w = p.width ?? this.canvas.width;
        const h = p.height ?? this.canvas.height;
        if (
            p.xpos < 0 ||
            p.ypos < 0 ||
            p.xpos + w > this.canvas.width ||
            p.ypos + h > this.canvas.height
        ) {
            throw new Error(
                `scene: layer "${layer.id}" rect exceeds canvas ` +
                    `${this.canvas.width}x${this.canvas.height}`,
            );
        }
        this.layers.push(layer);
        return this;
    }

    addVideoLayer(opts: LayerOptions & {
        source: string; // e.g. "/dev/video0"
    }): this {
        return this.pushLayer({
            id: opts.id,
            type: "video",
            source: opts.source,
            properties: {
                zorder: opts.zorder ?? this.nextZorder(),
                xpos: opts.x ?? 0,
                ypos: opts.y ?? 0,
                width: opts.w,
                height: opts.h,
                alpha: opts.alpha ?? 1.0,
            },
            ...(opts.filters ? { filters: opts.filters } : {}),
        });
    }

    addImageLayer(opts: LayerOptions & {
        filePath: string; // local path or file:// URI
    }): this {
        const source = opts.filePath.startsWith("file://")
            ? opts.filePath
            : `file://${opts.filePath}`;
        return this.pushLayer({
            id: opts.id,
            type: "image",
            source,
            properties: {
                zorder: opts.zorder ?? this.nextZorder(),
                xpos: opts.x ?? 0,
                ypos: opts.y ?? 0,
                width: opts.w,
                height: opts.h,
                alpha: opts.alpha ?? 1.0,
            },
            ...(opts.filters ? { filters: opts.filters } : {}),
        });
    }

    addTextLayer(opts: LayerOptions & {
        text: string;
        textOptions?: Record<string, string>;
    }): this {
        return this.pushLayer({
            id: opts.id,
            type: "text",
            source: "",
            text: opts.text,
            ...(opts.textOptions
                ? { "text-options": opts.textOptions }
                : {}),
            properties: {
                zorder: opts.zorder ?? this.nextZorder(),
                xpos: opts.x ?? 0,
                ypos: opts.y ?? 0,
                width: opts.w,
                height: opts.h,
                alpha: opts.alpha ?? 1.0,
            },
            ...(opts.filters ? { filters: opts.filters } : {}),
        });
    }

    addBrowserLayer(opts: LayerOptions & {
        url: string;
        framerate?: number;
        quality?: number;
    }): this {
        return this.pushLayer({
            id: opts.id,
            type: "browser",
            source: opts.url,
            properties: {
                zorder: opts.zorder ?? this.nextZorder(),
                xpos: opts.x ?? 0,
                ypos: opts.y ?? 0,
                width: opts.w,
                height: opts.h,
                alpha: opts.alpha ?? 1.0,
                ...(opts.framerate ? { framerate: opts.framerate } : {}),
                ...(opts.quality ? { quality: opts.quality } : {}),
            },
            ...(opts.filters ? { filters: opts.filters } : {}),
        });
    }

    build(): { scene: Scene; json: string } {
        return buildScene({
            name: "main",
            canvas: this.canvas,
            layers: this.layers,
        });
    }

    writeTo(path: string): Promise<{ scene: Scene; json: string }> {
        const result = this.build();
        return writeFile(path, result.json).then(() => result);
    }
}

export { SceneSchema };
