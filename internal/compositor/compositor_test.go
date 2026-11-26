package compositor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/scene"
)

// testPipeline is a live layered trunk: compositor in the middle, terminal
// appsink at the end (the §2.1 contract), no encoder (adaptive bitrate off —
// these tests only exercise layer attach/teardown and the command queue).
// The audio branch (audiotestsrc ... t1-f00-volume ... fakesink) exists for
// TestAlphaMutesAudio: it gives the alpha->mute path a named volume element
// to drive, drained by fakesink.
const testPipeline = "videotestsrc is-live=true pattern=ball " +
	"! video/x-raw,format=I420,width=640,height=360,framerate=30/1 " +
	"! videoconvert ! video/x-raw,format=BGRA " +
	"! compositor name=compositor background=black " +
	"! videoconvert ! video/x-raw,format=BGRA " +
	"! appsink name=appsink " +
	"audiotestsrc is-live=true ! audioconvert " +
	"! volume name=t1-f00-volume ! fakesink"

// newTestCompositor parses the layered test pipeline and leaves it in NULL.
func newTestCompositor(t *testing.T) *CompositorPipeline {
	t.Helper()
	cp, err := New(testPipeline)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := cp.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return cp
}

func textLayer(id string, x, y float64) scene.Layer {
	return scene.Layer{
		ID:   id,
		Type: "text",
		Text: "hi",
		Properties: map[string]float64{
			"zorder": 2, "xpos": x, "ypos": y,
			"width": 320, "height": 80,
		},
	}
}

// ctxWithDeadline fails the test if a command does not return in time — a
// wedged command goroutine would otherwise hang the suite forever.
func ctxWithDeadline(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// TestAddRemoveUpdateTextLayer is the core attach/teardown cycle on a live
// pipeline: attach while NULL, start, update pad props in place, remove.
func TestAddRemoveUpdateTextLayer(t *testing.T) {
	cp := newTestCompositor(t)
	ctx := ctxWithDeadline(t, 15*time.Second)

	if err := cp.AddLayer(ctx, textLayer("t1", 0, 0)); err != nil {
		t.Fatalf("AddLayer: %v", err)
	}
	if _, ok := cp.layers["t1"]; !ok {
		t.Fatal("layer t1 not in the layer map")
	}

	if err := cp.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// In-place pad property update on the running pipeline.
	updated := map[string]float64{"xpos": 64, "ypos": 32}
	if err := cp.UpdateLayerProps(ctx, "t1", updated); err != nil {
		t.Fatalf("UpdateLayerProps: %v", err)
	}
	if h := cp.layers["t1"]; h.props["xpos"] != 64 || h.props["ypos"] != 32 {
		t.Fatalf("props after update = %v", h.props)
	}
	// Unknown property keys must be ignored, not stored.
	_ = cp.UpdateLayerProps(ctx, "t1", map[string]float64{"bogus": 1})
	if _, leak := cp.layers["t1"].props["bogus"]; leak {
		t.Fatal("unknown property key leaked into pad props")
	}

	if err := cp.RemoveLayer(ctx, "t1"); err != nil {
		t.Fatalf("RemoveLayer: %v", err)
	}
	if _, ok := cp.layers["t1"]; ok {
		t.Fatal("layer t1 still in the map after RemoveLayer")
	}
}

// TestAlphaMutesAudio verifies that hiding a layer via pad alpha 0 also
// mutes its audio chain in place (the compositor's pad alpha only affects
// the video overlay; without the follow-through a hidden layer's audio
// keeps mixing into the stream), and that alpha > 0 un-mutes.
func TestAlphaMutesAudio(t *testing.T) {
	cp := newTestCompositor(t)
	ctx := ctxWithDeadline(t, 15*time.Second)

	if err := cp.AddLayer(ctx, textLayer("t1", 0, 0)); err != nil {
		t.Fatalf("AddLayer: %v", err)
	}
	if err := cp.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wire the pipeline's volume element into the handle as the layer's
	// audio filter (the text layer has no real audio chain).
	vol := cp.pipeline.As().GetByName("t1-f00-volume")
	if vol == nil {
		t.Fatal("volume element t1-f00-volume not found in test pipeline")
	}
	h := cp.layers["t1"]
	h.spec.Filters = []scene.FilterSpec{{Kind: "volume"}}
	h.volFilter = h.volumeFilterIndex()
	if h.volFilter != 0 {
		t.Fatalf("volFilter = %d, want 0", h.volFilter)
	}
	h.audioChain = []gst.Element{vol}

	if err := cp.UpdateLayerProps(ctx, "t1", map[string]float64{"alpha": 0}); err != nil {
		t.Fatalf("UpdateLayerProps alpha=0: %v", err)
	}
	if muted := vol.ObjectProperty("mute"); muted != true {
		t.Fatalf("mute after alpha 0 = %v, want true", muted)
	}

	if err := cp.UpdateLayerProps(ctx, "t1", map[string]float64{"alpha": 1}); err != nil {
		t.Fatalf("UpdateLayerProps alpha=1: %v", err)
	}
	if muted := vol.ObjectProperty("mute"); muted != false {
		t.Fatalf("mute after alpha 1 = %v, want false", muted)
	}
}

// TestAddDuplicateID verifies the duplicate-id attach is rejected and leaves
// the attached set untouched.
func TestAddDuplicateID(t *testing.T) {
	cp := newTestCompositor(t)
	ctx := ctxWithDeadline(t, 15*time.Second)

	if err := cp.AddLayer(ctx, textLayer("dup", 0, 0)); err != nil {
		t.Fatalf("first AddLayer: %v", err)
	}
	err := cp.AddLayer(ctx, textLayer("dup", 10, 10))
	if err == nil || !strings.Contains(err.Error(), "already attached") {
		t.Fatalf("duplicate AddLayer err = %v, want 'already attached'", err)
	}
	if len(cp.layers) != 1 {
		t.Fatalf("layer map = %d entries, want 1 (no half-attached second copy)", len(cp.layers))
	}
}

// TestRemoveUnknownLayer verifies removing a layer that was never attached
// is a clean error.
func TestRemoveUnknownLayer(t *testing.T) {
	cp := newTestCompositor(t)
	ctx := ctxWithDeadline(t, 15*time.Second)
	if err := cp.RemoveLayer(ctx, "ghost"); err == nil {
		t.Fatal("RemoveLayer(ghost) succeeded, want error")
	}
	if err := cp.UpdateLayerProps(ctx, "ghost", map[string]float64{"xpos": 1}); err == nil {
		t.Fatal("UpdateLayerProps(ghost) succeeded, want error")
	}
}

// TestSuspendResumeNesting verifies the batch counter: nested suspends only
// freeze on the first, resume only restores on the last, and an
// unmatched resume is an error.
func TestSuspendResumeNesting(t *testing.T) {
	cp := newTestCompositor(t)
	ctx := ctxWithDeadline(t, 15*time.Second)
	if err := cp.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := cp.SuspendStream(ctx); err != nil {
		t.Fatalf("SuspendStream #1: %v", err)
	}
	if got := cp.suspended.Load(); got != 1 {
		t.Fatalf("suspended = %d, want 1", got)
	}
	if err := cp.SuspendStream(ctx); err != nil {
		t.Fatalf("SuspendStream #2 (nested): %v", err)
	}
	if got := cp.suspended.Load(); got != 2 {
		t.Fatalf("nested suspended = %d, want 2", got)
	}
	if err := cp.ResumeStream(ctx); err != nil {
		t.Fatalf("ResumeStream #1: %v", err)
	}
	if err := cp.ResumeStream(ctx); err != nil {
		t.Fatalf("ResumeStream #2: %v", err)
	}
	if got := cp.suspended.Load(); got != 0 {
		t.Fatalf("suspended after full release = %d, want 0", got)
	}

	// Unmatched resume is an error and must not leave the counter negative.
	if err := cp.ResumeStream(ctx); err == nil {
		t.Fatal("ResumeStream without Suspend succeeded, want error")
	}
	if cp.suspended.Load() < 0 {
		t.Fatalf("suspended counter negative: %d", cp.suspended.Load())
	}
}

// TestStopIdempotent verifies Stop can be called repeatedly without hanging
// or erroring (ordered teardown calls it alongside per-layer Close).
func TestStopIdempotent(t *testing.T) {
	cp, err := New(testPipeline)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := ctxWithDeadline(t, 15*time.Second)
	if err := cp.AddLayer(ctx, textLayer("t1", 0, 0)); err != nil {
		t.Fatalf("AddLayer: %v", err)
	}
	if err := cp.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan error, 2)
	go func() { done <- cp.Stop() }()
	go func() { done <- cp.Stop() }()
	select {
	case e1 := <-done:
		if e1 != nil {
			t.Fatalf("Stop #1: %v", e1)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Stop #1 hung")
	}
	select {
	case e2 := <-done:
		if e2 != nil {
			t.Fatalf("Stop #2: %v", e2)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Stop #2 hung")
	}
}

// TestCommandsAfterStop verifies that once the command goroutine is down,
// submitted commands return ErrStopped instead of hanging.
func TestCommandsAfterStop(t *testing.T) {
	cp, err := New(testPipeline)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := ctxWithDeadline(t, 15*time.Second)
	if err := cp.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cp.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if err := cp.SuspendStream(ctx); err != ErrStopped {
		t.Fatalf("SuspendStream after Stop = %v, want ErrStopped", err)
	}
	if err := cp.AddLayer(ctx, textLayer("late", 0, 0)); err != ErrStopped {
		t.Fatalf("AddLayer after Stop = %v, want ErrStopped", err)
	}
	if err := cp.RemoveLayer(ctx, "late"); err != ErrStopped {
		t.Fatalf("RemoveLayer after Stop = %v, want ErrStopped", err)
	}
}

// TestNoCompositorPipelines verifies a layerless pipeline (no compositor
// element) loads fine and layer ops fail with ErrNoCompositor.
func TestNoCompositorPipelines(t *testing.T) {
	launch := "videotestsrc is-live=true ! video/x-raw,format=I420 " +
		"! appsink name=appsink"
	cp, err := New(launch)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = cp.Stop() }()
	if cp.HasCompositor() {
		t.Fatal("HasCompositor = true on a layerless pipeline")
	}
	ctx := ctxWithDeadline(t, 10*time.Second)
	if err := cp.AddLayer(ctx, textLayer("t1", 0, 0)); err != ErrNoCompositor {
		t.Fatalf("AddLayer = %v, want ErrNoCompositor", err)
	}
}

// TestBuildLayerDispatch covers the scene-type → concrete-layer dispatch.
func TestBuildLayerDispatch(t *testing.T) {
	l := scene.Layer{ID: "b2", Type: scene.LayerTypeBrowser, Source: "https://example.com"}
	got, err := buildLayer(l)
	if err != nil {
		t.Fatalf("buildLayer(browser) error: %v", err)
	}
	if got.Type() != "browser" {
		t.Errorf("buildLayer returned type %q, want browser", got.Type())
	}
	if _, err := buildLayer(scene.Layer{ID: "x", Type: "nope"}); err == nil {
		t.Error("buildLayer(unknown) expected error, got nil")
	}
}
