// Shared TypeScript types for @ctrlirl/ctrlirl-coder.

export type Balancer = "adaptive" | "fixed" | "aimd";

export type LogLevel = "debug" | "info" | "warning" | "error";

export type Hardware = "jetson" | "rk3588" | "n100" | "generic";

export type Source = "test" | "v4l2" | "file";

export type LayerType = "video" | "image" | "text" | "browser";
