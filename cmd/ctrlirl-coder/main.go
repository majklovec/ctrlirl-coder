// Package main is the ctrlirl-coder entry point: CLI flags, config/scene
// loading, the pure-Go --dry-run validation, and the GStreamer runtime
// (run_gst.go). There is one build: cgo with GStreamer and the CERALIVE SRT
// fork.
package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

type options struct {
	configPath string
	scenePath  string
	pipeline   string
	dryRun     bool
	apiAddr    string
}

func parseFlags(args []string) (*options, error) {
	fs := flag.NewFlagSet("ctrlirl-coder", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := &options{}
	fs.StringVar(&o.configPath, "config", "", "path to TOML configuration file")
	fs.StringVar(&o.scenePath, "scene", "", "path to scene JSON file (multi-layer)")
	fs.StringVar(&o.pipeline, "pipeline", "", "path to pipeline template file")
	fs.BoolVar(&o.dryRun, "dry-run", false, "validate configuration and exit (no GStreamer)")
	fs.StringVar(&o.apiAddr, "api-addr", "", "listen address for the shared HTTP API (JSON-RPC + /metrics; overrides [control] addr)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return o, nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: ctrlirl-coder --config <toml> [--pipeline <file>] [--scene <json>] [--dry-run]

  --config    TOML configuration (ctrlirl_coder schema)
  --pipeline  pipeline template (gst-launch style, terminal appsink name=appsink)
  --scene     JSON scene descriptor (multi-layer compositor)
  --api-addr  listen address for the shared HTTP API (e.g. 127.0.0.1:8899)
  --dry-run   validate config, scene and pipeline contract, then exit (pure Go)`)
}

// pipelineContract checks the template text for the elements the contract
// requires (spec §2). It is a pure-Go string check so the dry-run path can
// validate without GStreamer. The checks run in a fixed order so the output
// is deterministic.
func pipelineContract(text string) (missingMandatory, present []string) {
	type check struct {
		name string
		alts []string
	}
	checks := []check{
		// The pipeline must end in a terminal appsink named `appsink`; Go
		// pulls its buffers (gst_app_sink_pull_sample) and sends SRT itself
		// over the Go-owned socket (no in-pipeline SRT sink) — spec §2.
		{"bytetap", []string{"appsink"}},
		{"encoder", []string{"venc_bps", "venc_kbps"}},
	}
	for _, c := range checks {
		found := false
		for _, alt := range c.alts {
			if strings.Contains(text, alt) {
				found = true
				present = append(present, alt)
				break
			}
		}
		if !found {
			missingMandatory = append(missingMandatory, c.name)
		}
	}
	for _, opt := range []string{"overlay", "fakesink", "v_delay", "a_delay", "ptsfixup"} {
		if strings.Contains(text, opt) {
			present = append(present, opt)
		}
	}
	return missingMandatory, present
}

// logLevel reads CTRLIRL_LOG_LEVEL (debug|info|warn|error) for the slog
// handler; defaults to info.
func logLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CTRLIRL_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()})))

	o, err := parseFlags(os.Args[1:])
	if err != nil {
		usage()
		os.Exit(2)
	}
	if err := run(o); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
