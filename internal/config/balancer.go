package config

import "ctrlirl-coder/internal/balancer"

// BalancerParams converts the TOML configuration (Kbps + the adaptive/aimd
// tuning sections) into balancer.Params (bps) for the balancer package.
//
// srtLatencyMs and srtPktSize are link-geometry values known at runtime
// (negotiated / chosen) and are supplied by the caller.
func (c *Config) BalancerParams(srtLatencyMs, srtPktSize int) balancer.Params {
	return balancer.Params{
		MinBitrate:   c.General.MinBitrateKbps * 1000,
		MaxBitrate:   c.General.MaxBitrateKbps * 1000,
		SRTLatencyMs: srtLatencyMs,
		SRTPktSize:   srtPktSize,

		AdaptiveIncrStep:     c.Adaptive.IncrStep * 1000,
		AdaptiveDecrStep:     c.Adaptive.DecrStep * 1000,
		AdaptiveIncrInterval: c.Adaptive.IncrInterval,
		AdaptiveDecrInterval: c.Adaptive.DecrInterval,

		AimdIncrStep:     c.Aimd.IncrStep * 1000,
		AimdDecrMult:     c.Aimd.DecrMult,
		AimdIncrInterval: c.Aimd.IncrInterval,
		AimdDecrInterval: c.Aimd.DecrInterval,
	}
}
