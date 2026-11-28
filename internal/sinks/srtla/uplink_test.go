package srtla

import (
	"testing"
	"time"
)

func testCfg() *Config {
	cfg := Config{
		ReceiverHost:  "127.0.0.1",
		ReceiverPort:  1,
		UplinkIPs:     []string{"127.0.0.1"},
		Mode:          ModeEnhanced,
		ConnTimeoutMs: 4000,
	}
	cfg = cfg.Normalized()
	return &cfg
}

// fakeClock is a controllable clock for deterministic time tests.
type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time      { return f.t }
func (f *fakeClock) add(d time.Duration) { f.t = f.t.Add(d) }

func TestRingWraparound(t *testing.T) {
	// A full lap around the ring must land back on the next index.
	idx := 0
	for i := 0; i < pktRingSz; i++ {
		idx = RingIndex(idx, 1)
	}
	if idx != 0 {
		t.Fatalf("full lap: got %d, want 0", idx)
	}
	// Negative wrap: one before 0 is the last slot.
	if got := RingIndex(0, -1); got != pktRingSz-1 {
		t.Fatalf("RingIndex(0,-1) = %d, want %d", got, pktRingSz-1)
	}
}

func TestNAKShrinksWindow(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	u := NewUplink(1, "127.0.0.1", testCfg(), fc.Now)
	// Prime the ring with a data packet.
	u.NoteSent(100)
	if u.InFlight() != 1 {
		t.Fatalf("InFlight = %d, want 1", u.InFlight())
	}
	w0 := u.Window()
	if !u.RegisterNAK(100) {
		t.Fatalf("RegisterNAK(100) = false, want true")
	}
	if got := u.Window(); got != w0-WindowDecr {
		t.Fatalf("Window after NAK = %d, want %d", got, w0-WindowDecr)
	}
	// NAK for a sequence not in the log is a no-op.
	if u.RegisterNAK(99999) {
		t.Fatalf("RegisterNAK(99999) = true, want false")
	}
}

func TestNAKWindowFloor(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	u := NewUplink(1, "127.0.0.1", testCfg(), fc.Now)
	u.NoteSent(1)
	for i := 0; i < 40; i++ {
		u.RegisterNAK(1 + int32(i)) // first hits, rest miss
	}
	if got := u.Window(); got < WindowMin*WindowMult {
		t.Fatalf("Window floored to %d, want >= %d", got, WindowMin*WindowMult)
	}
}

func TestSRTAACKDrainsInFlight(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	u := NewUplink(1, "127.0.0.1", testCfg(), fc.Now)
	u.NoteSent(10)
	u.NoteSent(11)
	u.NoteSent(12)
	if u.InFlight() != 3 {
		t.Fatalf("InFlight = %d, want 3", u.InFlight())
	}
	// A SRTLA ACK for the most recent packet clears exactly one.
	u.RegisterACK(12)
	if got := u.InFlight(); got != 2 {
		t.Fatalf("InFlight after ACK(12) = %d, want 2", got)
	}
	// Growing window: a single packet receipt bumps the window by one.
	w := u.Window()
	u.RegisterACK(11)
	if got := u.Window(); got <= w {
		t.Fatalf("Window not grown by ACK: %d -> %d", w, got)
	}
	// ACK for an unknown packet still bumps window by one (reference:
	// the +1 applies to every conn with recent traffic).
	w = u.Window()
	u.RegisterACK(42)
	if got := u.Window(); got != w+1 {
		t.Fatalf("Window after unknown ACK = %d, want %d", got, w+1)
	}
}

func TestSRTAckClearsAllBelow(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	u := NewUplink(1, "127.0.0.1", testCfg(), fc.Now)
	u.NoteSent(1)
	u.NoteSent(2)
	u.NoteSent(3)
	u.NoteSent(4)
	// A cumulative SRT ACK at 3 clears everything with sn < 3.
	u.RegisterSRTAck(3)
	if got := u.InFlight(); got != 2 {
		t.Fatalf("InFlight after SrtAck(3) = %d, want 2 (sn 3,4 remain)", got)
	}
}

func TestTimeout(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	cfg := testCfg()
	u := NewUplink(1, "127.0.0.1", cfg, fc.Now)
	// No traffic at all: timed out.
	if !u.TimedOut() {
		t.Fatalf("fresh uplink should be timed out")
	}
	// A recent receive clears the timeout.
	u.TouchRecv()
	if u.TimedOut() {
		t.Fatalf("uplink with recent recv should not be timed out")
	}
	// Past the connection timeout it times out again.
	fc.add(time.Duration(cfg.ConnTimeoutMs)*time.Millisecond + time.Millisecond)
	u.lastSentNow()
	if !u.TimedOut() {
		t.Fatalf("outgoing traffic must not keep an unresponsive uplink alive")
	}
}

func TestMarkDeadForcesTimeout(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	u := NewUplink(1, "127.0.0.1", testCfg(), fc.Now)
	u.TouchRecv()
	u.MarkDead()
	if !u.TimedOut() {
		t.Fatalf("MarkDead should force timeout")
	}
	u.Reset()
	// Reset also clears the receive timestamp, so the uplink reads as timed
	// out until traffic returns; the forced-dead flag itself must be gone.
	u.TouchRecv()
	if u.TimedOut() {
		t.Fatalf("Reset should clear the forced timeout")
	}
}

func TestWindowGrowthCap(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	u := NewUplink(1, "127.0.0.1", testCfg(), fc.Now)
	u.TouchRecv()
	for i := 0; i < 10000; i++ {
		u.RegisterACK(int32(i))
	}
	if got := u.Window(); got > WindowMax*WindowMult {
		t.Fatalf("Window exceeded cap: %d > %d", got, WindowMax*WindowMult)
	}
}

// nakBurst NAKs n fresh packets on u.
func nakBurst(u *Uplink, first, n int32) {
	for sn := first; sn < first+n; sn++ {
		u.NoteSent(sn)
		u.RegisterNAK(sn)
	}
}

// A NAK burst counts for the second it happened in and then expires. The
// old four-slot ring re-read its stale slot every fourth second, so one loss
// episode kept penalising the uplink for a second in every four, forever.
func TestNAKBurstExpires(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	u := NewUplink(1, "127.0.0.1", testCfg(), fc.Now)
	nakBurst(u, 1, 10)
	if got := u.NAKBurst(); got != 10 {
		t.Fatalf("NAKBurst in the burst's second = %d, want 10", got)
	}
	for sec := 1; sec <= 12; sec++ {
		fc.add(time.Second)
		if got := u.NAKBurst(); got != 0 {
			t.Fatalf("NAKBurst %d s after the burst = %d, want 0", sec, got)
		}
	}
	// A new burst starts a fresh count.
	nakBurst(u, 100, 3)
	if got := u.NAKBurst(); got != 3 {
		t.Fatalf("NAKBurst of a later burst = %d, want 3", got)
	}
}

// The burst window is keyed on wall-clock seconds. A clock that steps back
// (NTP correcting a freshly booted encoder) used to index the ring with a
// negative offset and panic — on the per-packet scheduling path.
func TestNAKBurstSurvivesClockStepBack(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	u := NewUplink(1, "127.0.0.1", testCfg(), fc.Now)
	nakBurst(u, 1, 6)
	fc.add(-3 * time.Second)
	if got := u.NAKBurst(); got != 0 {
		t.Fatalf("NAKBurst after the clock stepped back = %d, want 0", got)
	}
	nakBurst(u, 100, 2) // must not panic either
	if got := u.NAKBurst(); got != 2 {
		t.Fatalf("NAKBurst of a burst after the step = %d, want 2", got)
	}
}
