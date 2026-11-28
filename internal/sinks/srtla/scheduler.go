package srtla

import (
	"log/slog"
	"math"
	"sync"
	"time"
)

// Scheduler ranks the uplinks and returns the index of the one a given
// datagram should be sent on. All schedulers must return -1 when no
// uplink is selectable (caller then drops the packet, per spec 14.3.2).
// Classic mirrors the reference sender's window/(inFlight+1) metric;
// enhanced applies the adaptive scoring of spec 14.3.3 on top.
type Scheduler interface {
	// Select returns the index of the best uplink for a new packet, or -1
	// when none is usable. uplinks must be the full, stable set.
	Select(uplinks []*Uplink, now time.Time) int

	// Feedback is invoked for receiver traffic destined for the given
	// uplink: SRTLA ACKs and SRT cumulative ACKs clear in-flight counts
	// and grow windows; SRT NAKs shrink them (and count toward the
	// enhanced burst penalty).
	Feedback(uplink *Uplink, sn int32, kind FeedbackKind, now time.Time)

	// MarkUnacked increments the enhanced stall counter when SRTLA ACKs
	// (the high-frequency signal) show the in-flight backlog is not
	// draining.
	MarkUnacked(uplink *Uplink, now time.Time)
}

// FeedbackKind classifies receiver feedback for the scheduler.
type FeedbackKind int

const (
	// FeedbackSrtAck is a cumulative SRT ACK (offset 16 of SRT_ACK).
	FeedbackSrtAck FeedbackKind = iota
	// FeedbackSrtlaAck is a SRTLA per-packet ACK (0x9100).
	FeedbackSrtlaAck
	// FeedbackSrtNak is one lost sequence from an SRT NAK (0x8003).
	FeedbackSrtNak
)

// Enhanced mode tuning (spec 14.3.3).
const (
	enhancedBurstThreshold  = 5
	enhancedAlpha           = 0.2
	enhancedMinScore        = 0.01
	enhancedHysteresis      = 0.10
	enhancedStallThreshold  = 1000
	enhancedStallMaxPenalty = 0.5

	// enhancedRTTMax is the RTT scale past which the RTT term contributes
	// its full penalty weight.
	enhancedRTTMax = 100.0
)

// NewScheduler builds the scheduler for the config's mode.
func NewScheduler(mode string, log *slog.Logger) Scheduler {
	if mode != ModeClassic {
		// Enhanced is the default for "enhanced" and any unknown value,
		// mirroring Config.Normalized().
		return &enhancedScheduler{
			log:      log,
			lastNak:  make(map[*Uplink]time.Time),
			stallCnt: make(map[*Uplink]int),
		}
	}
	return &classicScheduler{}
}

type classicScheduler struct{}

func (s *classicScheduler) Select(uplinks []*Uplink, now time.Time) int {
	best := -1
	bestScore := 0
	for i, u := range uplinks {
		if !u.Established() || u.TimedOut() {
			continue
		}
		sc := u.Score()
		if best < 0 || sc > bestScore {
			best, bestScore = i, sc
		}
	}
	return best
}

func (s *classicScheduler) Feedback(uplink *Uplink, sn int32, kind FeedbackKind, now time.Time) {
	switch kind {
	case FeedbackSrtAck:
		uplink.RegisterSRTAck(sn)
	case FeedbackSrtlaAck:
		uplink.RegisterACK(sn)
	case FeedbackSrtNak:
		uplink.RegisterNAK(sn)
	}
}

func (s *classicScheduler) MarkUnacked(*Uplink, time.Time) {}

type enhancedScheduler struct {
	// hyst holds the previous Select outcome so a trailing uplink within
	// 10% sticks; the window map is per-uplink by pointer identity.
	mu       sync.Mutex
	hystIdx  int
	log      *slog.Logger
	lastNak  map[*Uplink]time.Time
	stallCnt map[*Uplink]int
}

func (s *enhancedScheduler) Select(uplinks []*Uplink, now time.Time) int {
	if len(uplinks) == 0 {
		return -1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(uplinks, now)

	best, bestScore := -1, math.Inf(-1)
	for i, u := range uplinks {
		if !u.Established() || u.TimedOut() {
			continue
		}
		sc := s.score(u)
		u.SetWeight(sc)
		if best < 0 || sc > bestScore {
			best, bestScore = i, sc
		}
	}
	if best < 0 {
		return -1
	}
	// Hysteresis: keep the previous pick while it stays within 10% of the
	// current best, cutting flapping between near-equal uplinks.
	if s.hystIdx >= 0 && s.hystIdx < len(uplinks) &&
		uplinks[s.hystIdx].Established() && !uplinks[s.hystIdx].TimedOut() {
		prev := uplinks[s.hystIdx].Weight()
		if prev > 0 && bestScore > 0 && prev >= bestScore*(1-enhancedHysteresis) && prev >= enhancedMinScore {
			return s.hystIdx
		}
	}
	s.hystIdx = best
	return best
}

// score implements the enhanced metric: the classic window ratio, damped by
// the live RTT, recent packet loss, and the rolling NAK burst, then
// stalling-penalised.
func (s *enhancedScheduler) score(u *Uplink) float64 {
	if u.TimedOut() {
		return 0
	}
	base := float64(u.Window()) / (float64(u.InFlight()) + 1)

	// RTT term: scale the classic ratio toward the minimum as RTT grows.
	rtt := float64(u.RTTMs())
	if rtt > enhancedRTTMax {
		rtt = enhancedRTTMax
	}
	rttFactor := 1 - 0.5*(rtt/enhancedRTTMax)

	// Loss term: NAKs in the last second map onto a loss fraction.
	lossFactor := 1.0
	if b := u.NAKBurst(); b > 0 {
		lossFactor = 1.0 / (1.0 + 0.5*float64(b))
	}
	// Burst penalty: >=5 NAKs in 1s drops the candidate sharply.
	if b := u.NAKBurst(); b >= enhancedBurstThreshold {
		lossFactor *= 0.5
	}

	// Stall penalty: the backlog is large and SRTLA ACKs have not drained
	// it; scales the score down up to enhancedStallMaxPenalty (50%).
	if stall := s.stallCnt[u]; stall > 0 {
		frac := math.Min(float64(stall)/float64(enhancedStallThreshold), 1)
		lossFactor *= 1 - enhancedStallMaxPenalty*frac
	}

	sc := base * rttFactor * lossFactor
	if sc < enhancedMinScore {
		sc = enhancedMinScore
		// Exponential decay of the weight so a repeatedly stalling path
		// stays deprioritised (spec 14.3.3).
		if prev := u.Weight(); prev > 0 && prev < sc {
			sc = math.Min(sc, prev+enhancedMinScore)
		}
	}
	// Hysteresis smoothing: exponential moving average with alpha 0.2.
	prev := u.Weight()
	if prev > 0 {
		sc = (1-enhancedAlpha)*prev + enhancedAlpha*sc
	}
	return sc
}

// Feedback applies the reference window math through the uplink and tracks
// the enhanced stall/burst state.
func (s *enhancedScheduler) Feedback(uplink *Uplink, sn int32, kind FeedbackKind, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case FeedbackSrtAck:
		uplink.RegisterSRTAck(sn)
	case FeedbackSrtlaAck:
		uplink.RegisterACK(sn)
	case FeedbackSrtNak:
		uplink.RegisterNAK(sn)
		s.lastNak[uplink] = now
	}
	if kind == FeedbackSrtAck || kind == FeedbackSrtlaAck {
		delete(s.stallCnt, uplink)
	}
}

// MarkUnacked counts a non-draining in-flight state; the penalty scales
// with the count up to enhancedStallThreshold consecutive marks.
func (s *enhancedScheduler) MarkUnacked(uplink *Uplink, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A fresh NAK restarts the counting window: the path is at least
	// reacting, so the stall counter resets.
	if last, ok := s.lastNak[uplink]; ok && now.Sub(last) < 2*time.Second {
		return
	}
	if uplink.InFlight() >= enhancedStallThreshold {
		s.stallCnt[uplink]++
	} else {
		delete(s.stallCnt, uplink)
	}
}

// prune drops tracked state for uplinks that no longer exist (e.g. after a
// SIGHUP IP-list reload re-created the set).
func (s *enhancedScheduler) prune(uplinks []*Uplink, now time.Time) {
	if s.lastNak == nil {
		s.lastNak = make(map[*Uplink]time.Time)
	}
	if s.stallCnt == nil {
		s.stallCnt = make(map[*Uplink]int)
	}
	alive := make(map[*Uplink]struct{}, len(uplinks))
	for _, u := range uplinks {
		alive[u] = struct{}{}
	}
	for u := range s.lastNak {
		if _, ok := alive[u]; !ok {
			delete(s.lastNak, u)
		}
	}
	for u := range s.stallCnt {
		if _, ok := alive[u]; !ok {
			delete(s.stallCnt, u)
		}
	}
}
