## [1.1.1](https://github.com/majklovec/ctrlirl-coder/compare/v1.1.0...v1.1.1) (2026-11-27)


### Bug Fixes

* **ci:** main component from Release runs without completion filter (push-run race) ([cbb3a3e](https://github.com/majklovec/ctrlirl-coder/commit/cbb3a3eeed76fbc1c4dbceca4e0939336591a4b6))

# [1.1.0](https://github.com/majklovec/ctrlirl-coder/compare/v1.0.3...v1.1.0) (2026-11-27)


### Features

* **ci:** action bumps (checkout/setup-go v7), Pages apt repo with main+main-ci components ([cc80f48](https://github.com/majklovec/ctrlirl-coder/commit/cc80f4879b265d0b8080b353d51762362af044d2))

## [1.0.3](https://github.com/majklovec/ctrlirl-coder/compare/v1.0.2...v1.0.3) (2026-11-27)


### Bug Fixes

* **ci:** no-bump commits skip release jobs (detect step, always()) ([3000b05](https://github.com/majklovec/ctrlirl-coder/commit/3000b05c0e1a8523f985965fee4b8bcbfc53e794))

## [1.0.2](https://github.com/majklovec/ctrlirl-coder/compare/v1.0.1...v1.0.2) (2026-11-26)


### Bug Fixes

* **ci:** 30s frame deadline for rotate-corners test, release ships binaries+debs both arches; v1.0.0 re-cut ([d292942](https://github.com/majklovec/ctrlirl-coder/commit/d2929423e424304af7fe85a95db18b25f69bca6d))

## [1.0.1](https://github.com/majklovec/ctrlirl-coder/compare/v1.0.0...v1.0.1) (2026-11-26)


### Bug Fixes

* **ci:** keep main-only gating on dist jobs after workflow cleanup ([7a3e659](https://github.com/majklovec/ctrlirl-coder/commit/7a3e659bf26ba4de344a23b6ceb59bb6fede5b02))

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
