package balancers

import (
	"context"
	"log/slog"
	"math"

	"ctrlirl-coder/internal/sinks"
)

// The constants below mirror src/core/bitrate_control.{h,c} of the original
// ctrlirl_coder exactly. Do not "clean them up" — the adaptive behaviour is the
// contract.
const (
	bitrateIncrMin     = 30 * 1000  // BITRATE_INCR_MIN
	bitrateDecrMin     = 100 * 1000 // BITRATE_DECR_MIN
	bitrateIncrScale   = 30         // BITRATE_INCR_SCALE
	bitrateDecrScale   = 10         // BITRATE_DECR_SCALE
	decrFastIntervalMS = 250        // BITRATE_DECR_FAST_INT

	emaSlow          = 0.99
	emaFast          = 0.01
	emaRTTDelta      = 0.8
	emaRTTDeltaNew   = 0.2
	emaThroughput    = 0.97
	emaThroughputNew = 0.03

	rttMinDrift     = 1.001
	rttIgnoreValue  = 100
	rttInitial      = 300
	rttMinInitial   = 200.0
	lossRateThresh  = 0.5
	emaLoss         = 0.9
	emaLossNew      = 0.1
	bsTh3Mult       = 4
	bsTh2JitterMult = 3.0
	bsTh1JitterMult = 2.5
	bsThMin         = 50
	rttJitterMult   = 4
	rttAvgPercent   = 15
	rttStableDelta  = 0.01
	rttMinJitter    = 1
)

// Adaptive is the default RTT+buffer+loss driven balancer. It is stateful and
// must be driven from a single goroutine (the controller's balancer ticker).
type Adaptive struct {
	// Link geometry / limits (set at init).
	minBR, maxBR, latency, pktSize int
	incrStep, decrStep             int
	incrInterval, decrInterval     int
	decrFastMS                     int

	curBitrate int

	// Buffer size tracking.
	bsAvg, bsJitter float64
	prevBS          int

	// primed marks that the first stats sample has been used to seed the
	// moving averages (see Next).
	primed bool

	// RTT tracking.
	rttAvg, rttMin, rttJitter, rttAvgDelta float64
	prevRTT                                int

	// Throughput tracking (bps).
	throughput float64

	// Packet loss tracking.
	prevPktLoss, prevPktRetrans int64
	lossRate                    float64

	// Rate-limit timers (ms).
	nextIncr, nextDecr uint64
}

// NewAdaptive constructs an adaptive balancer starting at the max bitrate
// (the original begins optimistic at max_bitrate).
func NewAdaptive(p Params) *Adaptive {
	a := &Adaptive{
		minBR:        p.MinBitrate,
		maxBR:        p.MaxBitrate,
		latency:      p.SRTLatencyMs,
		pktSize:      p.SRTPktSize,
		incrStep:     norm(p.AdaptiveIncrStep, bitrateIncrMin),
		decrStep:     norm(p.AdaptiveDecrStep, bitrateDecrMin),
		incrInterval: norm(p.AdaptiveIncrInterval, 500),
		decrInterval: norm(p.AdaptiveDecrInterval, 200),

		curBitrate: p.MaxBitrate,
		rttMin:     rttMinInitial,
		prevRTT:    rttInitial,
		decrFastMS: decrFastIntervalMS,
	}
	if a.pktSize <= 0 {
		a.pktSize = 1316 // avoid divide-by-zero in rttToBS
	}
	return a
}

// SetBounds updates min/max and resets to a fresh max-starting state (the
// original reinitialised the algorithm on config reload).
func (a *Adaptive) SetBounds(p Params) {
	a.minBR = p.MinBitrate
	a.maxBR = p.MaxBitrate
	a.latency = p.SRTLatencyMs
	if p.SRTPktSize > 0 {
		a.pktSize = p.SRTPktSize
	}
	a.incrStep = norm(p.AdaptiveIncrStep, a.incrStep)
	a.decrStep = norm(p.AdaptiveDecrStep, a.decrStep)
	a.incrInterval = norm(p.AdaptiveIncrInterval, a.incrInterval)
	a.decrInterval = norm(p.AdaptiveDecrInterval, a.decrInterval)
	a.curBitrate = p.MaxBitrate
}

func (a *Adaptive) Name() string { return "adaptive" }

// Next mirrors bitrate_update().
func (a *Adaptive) Next(currentBPS int, s sinks.Stats, p Params) int {
	a.updateParams(p)

	bs := s.SendBufferBytes
	rttInt := int(s.RTT)
	timestamp := s.TimestampMs

	// --- Packet loss tracking ---
	lossDelta := s.PktSndLossTotal - a.prevPktLoss
	retransDelta := s.PktRetransTotal - a.prevPktRetrans
	a.prevPktLoss = s.PktSndLossTotal
	a.prevPktRetrans = s.PktRetransTotal

	if lossDelta > 0 || retransDelta > 0 {
		a.lossRate = a.lossRate*emaLoss + float64(lossDelta+retransDelta)*emaLossNew
	} else {
		a.lossRate *= emaLoss
	}
	pktLossCongestion := a.lossRate > lossRateThresh

	// --- Send buffer + throughput stats ---
	// Seed the moving averages from the first sample (same pattern as rttAvg
	// below). Without seeding they warm up from zero over tens of ticks while
	// the link is already steady: the rttToBS() cap on bsTh2 tracks the zero
	// throughput estimate, clamps bsTh2 near zero, and a healthy steady
	// SNDDATA (~1 in-flight packet, ~200 B — always present, even on
	// localhost) reads as heavy congestion, ramping the bitrate to the floor.
	priming := !a.primed
	if priming {
		a.primed = true
		a.bsAvg = float64(bs)
		a.throughput = s.SendRateMbps * 1e6
	} else {
		a.bsAvg = a.bsAvg*emaSlow + float64(bs)*emaFast
		a.throughput *= emaThroughput
		a.throughput += (s.SendRateMbps * 1e6) * emaThroughputNew
	}
	deltaBS := bs - a.prevBS
	if !priming && float64(deltaBS) > a.bsJitter {
		a.bsJitter = float64(deltaBS)
	}
	a.prevBS = bs

	// --- RTT stats ---
	if a.rttAvg == 0.0 {
		a.rttAvg = s.RTT
	} else {
		a.rttAvg = a.rttAvg*emaSlow + emaFast*s.RTT
	}
	deltaRTT := s.RTT - float64(a.prevRTT)
	a.rttAvgDelta = a.rttAvgDelta*emaRTTDelta + deltaRTT*emaRTTDeltaNew
	a.prevRTT = rttInt

	a.rttMin *= rttMinDrift
	if rttInt != rttIgnoreValue && s.RTT < a.rttMin && a.rttAvgDelta < 1.0 {
		a.rttMin = s.RTT
	}

	a.rttJitter *= emaSlow
	if deltaRTT > a.rttJitter {
		a.rttJitter = deltaRTT
	}

	// --- Thresholds ---
	bsTh3 := int64((a.bsAvg + a.bsJitter) * bsTh3Mult)
	bsTh2 := int64(math.Max(bsThMin, a.bsAvg+math.Max(a.bsJitter*bsTh2JitterMult, a.bsAvg)))
	bsTh2 = min(bsTh2, int64(a.rttToBS(a.latency/2)))
	bsTh1 := int64(math.Max(bsThMin, a.bsAvg+a.bsJitter*bsTh1JitterMult))
	rttThMax := a.rttAvg + math.Max(a.rttJitter*float64(rttJitterMult), a.rttAvg*float64(rttAvgPercent)/100.0)
	rttThMin := a.rttMin + math.Max(float64(rttMinJitter), a.rttJitter*2)

	// --- Decision ---
	bitrate := int64(a.curBitrate)
	branch := "hold"
	switch {
	case bitrate > int64(a.minBR) &&
		(rttInt >= a.latency/3 || int64(bs) > bsTh3):
		// Emergency: drop to minimum.
		bitrate = int64(a.minBR)
		a.nextDecr = timestamp + uint64(a.decrInterval)
		branch = "emergency"

	case timestamp > a.nextDecr &&
		(rttInt > a.latency/5 || int64(bs) > bsTh2 || pktLossCongestion):
		// Heavy congestion: fast decrease.
		bitrate -= int64(a.decrStep) + bitrate/bitrateDecrScale
		a.nextDecr = timestamp + uint64(a.decrFastMS)
		branch = "heavy"

	case timestamp > a.nextDecr &&
		(int64(rttInt) > int64(rttThMax) || int64(bs) > bsTh1):
		// Light congestion: slow decrease.
		bitrate -= int64(a.decrStep)
		a.nextDecr = timestamp + uint64(a.decrInterval)
		branch = "light"

	case timestamp > a.nextIncr &&
		int64(rttInt) < int64(rttThMin) && a.rttAvgDelta < rttStableDelta && !pktLossCongestion:
		// Stable: increase.
		bitrate += int64(a.incrStep) + bitrate/bitrateIncrScale
		a.nextIncr = timestamp + uint64(a.incrInterval)
		branch = "incr"
	}
	// Guarded: this build runs with debug logging off, and the 11
	// attributes are formatted eagerly (Next is on the 500 ms tick).
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		slog.Debug("adaptive", "branch", branch, "bs", bs, "bsTh1", bsTh1, "bsTh2", bsTh2, "bsTh3", bsTh3, "rtt", rttInt, "rttThMin", rttThMin, "rttThMax", rttThMax, "thr_mbps", a.throughput/1e6, "bsAvg", a.bsAvg, "bsJit", a.bsJitter)
	}

	bitrate = max(min(bitrate, int64(a.maxBR)), int64(a.minBR))
	a.curBitrate = int(bitrate)

	// Round to 100 kbps.
	return a.curBitrate / 100000 * 100000
}

// rttToBS estimates the in-flight BYTES on the wire for an rtt (ms) window,
// from the smoothed send throughput (bps): bps/8 bytes/s * rttMS/1000 s. The
// result is compared directly against the send buffer in bytes (the bsTh2
// cap), so it must be in bytes — not packets. (The old form divided by the
// packet size and kept the rate in kbit-era units, which made the cap ~1000x
// too small: a steady ~200 B SNDDATA then always read as heavy congestion and
// the target collapsed to the minimum, even on a healthy localhost link.)
func (a *Adaptive) rttToBS(rttMS int) float64 {
	return (a.throughput / 8.0) * float64(rttMS) / 1000.0
}

func (a *Adaptive) updateParams(p Params) {
	// Keep live limits in sync with config (cheap; lets a reload propagate).
	if p.MinBitrate > 0 {
		a.minBR = p.MinBitrate
	}
	if p.MaxBitrate > 0 {
		a.maxBR = p.MaxBitrate
	}
}
