package gstpull

import (
	"testing"
	"time"
)

// testRateBps is the pace target: 8 Mbps. At this rate 1000 bytes == 1 ms of
// wall clock (1000 * 8 / 8,000,000 s = 1 ms), which keeps the test math
// integer-exact.
const testRateBps int64 = 8_000_000

// TestPacerVODDrain pins the 1× clamp: a VOD source delivers 60 ms of content
// essentially instantaneously (bytes available as fast as the drain reads
// them); the pacer must hold the drain to ~60 ms of wall time, not ~0.
func TestPacerVODDrain(t *testing.T) {
	var pc pacer
	now := int64(0)
	var slept time.Duration
	for i := 0; i < 60; i++ {
		d := pc.tick(now, 1000, testRateBps)
		slept += d
		now += int64(d / time.Millisecond) // the sleep passes wall time
	}
	if now < 55 || now > 65 {
		t.Fatalf("drain finished at wall %d ms, want ~59 ms (60 ms of content at 1×)", now)
	}
	if slept < 55*time.Millisecond || slept > 65*time.Millisecond {
		t.Fatalf("VOD drain sleep = %v, want ~59 ms (instant source held to 1×)", slept)
	}
}

// TestPacerLiveSourceNoPace pins the no-op path: a self-pacing live source
// delivers each 1000-byte chunk exactly when wall clock reaches its due
// time — drift stays 0, so the drain must never sleep.
func TestPacerLiveSourceNoPace(t *testing.T) {
	var pc pacer
	var slept time.Duration
	for i := 0; i < 60; i++ {
		now := int64(i + 1) // wall clock == content time (1 ms/chunk)
		slept += pc.tick(now, 1000, testRateBps)
	}
	if slept != 0 {
		t.Fatalf("live source sleep = %v, want 0", slept)
	}
}

// TestPacerPositiveDrift: delivering bytes faster than the target rate
// accumulates positive drift and sleeps the shortfall.
func TestPacerPositiveDrift(t *testing.T) {
	var pc pacer
	if d := pc.tick(0, 1000, testRateBps); d != 0 { // anchor
		t.Fatalf("anchor tick slept %v", d)
	}
	if d := pc.tick(0, 5000, testRateBps); d != 5*time.Millisecond { // due 5, drift +5
		t.Fatalf("drift tick slept %v, want 5 ms", d)
	}
}

// TestPacerStallReanchor: a long drain gap (send stall, >>500 ms behind wall)
// re-anchors the pacer instead of demanding a huge catch-up sleep.
func TestPacerStallReanchor(t *testing.T) {
	var pc pacer
	if d := pc.tick(0, 1000, testRateBps); d != 0 { // anchor
		t.Fatalf("anchor tick slept %v", d)
	}
	if d := pc.tick(2000, 1000, testRateBps); d != 0 { // 2 s behind → re-anchor, no catch-up
		t.Fatalf("stall tick slept %v, want 0 (re-anchor)", d)
	}
	if d := pc.tick(2001, 1000, testRateBps); d != 0 { // fresh anchor, exactly on time
		t.Fatalf("post-reanchor tick slept %v, want 0", d)
	}
}

// TestPacerDisabled: a non-positive rate (no target yet / live source before
// the controller) and an empty buffer never sleep.
func TestPacerDisabled(t *testing.T) {
	var pc pacer
	if d := pc.tick(0, 1000, 0); d != 0 {
		t.Fatalf("zero-rate tick slept %v", d)
	}
	if d := pc.tick(0, 0, testRateBps); d != 0 {
		t.Fatalf("empty-buffer tick slept %v", d)
	}
}
