package balancers

import "ctrlirl-coder/internal/sinks"

const (
	defaultAimdIncrStep     = 50_000
	defaultAimdDecrMult     = 0.75
	defaultAimdIncrInterval = 500
	defaultAimdDecrInterval = 200
)

// AIMD is an additive-increase/multiplicative-decrease bitrate balancer.
// Loss/retransmission samples decrease immediately; otherwise the configured
// additive step is applied once each increase interval.
type AIMD struct {
	minBR, maxBR       int
	incrStep           int
	decrMult           float64
	incrInterval       int
	decrInterval       int
	prevPktLoss        int64
	prevPktRetrans     int64
	nextIncr, nextDecr uint64
}

// NewAIMD constructs an AIMD balancer.
func NewAIMD(p Params) *AIMD {
	a := &AIMD{}
	a.updateParams(p)
	return a
}

func (a *AIMD) Name() string { return "aimd" }

// Next applies multiplicative decrease for congestion and additive increase
// only after the configured increase interval.
func (a *AIMD) Next(currentBPS int, s sinks.Stats, p Params) int {
	a.updateParams(p)

	lossDelta := s.PktSndLossTotal - a.prevPktLoss
	retransDelta := s.PktRetransTotal - a.prevPktRetrans
	a.prevPktLoss = s.PktSndLossTotal
	a.prevPktRetrans = s.PktRetransTotal

	congestion := lossDelta > 0 || retransDelta > 0 ||
		s.PacketLossRate > lossRateThresh

	var next int64
	timestamp := s.TimestampMs
	switch {
	case congestion && timestamp >= a.nextDecr:
		next = int64(float64(currentBPS) * a.decrMult)
		a.nextDecr = timestamp + uint64(a.decrInterval)
		a.nextIncr = timestamp + uint64(a.incrInterval)
	case !congestion && timestamp >= a.nextIncr:
		next = int64(currentBPS) + int64(a.incrStep)
		a.nextIncr = timestamp + uint64(a.incrInterval)
	default:
		next = int64(currentBPS)
	}

	next = max(min(next, int64(a.maxBR)), int64(a.minBR))
	return int(next)
}

func (a *AIMD) updateParams(p Params) {
	if p.MinBitrate > 0 {
		a.minBR = p.MinBitrate
	}
	if p.MaxBitrate > 0 {
		a.maxBR = p.MaxBitrate
	}
	if a.minBR <= 0 {
		a.minBR = 500_000
	}
	if a.maxBR <= 0 {
		a.maxBR = 6_000_000
	}
	if p.AimdIncrStep > 0 {
		a.incrStep = p.AimdIncrStep
	} else if a.incrStep <= 0 {
		a.incrStep = defaultAimdIncrStep
	}
	if p.AimdDecrMult > 0 && p.AimdDecrMult < 1 {
		a.decrMult = p.AimdDecrMult
	} else if a.decrMult <= 0 || a.decrMult >= 1 {
		a.decrMult = defaultAimdDecrMult
	}
	a.incrInterval = norm(p.AimdIncrInterval, a.incrInterval)
	if a.incrInterval <= 0 {
		a.incrInterval = defaultAimdIncrInterval
	}
	a.decrInterval = norm(p.AimdDecrInterval, a.decrInterval)
	if a.decrInterval <= 0 {
		a.decrInterval = defaultAimdDecrInterval
	}
}
