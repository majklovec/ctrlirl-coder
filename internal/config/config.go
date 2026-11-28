// Package config loads the TOML configuration file: the original ctrlirl_coder
// schema (general / srt / adaptive / aimd) plus the [overlay], [pipeline] and
// [scene] tables added for the multi-layer compositor.
package config

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// Config is the full application configuration. Sub-structs map to TOML
// tables by their `toml:"..."` tag; key names match the original
// ctrlirl_coder.conf schema so values carry over directly (only the format
// changes).
type Config struct {
	// Sink selects the output path: "srt" (direct SRT socket) or "srtla"
	// (SRT Link Aggregation over multiple uplinks). Defaults to "srt".
	Sink     string         `toml:"sink"`
	General  GeneralParams  `toml:"general"`
	SRT      SRTParams      `toml:"srt"`
	SRTLA    SRTLAParams    `toml:"srtla"`
	Adaptive AdaptiveParams `toml:"adaptive"`
	Aimd     AimdParams     `toml:"aimd"`
	Overlay  Overlay        `toml:"overlay"`
	Pipeline Pipeline       `toml:"pipeline"`
	Scene    Scene          `toml:"scene"`
	Control  Control        `toml:"control"`
}

// Sink selectors.
const (
	SinkSRT   = "srt"
	SinkSRTLA = "srtla"
)

// GeneralParams mirrors the [general] table.
type GeneralParams struct {
	MinBitrateKbps int    `toml:"min_bitrate"`
	MaxBitrateKbps int    `toml:"max_bitrate"`
	Balancer       string `toml:"balancer"`
	// FrameStallMS is the frame-production stall threshold (ms): no frame
	// pulled from the appsink for this long flags the pipeline as
	// zombie-encoding and the adaptive balancer drops to the minimum bitrate
	// (the original frame_liveness watchdog gate). 0 → 3000 ms default.
	FrameStallMS int `toml:"frame_stall_ms"`
}

// SRTParams mirrors the [srt] table. URI is the authoritative SRT destination:
// the pipeline template carries no SRT sink (it ends in a terminal appsink),
// so the process owns the socket and this field says where to send.
type SRTParams struct {
	LatencyMs  int    `toml:"latency"`
	URI        string `toml:"uri"`
	StreamID   string `toml:"streamid"`
	Passphrase string `toml:"passphrase"`
	// MaxBandwidthKbps, when > 0, caps the SRT send rate (SRTO_MAXBW, kbit/s).
	MaxBandwidthKbps int `toml:"max_bandwidth_kbps"`
}

// AdaptiveParams mirrors the [adaptive] table (steps in Kbps, intervals ms).
type AdaptiveParams struct {
	IncrStep     int `toml:"incr_step"`
	DecrStep     int `toml:"decr_step"`
	IncrInterval int `toml:"incr_interval"`
	DecrInterval int `toml:"decr_interval"`
}

// AimdParams mirrors the [aimd] table (step in Kbps, mult unitless).
type AimdParams struct {
	IncrStep     int     `toml:"incr_step"`
	DecrMult     float64 `toml:"decr_mult"`
	IncrInterval int     `toml:"incr_interval"`
	DecrInterval int     `toml:"decr_interval"`
}

// Overlay configures the on-screen stats text (updated at ~1 Hz).
type Overlay struct {
	Enabled    bool   `toml:"enabled"`
	Format     string `toml:"format"`
	IntervalMS int    `toml:"interval_ms"`
}

// Pipeline points at a .pipeline template file (single-layer, original-style).
type Pipeline struct {
	Template string `toml:"template"`
}

// Scene points at a JSON scene descriptor (multi-layer compositor).
type Scene struct {
	ConfigPath string `toml:"config_path"`
}

// Control configures the shared HTTP API server: one listener carrying
// the JSON-RPC 2.0 control API (POST /rpc) and the /metrics endpoint
// (GET /metrics, Prometheus text exposition format).
type Control struct {
	// Addr is the listen address (e.g. "127.0.0.1:8899"). Empty: the API
	// server is disabled. A --api-addr flag overrides it.
	Addr string `toml:"addr" json:"addr"`
}

// Load reads and validates the TOML file at path. Invalid values fail fast
// with a descriptive error.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("config: load %s: %w", path, err)
	}
	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Sink == "" {
		c.Sink = SinkSRT
	}
	if c.General.Balancer == "" {
		c.General.Balancer = "adaptive"
	}
	if c.SRT.LatencyMs <= 0 {
		c.SRT.LatencyMs = 2000
	}
	if c.SRTLA.ReceiverPort <= 0 {
		c.SRTLA.ReceiverPort = 5000
	}
	if c.SRTLA.Mode == "" {
		c.SRTLA.Mode = "enhanced"
	}
	if c.SRTLA.ConnTimeoutMs <= 0 {
		c.SRTLA.ConnTimeoutMs = 5000
	}
	if c.General.MinBitrateKbps <= 0 {
		c.General.MinBitrateKbps = 500
	}
	if c.General.MaxBitrateKbps <= 0 {
		c.General.MaxBitrateKbps = 6000
	}
	if c.General.FrameStallMS <= 0 {
		c.General.FrameStallMS = 3000
	}
	if c.Overlay.IntervalMS <= 0 {
		c.Overlay.IntervalMS = 1000
	}
	if c.Overlay.Format == "" {
		c.Overlay.Format = DefaultOverlayFormat
	}
}

// DefaultOverlayFormat matches the original overlay_ui.c layout:
//
//	b: <set_kbps>/<throughput_mbps> rtt: <rtt>/<min>/<max> bs: <bs>/<t1>/<t2>/<t3>
const DefaultOverlayFormat = "  b: %5d/%5.0f rtt: %3d/%3d/%3d bs: %3d/%3d/%3d/%3d"

// Validate enforces sane cross-field invariants and fails fast.
func (c *Config) Validate() error {
	if c.General.MinBitrateKbps <= 0 {
		return fmt.Errorf("config: min_bitrate must be > 0 (got %d)", c.General.MinBitrateKbps)
	}
	if c.General.MaxBitrateKbps < c.General.MinBitrateKbps {
		return fmt.Errorf("config: max_bitrate (%d) must be >= min_bitrate (%d)",
			c.General.MaxBitrateKbps, c.General.MinBitrateKbps)
	}
	switch c.General.Balancer {
	case "adaptive", "fixed", "aimd":
	default:
		return fmt.Errorf("config: unknown balancer %q (want adaptive|fixed|aimd)", c.General.Balancer)
	}
	switch c.Sink {
	case SinkSRT, SinkSRTLA:
	default:
		return fmt.Errorf("config: unknown sink %q (want srt|srtla)", c.Sink)
	}
	if c.Aimd.DecrMult < 0 || c.Aimd.DecrMult >= 1 {
		return fmt.Errorf("config: aimd.decr_mult must be in [0,1) (got %v)", c.Aimd.DecrMult)
	}
	if c.Sink == SinkSRTLA {
		if c.SRTLA.ReceiverHost == "" {
			return fmt.Errorf("config: srtla.receiver_host is required when sink = %q", SinkSRTLA)
		}
		if len(c.SRTLA.UplinkIPs) == 0 {
			return fmt.Errorf("config: srtla.uplink_ips must not be empty when sink = %q", SinkSRTLA)
		}
	}
	return nil
}

// MinBitratebps returns the configured minimum in bits/second.
func (c *Config) MinBitratebps() int { return c.General.MinBitrateKbps * 1000 }

// MaxBitratebps returns the configured maximum in bits/second.
func (c *Config) MaxBitratebps() int { return c.General.MaxBitrateKbps * 1000 }
