package srtla

import (
	"log/slog"
	"math"
	"sync"
	"time"
)

// Both schedulers read an uplink through Uplink.schedState — one consistent
// snapshot under one lock per uplink per packet — and all per-uplink
// scheduling state (window, in-flight, NAK burst, stall count, weight) lives
// on the Uplink. A scheduler itself only remembers its previous pick.

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

	// enhancedNAKQuiet: for this long after a NAK the stall counter is left
	// alone — the path is at least reacting.
	enhancedNAKQuiet = 2 * time.Second
)

// NewScheduler builds the scheduler for the config's mode. Enhanced is the
// default for "enhanced" and any unknown value, mirroring
// Config.Normalized().
func NewScheduler(mode string, log *slog.Logger) Scheduler {
	if mode == ModeClassic {
		return &classicScheduler{}
	}
	return &enhancedScheduler{log: log}
}

// applyFeedback routes receiver feedback to the uplink's congestion state
// (the reference window math, plus the stall/NAK bookkeeping the enhanced
// score reads, both live on the Uplink).
func applyFeedback(u *Uplink, sn int32, kind FeedbackKind) {
	switch kind {
	case FeedbackSrtAck:
		u.RegisterSRTAck(sn)
	case FeedbackSrtlaAck:
		u.RegisterACK(sn)
	case FeedbackSrtNak:
		u.RegisterNAK(sn)
	}
}

type classicScheduler struct{}

// Select picks the usable uplink with the best window/(inFlight+1); the
// first one wins a tie.
func (s *classicScheduler) Select(uplinks []*Uplink, now time.Time) int {
	best, bestScore := -1, 0
	for i, u := range uplinks {
		st := u.schedState(now)
		if !st.usable {
			continue
		}
		if sc := st.window / (st.inFlight + 1); best < 0 || sc > bestScore {
			best, bestScore = i, sc
		}
	}
	return best
}

func (s *classicScheduler) Feedback(u *Uplink, sn int32, kind FeedbackKind, _ time.Time) {
	applyFeedback(u, sn, kind)
}

func (s *classicScheduler) MarkUnacked(*Uplink, time.Time) {}

type enhancedScheduler struct {
	mu sync.Mutex
	// hystIdx is the previous Select outcome: that uplink keeps the pick
	// while its score stays within 10% of the best.
	hystIdx int
	log     *slog.Logger
}

func (s *enhancedScheduler) Select(uplinks []*Uplink, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	best, bestScore := -1, math.Inf(-1)
	prev := 0.0 // this pass's score of the previous pick, if it is still usable
	for i, u := range uplinks {
		st := u.schedState(now)
		if !st.usable {
			continue
		}
		sc := enhancedScore(st)
		u.SetWeight(sc)
		if best < 0 || sc > bestScore {
			best, bestScore = i, sc
		}
		if i == s.hystIdx {
			prev = sc
		}
	}
	if best < 0 {
		return -1
	}
	// Hysteresis: keep the previous pick while it stays within 10% of the
	// current best, cutting flapping between near-equal uplinks.
	if prev > 0 && bestScore > 0 && prev >= bestScore*(1-enhancedHysteresis) && prev >= enhancedMinScore {
		return s.hystIdx
	}
	s.hystIdx = best
	return best
}

// enhancedScore is the enhanced metric: the classic window ratio, damped by
// the live RTT, recent packet loss, and the NAK burst, then
// stalling-penalised and smoothed against the uplink's previous score.
func enhancedScore(st schedState) float64 {
	base := float64(st.window) / (float64(st.inFlight) + 1)

	// RTT term: scale the classic ratio toward the minimum as RTT grows.
	rtt := min(float64(st.rttMs), enhancedRTTMax)
	rttFactor := 1 - 0.5*(rtt/enhancedRTTMax)

	// Loss term: NAKs in the current second map onto a loss fraction.
	lossFactor := 1.0
	if st.nakBurst > 0 {
		lossFactor = 1.0 / (1.0 + 0.5*float64(st.nakBurst))
	}
	// Burst penalty: >=5 NAKs in 1s drops the candidate sharply.
	if st.nakBurst >= enhancedBurstThreshold {
		lossFactor *= 0.5
	}

	// Stall penalty: the backlog is large and SRTLA ACKs have not drained
	// it; scales the score down up to enhancedStallMaxPenalty (50%).
	if st.stalls > 0 {
		frac := math.Min(float64(st.stalls)/float64(enhancedStallThreshold), 1)
		lossFactor *= 1 - enhancedStallMaxPenalty*frac
	}

	sc := base * rttFactor * lossFactor
	if sc < enhancedMinScore {
		sc = enhancedMinScore
		// Exponential decay of the weight so a repeatedly stalling path
		// stays deprioritised (spec 14.3.3).
		if st.weight > 0 && st.weight < sc {
			sc = math.Min(sc, st.weight+enhancedMinScore)
		}
	}
	// Hysteresis smoothing: exponential moving average with alpha 0.2.
	if st.weight > 0 {
		sc = (1-enhancedAlpha)*st.weight + enhancedAlpha*sc
	}
	return sc
}

func (s *enhancedScheduler) Feedback(u *Uplink, sn int32, kind FeedbackKind, _ time.Time) {
	applyFeedback(u, sn, kind)
}

// MarkUnacked counts a non-draining in-flight state; the penalty scales
// with the count up to enhancedStallThreshold consecutive marks.
func (s *enhancedScheduler) MarkUnacked(u *Uplink, now time.Time) {
	u.noteUnacked(now)
}
