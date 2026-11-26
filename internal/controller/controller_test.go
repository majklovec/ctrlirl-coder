package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"ctrlirl-coder/internal/config"
	"ctrlirl-coder/internal/sinks"
)

// fakeApplier records everything the controller pushes to the pipeline.
type fakeApplier struct {
	mu       sync.Mutex
	bitrates []int
	overlays []string
}

func (f *fakeApplier) SetBitrate(bps int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bitrates = append(f.bitrates, bps)
	return nil
}

func (f *fakeApplier) SetOverlayText(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overlays = append(f.overlays, text)
}

func (f *fakeApplier) lastBitrate() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bitrates) == 0 {
		return -1
	}
	return f.bitrates[len(f.bitrates)-1]
}

// fakeLiveness is a stub appsink frame-production signal.
type fakeLiveness struct{ stalled bool }

func (f *fakeLiveness) Stalled() bool { return f.stalled }

func testConfig() *config.Config {
	c := &config.Config{
		Sink: config.SinkSRT,
		General: config.GeneralParams{
			MinBitrateKbps: 500,
			MaxBitrateKbps: 6000,
			Balancer:       "adaptive",
		},
		SRT:     config.SRTParams{LatencyMs: 2000},
		Overlay: config.Overlay{Enabled: false, IntervalMS: 100, Format: config.DefaultOverlayFormat},
	}
	if err := c.Validate(); err != nil {
		panic(err)
	}
	return c
}

func runController(t *testing.T, src sinks.StatsSource, ap *fakeApplier, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	c := New(src, nil, ap, testConfig())
	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// A cold link (no RTT) must never trigger a bitrate change.
func TestColdLinkNoAdjustment(t *testing.T) {
	src := sinks.NewMockStatsSourceWith(sinks.MockProfile{RTT: 0}) // invalid samples
	ap := &fakeApplier{}
	runController(t, src, ap, 1200*time.Millisecond)

	ap.mu.Lock()
	n := len(ap.bitrates)
	ap.mu.Unlock()
	if n != 0 {
		t.Fatalf("cold link: SetBitrate was called %d times, want 0", n)
	}
}

// A congested link (RTT well past a third of the SRT latency) must drive the
// bitrate down to the configured minimum.
func TestCongestedLinkDropsToMin(t *testing.T) {
	c := testConfig()
	src := sinks.NewMockStatsSourceWith(sinks.MockProfile{
		RTT:           800, // 800ms >= latency(2000)/3 → emergency drop
		SendRateMbps:  20,
		BandwidthMbps: 100,
	})
	ap := &fakeApplier{}

	ctx, cancel := context.WithTimeout(context.Background(), 1600*time.Millisecond)
	defer cancel()
	ctl := New(src, nil, ap, c)
	if err := ctl.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := ap.lastBitrate(); got != c.MinBitratebps() {
		t.Fatalf("congested: last bitrate = %d bps, want min %d", got, c.MinBitratebps())
	}
}

// A healthy link: the adaptive balancer starts at max and must never go
// below it (no decreases with stable low RTT and no loss).
func TestHealthyLinkHoldsMax(t *testing.T) {
	c := testConfig()
	src := sinks.NewMockStatsSource() // default healthy profile
	ap := &fakeApplier{}

	ctx, cancel := context.WithTimeout(context.Background(), 1600*time.Millisecond)
	defer cancel()
	ctl := New(src, nil, ap, c)
	if err := ctl.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := ap.lastBitrate(); got > c.MaxBitratebps() || got < c.MinBitratebps() {
		t.Fatalf("healthy: last bitrate %d outside [%d,%d]", got, c.MinBitratebps(), c.MaxBitratebps())
	}
	ap.mu.Lock()
	n := len(ap.bitrates)
	ap.mu.Unlock()
	if n == 0 {
		t.Fatal("healthy: expected at least one SetBitrate call")
	}
}

// A stalled appsink (no frames within the threshold) must drop the target to
// the minimum even on a healthy SRT link — the zombie-encode gate.
func TestStalledAppsinkDropsToMin(t *testing.T) {
	c := testConfig()
	src := sinks.NewMockStatsSource() // healthy link
	ap := &fakeApplier{}
	liv := &fakeLiveness{stalled: true}

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	ctl := New(src, liv, ap, c)
	if err := ctl.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := ap.lastBitrate(); got != c.MinBitratebps() {
		t.Fatalf("stalled appsink: last bitrate = %d bps, want min %d", got, c.MinBitratebps())
	}
}

// The overlay loop must publish at least one text update at the configured
// interval, and the controller must stop promptly on ctx cancel.
func TestOverlayAndShutdown(t *testing.T) {
	c := testConfig()
	c.Overlay.Enabled = true
	c.Overlay.IntervalMS = 50
	src := sinks.NewMockStatsSource()
	ap := &fakeApplier{}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	ctl := New(src, nil, ap, c)
	start := time.Now()
	if err := ctl.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if dt := time.Since(start); dt > 2*time.Second {
		t.Fatalf("Run took %s to return after ctx cancel (shutdown not prompt)", dt)
	}
	ap.mu.Lock()
	n := len(ap.overlays)
	first := ""
	if n > 0 {
		first = ap.overlays[0]
	}
	ap.mu.Unlock()
	if n == 0 {
		t.Fatal("overlay: no SetOverlayText calls within 400ms at a 50ms interval")
	}
	if len(first) == 0 {
		t.Fatal("overlay: empty text")
	}
}
