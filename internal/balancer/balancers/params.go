// Package balancers contains the concrete bitrate-control implementations.
package balancers

// Params carries every tunable that an algorithm needs. It is assembled by the
// controller from config.Config so these implementations have no dependency on
// the TOML config schema. Zero values are normalised by each algorithm to the
// original defaults.
type Params struct {
	// Limits and link geometry.
	MinBitrate   int // bps
	MaxBitrate   int // bps
	SRTLatencyMs int // negotiated SRT latency (ms)
	SRTPktSize   int // SRT packet size (bytes)

	// Adaptive tuning (bps / ms).
	AdaptiveIncrStep     int
	AdaptiveDecrStep     int
	AdaptiveIncrInterval int
	AdaptiveDecrInterval int

	// AIMD tuning.
	AimdIncrStep     int
	AimdDecrMult     float64
	AimdIncrInterval int
	AimdDecrInterval int
}

// clamp bounds bps to [MinBitrate, MaxBitrate].
func (p Params) clamp(bps int) int {
	if bps < p.MinBitrate {
		return p.MinBitrate
	}
	if bps > p.MaxBitrate {
		return p.MaxBitrate
	}
	return bps
}
