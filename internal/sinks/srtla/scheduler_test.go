package srtla

import (
	"log/slog"
	"testing"
	"time"
)

func mkUplinks(fc *fakeClock, n int, cfg *Config) []*Uplink {
	norm := cfg.Normalized()
	cfg = &norm
	us := make([]*Uplink, n)
	for i := range us {
		us[i] = NewUplink(i+1, "127.0.0.1", cfg, fc.Now)
		us[i].TouchRecv()
		us[i].RegDone()
	}
	return us
}

func TestClassicSelectPrefersHigherScore(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	us := mkUplinks(fc, 3, testCfg())
	// Load uplinks 0 and 1 with in-flight so their scores drop; uplink 2
	// stays clean, so it must win.
	for i := 0; i < 10; i++ {
		us[0].NoteSent(int32(i))
		us[1].NoteSent(int32(i + 100))
	}
	s := NewScheduler(ModeClassic, slog.Default())
	got := s.Select(us, fc.Now())
	if got != 2 {
		t.Fatalf("Select = %d, want 2 (only uplink without backlog)", got)
	}
}

func TestClassicSelectDropsTimedOut(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	us := mkUplinks(fc, 2, testCfg())
	// Age uplink 0 past its timeout; uplink 1 stays fresh.
	us[0].TouchRecv()
	fc.add(5 * time.Second)
	us[1].TouchRecv()
	s := NewScheduler(ModeClassic, slog.Default())
	if got := s.Select(us, fc.Now()); got != 1 {
		t.Fatalf("Select = %d, want 1 (uplink 0 timed out)", got)
	}
}

func TestClassicSelectNoneUsable(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	us := mkUplinks(fc, 2, testCfg())
	fc.add(30 * time.Second) // everything times out
	s := NewScheduler(ModeClassic, slog.Default())
	if got := s.Select(us, fc.Now()); got != -1 {
		t.Fatalf("Select = %d, want -1 (all timed out)", got)
	}
}

func TestSchedulersSkipUnregisteredUplinks(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	for _, mode := range []string{ModeClassic, ModeEnhanced} {
		us := mkUplinks(fc, 2, testCfg())
		us[0].Reset()
		us[0].TouchRecv()
		if got := NewScheduler(mode, slog.Default()).Select(us, fc.Now()); got != 1 {
			t.Errorf("%s selected uplink %d, want registered uplink 1", mode, got)
		}
	}
}

func TestEnhancedBurstPenalty(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	us := mkUplinks(fc, 2, testCfg())
	s := NewScheduler(ModeEnhanced, slog.Default())
	// Feed the first uplink a NAK burst; it should lose to the clean one.
	for i := 0; i < 10; i++ {
		us[0].NoteSent(int32(100 + i))
	}
	base := fc.Now()
	for i := 0; i < enhancedBurstThreshold+2; i++ {
		s.Feedback(us[0], int32(100+i), FeedbackSrtNak, base)
	}
	if got := s.Select(us, base); got != 1 {
		t.Fatalf("Select = %d, want 1 (uplink 0 hit a NAK burst)", got)
	}
}

func TestEnhancedFallsBackToClassicScore(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	us := mkUplinks(fc, 3, testCfg())
	// No telemetry, no NAKs: enhanced should agree with classic and pick
	// the uplink with the cleanest backlog.
	for i := 0; i < 10; i++ {
		us[0].NoteSent(int32(i))
		us[1].NoteSent(int32(i + 100))
	}
	s := NewScheduler(ModeEnhanced, slog.Default())
	if got := s.Select(us, fc.Now()); got != 2 {
		t.Fatalf("Select = %d, want 2 (clean uplink)", got)
	}
}

func TestSchedulerModesMatchConfig(t *testing.T) {
	if _, ok := NewScheduler(ModeClassic, nil).(*classicScheduler); !ok {
		t.Fatalf("classic mode did not build a classicScheduler")
	}
	if _, ok := NewScheduler(ModeEnhanced, nil).(*enhancedScheduler); !ok {
		t.Fatalf("enhanced mode did not build an enhancedScheduler")
	}
	// Unknown mode defaults to enhanced, as Normalized() guarantees.
	if _, ok := NewScheduler("bogus", nil).(*enhancedScheduler); !ok {
		t.Fatalf("unknown mode should build an enhancedScheduler")
	}
}
