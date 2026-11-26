# 1.0.0 (2026-11-26)


### Bug Fixes

* **ci:** apt package removal, relative cgo paths, bun test guards ([7d46ee7](https://github.com/majklovec/ctrlirl-coder/commit/7d46ee749e4ee208d9b66411cd4a90371a802485))
* **ci:** GStreamer dev headers, missing arm64 plugin-base dev, Makefile set -e ([9933dd4](https://github.com/majklovec/ctrlirl-coder/commit/9933dd4926af06d2f5f10797e623647080b8bb74))
* **ci:** gstreamer1.0-x for textoverlay, skip CI perf probe, deb/deps ([9a4d3aa](https://github.com/majklovec/ctrlirl-coder/commit/9a4d3aafc2e1865193b2937781cf4e4199e2ed80))
* **ci:** skip 60fps SwiftShader perf tests in CI, qemu smoke best-effort ([db92c33](https://github.com/majklovec/ctrlirl-coder/commit/db92c3379c12613649cddfcdd576c25038a727f3))


### Features

* **ci:** semantic-release versioning, changelog, GitHub releases; drop Pages apt repo ([28db7df](https://github.com/majklovec/ctrlirl-coder/commit/28db7dfd2e75da9502b67d6ac17d40e1b0a20471)), closes [softprops/action-#release](https://github.com/softprops/action-/issues/release)

# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## 0.0.0 (Unreleased)

Initial port of the C `CtrlIRLCoder` live SRT encoder to Go: GStreamer
pipeline with a Go-owned CERALIVE SRT socket (SRTLA), adaptive bitrate
balancing (AIMD) with encoder/overlay/SRT-stall heuristics, multi-layer
scenes (video / image / text / browser layers + filters: hue, saturation,
colorbalance, gamma, hue-rotate, blur, sharpen), Bun/TypeScript bindings
with a WebSocket RPC bridge, deb packaging (amd64 + arm64 cross-compiled
against the CERALIVE SRT fork), and JSON-RPC 2.0 control API
(`system.*`, `scene.*`, `layer.*`, `filter.*`).
