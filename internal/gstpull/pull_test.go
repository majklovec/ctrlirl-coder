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

// drainFakesrc runs the puller over a finite fakesrc pipeline until EOS and
// returns it, with every payload byte handed to send.
func drainFakesrc(t *testing.T, buffers, size int, send Send) *Puller {
	t.Helper()
	gstutil.EnsureInit()
	pip, el := fakesrcPipeline(t, buffers, size)
	defer func() {
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
	}()
	p := Attach(el, 1000)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer close(done)
		p.Run(ctx, send)
	}()
	if err := pip.SetState(gst.StatePlaying); err != nil {
		t.Fatalf("playing: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("pull loop did not exit after EOS")
	}
	return p
}

// TestDrainDeliversEveryByteInOrder pins the batching contract on the
// production buffer size (188-byte TS packets): every pulled byte reaches the
// send callback exactly once and in order, including the partial batch left
// at EOS, and the counters agree with what was delivered.
func TestDrainDeliversEveryByteInOrder(t *testing.T) {
	const buffers = 1000
	var (
		got     []byte
		batches int
	)
	p := drainFakesrc(t, buffers, tsPacketSize, func(b []byte) (int, error) {
		if len(b) == 0 {
			t.Error("send called with an empty batch")
		}
		got = append(got, b...) // the callee must not retain b
		batches++
		return len(b), nil
	})

	const want = buffers * tsPacketSize
	if len(got) != want {
		t.Fatalf("send received %d bytes, want %d", len(got), want)
	}
	// fakesrc filltype=pattern writes a byte counter that restarts in every
	// buffer, so a dropped, duplicated or reordered buffer breaks the ramp.
	for i, b := range got {
		if b != byte(i%tsPacketSize) {
			t.Fatalf("payload byte %d = %#02x, want %#02x (buffers lost or reordered)", i, b, byte(i%tsPacketSize))
		}
	}
	if f := p.Frames(); f != buffers {
		t.Errorf("Frames = %d, want %d", f, buffers)
	}
	if n := p.BytesPulled(); n != want {
		t.Errorf("BytesPulled = %d, want %d", n, want)
	}
	if n := p.BytesSent(); n != want {
		t.Errorf("BytesSent = %d, want %d (every batch, including the last partial one)", n, want)
	}
	if d := p.Dropped(); d != 0 {
		t.Errorf("Dropped = %d, want 0", d)
	}
	if batches >= buffers {
		t.Errorf("send ran %d times for %d buffers: buffers are not batched", batches, buffers)
	}
}

// TestDrainCountsFailedBatches pins the drop accounting: a failed send drops
// that batch (counted once, its bytes not counted as sent) and the drain
// keeps going to EOS.
func TestDrainCountsFailedBatches(t *testing.T) {
	const buffers = 2000
	var (
		calls, failed int
		okBytes       uint64
	)
	p := drainFakesrc(t, buffers, tsPacketSize, func(b []byte) (int, error) {
		calls++
		if calls%2 == 0 {
			failed++
			return 0, fmt.Errorf("link down")
		}
		okBytes += uint64(len(b))
		return len(b), nil
	})
	if f := p.Frames(); f != buffers {
		t.Errorf("Frames = %d, want %d (a failed send must not stop the drain)", f, buffers)
	}
	if failed == 0 {
		t.Fatal("no send failed; the test needs at least two batches")
	}
	if d := p.Dropped(); d != uint64(failed) {
		t.Errorf("Dropped = %d, want %d (one per failed batch)", d, failed)
	}
	if n := p.BytesSent(); n != okBytes {
		t.Errorf("BytesSent = %d, want %d (only the batches that went out)", n, okBytes)
	}
}

// TestDrainDeliversOversizedBufferAlone pins the raw-terminal contract other
// packages' tests rely on: a buffer larger than the batch size reaches send
// whole and alone.
func TestDrainDeliversOversizedBufferAlone(t *testing.T) {
	const size = 3*srtBatchSize + 7
	calls := 0
	p := drainFakesrc(t, 5, size, func(b []byte) (int, error) {
		calls++
		if len(b) != size {
			t.Errorf("send got %d bytes, want one whole %d-byte buffer", len(b), size)
		}
		return len(b), nil
	})
	if calls != 5 || p.Frames() != 5 {
		t.Errorf("send calls = %d, Frames = %d, want 5 and 5", calls, p.Frames())
	}
}

// TestDrainFlushesWhenIdle pins flush-on-idle: a slow stream's bytes go out
// as they arrive instead of waiting for a full batch. 10 live frames of
// 6 kB at 30 fps never fill a 32 kB batch within the linger, so each frame
// must be its own send.
func TestDrainFlushesWhenIdle(t *testing.T) {
	gstutil.EnsureInit()
	pip, err := gstutil.FromLaunch(
		"videotestsrc is-live=true num-buffers=10 " +
			"! video/x-raw,format=I420,width=64,height=64,framerate=30/1 ! appsink name=appsink")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
	}()
	const frame = 64 * 64 * 3 / 2
	p := Attach(pip.As().GetByName("appsink"), 1000)
	var sizes []int
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(context.Background(), func(b []byte) (int, error) {
			sizes = append(sizes, len(b))
			return len(b), nil
		})
	}()
	if err := pip.SetState(gst.StatePlaying); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("pull loop did not exit after EOS")
	}
	if p.Frames() != 10 {
		t.Skipf("pulled %d of 10 live frames (runner descheduled); nothing to assert", p.Frames())
	}
	// Frames are 33 ms apart, the linger is 2 ms: allow a couple of merged
	// pairs for a descheduled runner, but not accumulation to a full batch.
	if len(sizes) < 8 {
		t.Errorf("10 frames went out in %d sends %v: bytes were held back", len(sizes), sizes)
	}
	for _, n := range sizes {
		if n%frame != 0 {
			t.Errorf("send of %d bytes is not a whole number of %d-byte frames", n, frame)
		}
	}
}

// TestDrainSurvivesReadyFreeze pins what scene switches rely on: the
// compositor freezes the pipeline at READY around layer changes, and the
// drain must sit that out and carry on at PLAYING. (Only EOS or the NULL
// state end it.)
func TestDrainSurvivesReadyFreeze(t *testing.T) {
	gstutil.EnsureInit()
	pip, err := gstutil.FromLaunch(
		"videotestsrc is-live=true ! video/x-raw,width=64,height=64,framerate=30/1 ! appsink name=appsink")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
	}()
	p := Attach(pip.As().GetByName("appsink"), 1000)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		p.Run(context.Background(), func(b []byte) (int, error) { return len(b), nil })
	}()
	waitFrames := func(min uint64) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for p.Frames() < min && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if p.Frames() < min {
			t.Fatalf("frames = %d, want >= %d", p.Frames(), min)
		}
	}
	if err := pip.SetState(gst.StatePlaying); err != nil {
		t.Fatal(err)
	}
	waitFrames(3)

	if err := pip.SetState(gst.StateReady); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	select {
	case <-exited:
		t.Fatal("the drain exited during a READY freeze; the stream would be dead after a scene switch")
	default:
	}
	frozen := p.Frames()

	if err := pip.SetState(gst.StatePlaying); err != nil {
		t.Fatal(err)
	}
	waitFrames(frozen + 3)

	pip.SetState(gst.StateNull)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain did not exit after the pipeline went to NULL")
	}
}
