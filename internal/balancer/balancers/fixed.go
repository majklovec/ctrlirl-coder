package balancers

import "ctrlirl-coder/internal/sinks"

// Fixed is the "fixed" balancer: it never adapts. The controller always runs
// at the maximum configured bitrate (matching the original, where the fixed
// algorithm starts and stays at max_bitrate). It is still a no-op on the
// current value so tests can assert stability from any starting point.
type Fixed struct{}

// NewFixed returns a fresh Fixed balancer.
func NewFixed() *Fixed { return &Fixed{} }

func (f *Fixed) Name() string { return "fixed" }

// Next returns the current value unchanged (clamped): the fixed operating
// point. The controller initialises currentBPS to MaxBitrate for a fixed
// stream, matching the original behaviour.
func (f *Fixed) Next(currentBPS int, _ sinks.Stats, p Params) int {
	return p.clamp(currentBPS)
}
