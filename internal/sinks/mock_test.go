package sinks

import (
	"errors"
	"testing"
	"time"
)

func TestMockStatsSourceCumulativeCounters(t *testing.T) {
	m := NewMockStatsSourceWith(MockProfile{
		RTT:                25,
		SendRateMbps:       4,
		BandwidthMbps:      10,
		PktLossPerInterval: 2,
	})

	first, err := m.Stats()
	if err != nil {
		t.Fatalf("Stats(): %v", err)
	}
	if !first.IsValid() {
		t.Fatalf("expected valid sample (RTT %v), got IsValid=false", first.RTT)
	}
	if first.PktSndLossTotal != 2 {
		t.Fatalf("first loss total = %d, want 2", first.PktSndLossTotal)
	}

	second, err := m.Stats()
	if err != nil {
		t.Fatalf("Stats() 2: %v", err)
	}
	if second.PktSndLossTotal != 4 {
		t.Fatalf("loss counters must be cumulative: got %d, want 4", second.PktSndLossTotal)
	}
	if second.PacketsSentTotal <= first.PacketsSentTotal {
		t.Fatalf("packets-sent should advance monotonically: %d -> %d",
			first.PacketsSentTotal, second.PacketsSentTotal)
	}
}

func TestMockStatsSourceInvalidWhenNoRTT(t *testing.T) {
	m := NewMockStatsSourceWith(MockProfile{}) // RTT 0
	s, err := m.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if s.IsValid() {
		t.Fatal("sample with RTT=0 must be invalid (cold link guard)")
	}
}

func TestMockStatsSourceClosed(t *testing.T) {
	m := NewMockStatsSource()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := m.Stats()
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func TestEncoderUnits(t *testing.T) {
	if got := BitratePropertyValue("venc_bps", 4_000_000); got != 4_000_000 {
		t.Fatalf("venc_bps: got %d, want 4000000", got)
	}
	if got := BitratePropertyValue("venc_kbps", 4_000_000); got != 4000 {
		t.Fatalf("venc_kbps: got %d, want 4000", got)
	}
}

func TestMockInterval(t *testing.T) {
	m := NewMockStatsSource()
	m.SetInterval(10 * time.Millisecond)
	m.mu.Lock()
	got := m.interval
	m.mu.Unlock()
	if got != 10*time.Millisecond {
		t.Fatalf("interval = %v, want 10ms", got)
	}
}
