package config

import "ctrlirl-coder/internal/sinks/srtla"

// SRTLAParams mirrors the [srtla] table and the SRTLA sender configuration.
// The top-level `sink` selector ("srt" | "srtla") chooses which output path
// the process uses; when it is "srtla" the rest of this table is required.
type SRTLAParams struct {
	ReceiverHost  string   `toml:"receiver_host"`
	ReceiverPort  int      `toml:"receiver_port"`
	UplinkIPs     []string `toml:"uplink_ips"`
	Mode          string   `toml:"mode"`
	ConnTimeoutMs int      `toml:"conn_timeout_ms"`
}

// ToSRTLAConfig maps the TOML table onto the sender configuration with
// defaults filled and the mode canonicalized.
func (s SRTLAParams) ToSRTLAConfig() srtla.Config {
	return srtla.Config{
		ReceiverHost:  s.ReceiverHost,
		ReceiverPort:  s.ReceiverPort,
		UplinkIPs:     s.UplinkIPs,
		Mode:          s.Mode,
		ConnTimeoutMs: s.ConnTimeoutMs,
	}.Normalized()
}
