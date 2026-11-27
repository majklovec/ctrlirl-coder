package gstpull

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-gst/go-glib/pkg/gobject/v2"
	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
)

// TestAppsinkPull drives the real cgo path: a finite (but live-paced)
// videotestsrc pipeline ending in an appsink, pulled by the Go drain
// goroutine. Verifies frame count, byte count, caps framerate, PTS, liveness
// and the pull loop's exit on EOS (pull returns NULL once the sink is in EOS).
// Live pacing (30 frames at 30 fps ≈ 1 s) lets the rolling throughput window
// finalize at least once.
//
// Live-paced sources are racy on small/loaded CI runners: if the test process
// is descheduled while the source runs out, the drop=true appsink flushes its
// buffers before the puller drains them and the run yields 0 frames. The run
// is retried up to 3 times to make the flake non-blocking.
func TestAppsinkPull(t *testing.T) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if lastErr = runAppsinkPull(t); lastErr == nil {
			if attempt > 1 {
				t.Logf("passed on attempt %d", attempt)
			}
			return
		}
		t.Logf("attempt %d flaked: %v", attempt, lastErr)
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	t.Fatalf("failed after 3 attempts: %v", lastErr)
}

func runAppsinkPull(t *testing.T) error {
	t.Helper()
	gstutil.EnsureInit()
	pip, err := gstutil.FromLaunch(
		"videotestsrc is-live=true num-buffers=30 pattern=ball " +
			"! video/x-raw,format=I420,width=64,height=64,framerate=30/1 " +
			"! videoconvert ! appsink name=appsink")
	if err != nil {
		t.Fatalf("parse pipeline: %v", err)
	}
	defer func() {
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
	}()

	el := pip.As().GetByName("appsink")
	if el == nil {
		t.Fatal("pipeline has no appsink element")
	}

	p := Attach(el, 100)
	var sent atomic.Int64
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer close(done)
		p.Run(ctx, func(b []byte) (int, error) {
			sent.Add(int64(len(b)))
			return len(b), nil
		})
	}()

	if err := pip.SetState(gst.StatePlaying); err != nil {
		t.Fatalf("playing: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("pull loop did not exit after EOS")
	}

	if got := p.Frames(); got != 30 {
		return fmt.Errorf("frames = %d, want 30", got)
	}
	if p.BytesPulled() == 0 {
		return fmt.Errorf("no bytes pulled")
	}
	if got := p.BytesSent(); got != p.BytesPulled() {
		return fmt.Errorf("sent %d != pulled %d", got, p.BytesPulled())
	}
	if got := p.ThroughputKbps(); got == 0 {
		return fmt.Errorf("throughput kbps = 0 after 30 frames")
	}
	if fr := p.Framerate(); fr < 29.9 || fr > 30.1 {
		return fmt.Errorf("framerate = %v, want 30", fr)
	}
	if p.LastPTS() == 0 {
		return fmt.Errorf("last PTS = 0")
	}
	if p.Dropped() != 0 {
		return fmt.Errorf("dropped = %d, want 0", p.Dropped())
	}
	return nil
}

// TestStallDetection checks the frame_liveness math: fresh after a pull,
// stalled after the threshold elapses with no frames.
func TestStallDetection(t *testing.T) {
	gstutil.EnsureInit()
	pip, err := gstutil.FromLaunch(
		"videotestsrc num-buffers=3 pattern=ball " +
			"! video/x-raw,format=I420,width=64,height=64,framerate=30/1 " +
			"! videoconvert ! appsink name=appsink")
	if err != nil {
		t.Fatalf("parse pipeline: %v", err)
	}
	defer func() {
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
	}()

	el := pip.As().GetByName("appsink")
	p := Attach(el, 120) // 120 ms threshold

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx, func(b []byte) (int, error) { return len(b), nil })
	if err := pip.SetState(gst.StatePlaying); err != nil {
		t.Fatalf("playing: %v", err)
	}

	// Frames flow for a moment.
	deadline := time.Now().Add(5 * time.Second)
	for p.Frames() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p.Frames() == 0 {
		t.Fatal("no frames pulled")
	}
	if p.Stalled() {
		t.Fatal("stalled right after pulling frames")
	}
	if age := p.LastFrameAge(); age >= 120*time.Millisecond {
		t.Fatalf("last frame age %v already past threshold", age)
	}

	// After EOS + the threshold, production is stalled.
	time.Sleep(250 * time.Millisecond)
	if !p.Stalled() {
		t.Fatal("not stalled 250ms after the last frame (threshold 120ms)")
	}
}
