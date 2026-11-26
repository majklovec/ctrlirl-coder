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
