## [1.2.12](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.11...v1.2.12) (2026-11-27)


### Performance Improvements

* **ci:** run dist-amd64 + dist-arm64 in parallel ([79ca855](https://github.com/majklovec/ctrlirl-coder/commit/79ca85548bc41442b19e9bb06a2f3f54556f22d8))

## [1.2.11](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.10...v1.2.11) (2026-11-27)


### Bug Fixes

* **ci:** ship hash-less apt Release — apt 3.x skips the Packages index when ([7a160a0](https://github.com/majklovec/ctrlirl-coder/commit/7a160a078f53a05a318026d5859135f9d3cb8304))

## [1.2.10](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.9...v1.2.10) (2026-11-27)


### Bug Fixes

* **ci:** add SHA256 hash block to apt Release file ([6d001a1](https://github.com/majklovec/ctrlirl-coder/commit/6d001a1fdbc5b81735f7bde3b5e18de345eca793))

## [1.2.9](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.8...v1.2.9) (2026-11-27)


### Bug Fixes

* **ci:** canonical apt layout, main-only, fix scanpkg/redirect bugs ([8d141b7](https://github.com/majklovec/ctrlirl-coder/commit/8d141b76932383cb2bd247a7eaee6f87b8f429e8))

## [1.2.8](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.7...v1.2.8) (2026-11-27)


### Bug Fixes

* **ci:** pick latest release with .deb assets for main component ([7e0b3ae](https://github.com/majklovec/ctrlirl-coder/commit/7e0b3aed0f29c73d68a6590b0332840a00588234))

## [1.2.7](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.6...v1.2.7) (2026-11-27)


### Bug Fixes

* **ci:** read main component from published release assets, not run artifacts ([f1463e0](https://github.com/majklovec/ctrlirl-coder/commit/f1463e006ec01cdc6e9255fc410f9b7c5f638560))

## [1.2.6](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.5...v1.2.6) (2026-11-27)


### Bug Fixes

* **ci:** release proof = Release job success, not step outputs ([8157bda](https://github.com/majklovec/ctrlirl-coder/commit/8157bda6cb0c69e732f3ba0d7bd28a469a57ea1e))
* **ci:** sync branch+tags before semantic-release to prevent duplicate version bumps ([a4a529b](https://github.com/majklovec/ctrlirl-coder/commit/a4a529bb0cbd928b9a754b4e6c203ca672701001))

## [1.2.6](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.5...v1.2.6) (2026-11-27)


### Bug Fixes

* **ci:** release proof = Release job success, not step outputs ([8157bda](https://github.com/majklovec/ctrlirl-coder/commit/8157bda6cb0c69e732f3ba0d7bd28a469a57ea1e))

## [1.2.5](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.4...v1.2.5) (2026-11-27)


### Bug Fixes

* **ci:** key latest-release scan on Release job success ([0dfd3e9](https://github.com/majklovec/ctrlirl-coder/commit/0dfd3e90bdb70170fa4b5fc0e2032c6f0adc80eb))

## [1.2.4](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.3...v1.2.4) (2026-11-27)


### Bug Fixes

* **ci:** split repo artifact at correct depth (dist/ subdir) ([e3d608a](https://github.com/majklovec/ctrlirl-coder/commit/e3d608a8c117d9b374eca958c6fe2f2ffc1f276a))

## [1.2.3](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.2...v1.2.3) (2026-11-27)


### Bug Fixes

* **ci:** download release payloads by explicit name, not pattern ([ec9b455](https://github.com/majklovec/ctrlirl-coder/commit/ec9b45555854694273941576a607394246e6fe94))

## [1.2.2](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.1...v1.2.2) (2026-11-27)


### Bug Fixes

* **ci:** .data on listWorkflowRuns, apt retry for transient 403/502 mirrors ([71e97e0](https://github.com/majklovec/ctrlirl-coder/commit/71e97e0133bf23020c28de39d9c6ab4c0c5fc3c7))
* **ci:** apt retry loop bash syntax + typed client .data ([f40c3bc](https://github.com/majklovec/ctrlirl-coder/commit/f40c3bcc9169344be655d9d90ba2301aa15efd6a))
* **ci:** artifact shadowing, deb version, split guard, arm64 CI deb ([faa9299](https://github.com/majklovec/ctrlirl-coder/commit/faa92993ddf73ee9e5ed9160503a793689347e46))
* **ci:** merge step if-conditions into the step item (schema) ([1855184](https://github.com/majklovec/ctrlirl-coder/commit/18551848a869110ee2afbc411d9b07e0f044f4b5))
* **ci:** robust API list extraction + libfile sed ([8312c99](https://github.com/majklovec/ctrlirl-coder/commit/8312c99269252e636698f9b388b04ceea6c21063))
* **ci:** rpath $ORIGIN under set -u + defensive API unwrap ([c5c5d82](https://github.com/majklovec/ctrlirl-coder/commit/c5c5d8214ee53c8c458f551e62caee472bd15321))
* **ci:** single workflow — version, dist, release, Pages in one ci.yml ([2c36c68](https://github.com/majklovec/ctrlirl-coder/commit/2c36c682509e74fb85232cb12802706fad085478)), closes [action-#release](https://github.com/action-/issues/release)

## [1.2.2](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.1...v1.2.2) (2026-11-27)


### Bug Fixes

* **ci:** .data on listWorkflowRuns, apt retry for transient 403/502 mirrors ([71e97e0](https://github.com/majklovec/ctrlirl-coder/commit/71e97e0133bf23020c28de39d9c6ab4c0c5fc3c7))
* **ci:** apt retry loop bash syntax + typed client .data ([f40c3bc](https://github.com/majklovec/ctrlirl-coder/commit/f40c3bcc9169344be655d9d90ba2301aa15efd6a))
* **ci:** merge step if-conditions into the step item (schema) ([1855184](https://github.com/majklovec/ctrlirl-coder/commit/18551848a869110ee2afbc411d9b07e0f044f4b5))
* **ci:** robust API list extraction + libfile sed ([8312c99](https://github.com/majklovec/ctrlirl-coder/commit/8312c99269252e636698f9b388b04ceea6c21063))
* **ci:** rpath $ORIGIN under set -u + defensive API unwrap ([c5c5d82](https://github.com/majklovec/ctrlirl-coder/commit/c5c5d8214ee53c8c458f551e62caee472bd15321))
* **ci:** single workflow — version, dist, release, Pages in one ci.yml ([2c36c68](https://github.com/majklovec/ctrlirl-coder/commit/2c36c682509e74fb85232cb12802706fad085478)), closes [action-#release](https://github.com/action-/issues/release)

## [1.2.2](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.1...v1.2.2) (2026-11-27)


### Bug Fixes

* **ci:** .data on listWorkflowRuns, apt retry for transient 403/502 mirrors ([71e97e0](https://github.com/majklovec/ctrlirl-coder/commit/71e97e0133bf23020c28de39d9c6ab4c0c5fc3c7))
* **ci:** apt retry loop bash syntax + typed client .data ([f40c3bc](https://github.com/majklovec/ctrlirl-coder/commit/f40c3bcc9169344be655d9d90ba2301aa15efd6a))
* **ci:** merge step if-conditions into the step item (schema) ([1855184](https://github.com/majklovec/ctrlirl-coder/commit/18551848a869110ee2afbc411d9b07e0f044f4b5))
* **ci:** rpath $ORIGIN under set -u + defensive API unwrap ([c5c5d82](https://github.com/majklovec/ctrlirl-coder/commit/c5c5d8214ee53c8c458f551e62caee472bd15321))
* **ci:** single workflow — version, dist, release, Pages in one ci.yml ([2c36c68](https://github.com/majklovec/ctrlirl-coder/commit/2c36c682509e74fb85232cb12802706fad085478)), closes [action-#release](https://github.com/action-/issues/release)

## [1.2.2](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.1...v1.2.2) (2026-11-27)


### Bug Fixes

* **ci:** .data on listWorkflowRuns, apt retry for transient 403/502 mirrors ([71e97e0](https://github.com/majklovec/ctrlirl-coder/commit/71e97e0133bf23020c28de39d9c6ab4c0c5fc3c7))
* **ci:** apt retry loop bash syntax + typed client .data ([f40c3bc](https://github.com/majklovec/ctrlirl-coder/commit/f40c3bcc9169344be655d9d90ba2301aa15efd6a))
* **ci:** merge step if-conditions into the step item (schema) ([1855184](https://github.com/majklovec/ctrlirl-coder/commit/18551848a869110ee2afbc411d9b07e0f044f4b5))
* **ci:** single workflow — version, dist, release, Pages in one ci.yml ([2c36c68](https://github.com/majklovec/ctrlirl-coder/commit/2c36c682509e74fb85232cb12802706fad085478)), closes [action-#release](https://github.com/action-/issues/release)

## [1.2.2](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.1...v1.2.2) (2026-11-27)


### Bug Fixes

* **ci:** .data on listWorkflowRuns, apt retry for transient 403/502 mirrors ([71e97e0](https://github.com/majklovec/ctrlirl-coder/commit/71e97e0133bf23020c28de39d9c6ab4c0c5fc3c7))
* **ci:** merge step if-conditions into the step item (schema) ([1855184](https://github.com/majklovec/ctrlirl-coder/commit/18551848a869110ee2afbc411d9b07e0f044f4b5))
* **ci:** single workflow — version, dist, release, Pages in one ci.yml ([2c36c68](https://github.com/majklovec/ctrlirl-coder/commit/2c36c682509e74fb85232cb12802706fad085478)), closes [action-#release](https://github.com/action-/issues/release)

## [1.2.2](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.1...v1.2.2) (2026-11-27)


### Bug Fixes

* **ci:** merge step if-conditions into the step item (schema) ([1855184](https://github.com/majklovec/ctrlirl-coder/commit/18551848a869110ee2afbc411d9b07e0f044f4b5))
* **ci:** single workflow — version, dist, release, Pages in one ci.yml ([2c36c68](https://github.com/majklovec/ctrlirl-coder/commit/2c36c682509e74fb85232cb12802706fad085478)), closes [action-#release](https://github.com/action-/issues/release)

## [1.2.1](https://github.com/majklovec/ctrlirl-coder/compare/v1.2.0...v1.2.1) (2026-11-27)


### Bug Fixes

* **ci:** Pages repo — one deploy carries main+main-ci+binaries, only successful releases ([173a3f3](https://github.com/majklovec/ctrlirl-coder/commit/173a3f3f189e23c2b508f96a2ee3d0088e8617b4))

# [1.2.0](https://github.com/majklovec/ctrlirl-coder/compare/v1.1.2...v1.2.0) (2026-11-27)


### Bug Fixes

* **ci:** doc the stale-push release cancellation window; re-cut v1.2.0 ([6971e5e](https://github.com/majklovec/ctrlirl-coder/commit/6971e5ed8a0722977b51f2e596a0307fc2f168e5))


### Features

* **ci:** Pages index.html listing published .debs ([0c10063](https://github.com/majklovec/ctrlirl-coder/commit/0c10063d1f2b2725802b22cea1cbf8402cf2b3d0))

# [1.2.0](https://github.com/majklovec/ctrlirl-coder/compare/v1.1.2...v1.2.0) (2026-11-27)


### Features

* **ci:** Pages index.html listing published .debs ([0c10063](https://github.com/majklovec/ctrlirl-coder/commit/0c10063d1f2b2725802b22cea1cbf8402cf2b3d0))

## [1.1.2](https://github.com/majklovec/ctrlirl-coder/compare/v1.1.1...v1.1.2) (2026-11-27)


### Bug Fixes

* **ci:** upload-pages-artifact v5, deploy-pages v5 (Node 20 deprecation) ([f9a27c7](https://github.com/majklovec/ctrlirl-coder/commit/f9a27c7fe764c8dd6db4fbb2c2a6c4de33483fad))

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
