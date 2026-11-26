package srtla

import (
	"testing"
	"time"
)

func TestSummaryFields(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	cfg := testCfg()
	cfg.UplinkIPs = []string{"127.0.0.1", "127.0.0.1"}
	s, err := New(cfg, nil, fc.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Seed counters.
	s.sentPkts.Store(100)
	s.sentBytes.Store(100 * SRTMaxPayload)
	s.nakTotal.Store(10)
	s.ackTotal.Store(90)
	s.uplinks[0].SetRTT(40)
	s.uplinks[1].SetRTT(60)

	got := s.summary()
	if got.RTT <= 0 {
		t.Fatalf("RTT = %v, want > 0", got.RTT)
	}
	if got.RTT < 40 || got.RTT > 60 {
		t.Fatalf("RTT = %v, want between the uplink RTTs", got.RTT)
	}
	if got.PacketLossRate < 9.9 || got.PacketLossRate > 10.1 {
		t.Fatalf("PacketLossRate = %v, want ~10%%", got.PacketLossRate)
	}
	if got.PktSndLossTotal != 10 {
		t.Fatalf("PktSndLossTotal = %d, want 10", got.PktSndLossTotal)
	}
	if got.PacketsSentTotal != 100 {
		t.Fatalf("PacketsSentTotal = %d, want 100", got.PacketsSentTotal)
	}
	if got.ACKTotal != 90 {
		t.Fatalf("ACKTotal = %d, want 90", got.ACKTotal)
	}
	// No in-flight, so the send buffer estimate is zero.
	if got.SendBufferBytes != 0 {
		t.Fatalf("SendBufferBytes = %d, want 0", got.SendBufferBytes)
	}
}

func TestSummarySendBufferReflectsInFlight(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	s, _ := New(testCfg(), nil, fc.Now)
	s.uplinks[0].NoteSent(1)
	s.uplinks[0].NoteSent(2)
	s.uplinks[0].NoteSent(3)
	got := s.summary()
	want := 3 * SRTMaxPayload
	if got.SendBufferBytes != want {
		t.Fatalf("SendBufferBytes = %d, want %d", got.SendBufferBytes, want)
	}
}

func TestSummaryLossZeroWhenNoFeedback(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	s, _ := New(testCfg(), nil, fc.Now)
	if got := s.summary().PacketLossRate; got != 0 {
		t.Fatalf("PacketLossRate with no feedback = %v, want 0", got)
	}
}

func TestDetailedSnapshot(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	cfg := testCfg()
	cfg.UplinkIPs = []string{"127.0.0.1", "127.0.0.2"}
	cfg.Mode = ModeClassic
	s, err := New(cfg, nil, fc.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.uplinks[0].SetRTT(40)
	s.uplinks[0].RegDone()
	s.uplinks[0].NoteSent(1)
	s.uplinks[0].NoteSent(2)

	d := s.Detailed()
	if d.Mode != ModeClassic {
		t.Fatalf("Mode = %q, want %q", d.Mode, ModeClassic)
	}
	if len(d.Uplinks) != 2 {
		t.Fatalf("len(Uplinks) = %d, want 2", len(d.Uplinks))
	}
	u0 := d.Uplinks[0]
	if u0.ID != 1 || u0.IP != "127.0.0.1" {
		t.Fatalf("uplink[0] = %+v, want ID 1 / 127.0.0.1", u0)
	}
	if u0.RTTMs != 40 {
		t.Fatalf("uplink[0].RTTMs = %v, want 40", u0.RTTMs)
	}
	if !u0.Established {
		t.Fatalf("uplink[0].Established = false, want true (RegDone set)")
	}
	if u0.InFlight != 2 {
		t.Fatalf("uplink[0].InFlight = %d, want 2", u0.InFlight)
	}
	if u0.LastActivityMs != 0 {
		t.Fatalf("uplink[0].LastActivityMs = %v, want 0 (clock at last activity)", u0.LastActivityMs)
	}
	if d.ActiveConns != 0 || d.HasConnected || d.Failover {
		t.Fatalf("sender flags = %+v, want all zero/unconnected", d)
	}
}
