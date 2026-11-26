package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const sample = `
[general]
min_bitrate = 500
max_bitrate = 6000
balancer = "adaptive"

[srt]
latency = 2500
uri = "srt://10.0.0.1:9000"
streamid = "live1"
passphrase = "secret"

[adaptive]
incr_step = 30
decr_step = 100
incr_interval = 500
decr_interval = 200

[aimd]
incr_step = 50
decr_mult = 0.75
incr_interval = 500
decr_interval = 200

[overlay]
enabled = true
interval_ms = 500

[pipeline]
template = "pipeline/example-full.pipeline"

[scene]
config_path = "pipeline/example-scene.json"
`

func TestLoad(t *testing.T) {
	c, err := Load(writeTemp(t, sample))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.General.MinBitrateKbps != 500 || c.General.MaxBitrateKbps != 6000 {
		t.Errorf("bitrates = %d/%d, want 500/6000", c.General.MinBitrateKbps, c.General.MaxBitrateKbps)
	}
	if c.General.Balancer != "adaptive" {
		t.Errorf("balancer = %q", c.General.Balancer)
	}
	if c.SRT.LatencyMs != 2500 {
		t.Errorf("latency = %d, want 2500", c.SRT.LatencyMs)
	}
	if c.SRT.URI != "srt://10.0.0.1:9000" || c.SRT.StreamID != "live1" || c.SRT.Passphrase != "secret" {
		t.Errorf("srt section not parsed: %+v", c.SRT)
	}
	if c.Overlay.IntervalMS != 500 || !c.Overlay.Enabled {
		t.Errorf("overlay = %+v", c.Overlay)
	}
	if c.Overlay.Format == "" {
		t.Errorf("overlay format default not applied")
	}
	if c.Scene.ConfigPath != "pipeline/example-scene.json" {
		t.Errorf("scene = %+v", c.Scene)
	}
	p := c.BalancerParams(2500, 1316)
	if p.MinBitrate != 500000 || p.MaxBitrate != 6000000 {
		t.Errorf("BalancerParams bps = %d/%d", p.MinBitrate, p.MaxBitrate)
	}
	if p.AdaptiveIncrStep != 30000 || p.AimdDecrMult != 0.75 {
		t.Errorf("tuning not converted: %+v", p)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load(writeTemp(t, "[general]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.General.Balancer != "adaptive" {
		t.Errorf("default balancer = %q", c.General.Balancer)
	}
	if c.SRT.LatencyMs != 2000 {
		t.Errorf("default latency = %d", c.SRT.LatencyMs)
	}
	if c.General.MinBitrateKbps != 500 || c.General.MaxBitrateKbps != 6000 {
		t.Errorf("default bitrates = %d/%d", c.General.MinBitrateKbps, c.General.MaxBitrateKbps)
	}
	if c.Overlay.IntervalMS != 1000 {
		t.Errorf("default overlay interval = %d", c.Overlay.IntervalMS)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]string{
		"max<min":  "[general]\nmin_bitrate = 6000\nmax_bitrate = 500\n",
		"bad algo": "[general]\nbalancer = \"bogus\"\n",
		"bad mult": "[general]\n\n[aimd]\ndecr_mult = 1.5\n",
	}
	for name, body := range cases {
		if _, err := Load(writeTemp(t, body)); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

func TestReloadDiff(t *testing.T) {
	p := writeTemp(t, "[general]\nmin_bitrate=500\nmax_bitrate=6000\n")
	prev, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("[general]\nmin_bitrate=700\nmax_bitrate=8000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	older, next, err := prev.Reload(p)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if older.General.MinBitrateKbps != 500 || next.General.MinBitrateKbps != 700 {
		t.Errorf("reload diff wrong: prev=%d next=%d",
			older.General.MinBitrateKbps, next.General.MinBitrateKbps)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/nope.toml"); err == nil {
		t.Fatalf("expected error for missing file")
	}
}

func TestSRTLAResolved(t *testing.T) {
	body := `sink = "srtla"
[general]
[srtla]
receiver_host = "srtla.example.com"
uplink_ips = ["192.168.1.10", "10.0.0.5"]
mode = "classic"
`
	c, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Sink != "srtla" {
		t.Errorf("sink = %q, want srtla", c.Sink)
	}
	s := c.SRTLA
	if s.ReceiverPort != 5000 {
		t.Errorf("default receiver_port = %d", s.ReceiverPort)
	}
	if s.ConnTimeoutMs != 5000 {
		t.Errorf("default conn_timeout_ms = %d", s.ConnTimeoutMs)
	}
	if len(s.UplinkIPs) != 2 || s.UplinkIPs[1] != "10.0.0.5" {
		t.Errorf("uplink_ips = %v", s.UplinkIPs)
	}
	cfg := s.ToSRTLAConfig()
	if cfg.Mode != "classic" {
		t.Errorf("mode = %q, want classic", cfg.Mode)
	}
	if cfg.ReceiverHost != "srtla.example.com" {
		t.Errorf("receiver_host = %q", cfg.ReceiverHost)
	}
}

func TestSRTLAValidate(t *testing.T) {
	cases := map[string]string{
		"no host": `
sink = "srtla"
[general]
[srtla]
uplink_ips = ["1.2.3.4"]
`,
		"no uplinks": `
sink = "srtla"
[general]
[srtla]
receiver_host = "h"
`,
		"bad sink": `
sink = "hls"
[general]
`,
	}
	for name, body := range cases {
		if _, err := Load(writeTemp(t, body)); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
	// Direct SRT (default sink) with no srtla fields is fine.
	if _, err := Load(writeTemp(t, "[general]\n")); err != nil {
		t.Errorf("default sink should not require srtla fields: %v", err)
	}
	if _, err := Load(writeTemp(t, "sink = \"srt\"\n[general]\n")); err != nil {
		t.Errorf("sink=srt should not require srtla fields: %v", err)
	}
}
