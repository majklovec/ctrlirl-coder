package browser

import (
	"context"
	"testing"
	"time"

	"github.com/go-gst/go-glib/pkg/gobject/v2"
	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
	"ctrlirl-coder/internal/layer"
	"ctrlirl-coder/internal/scene"
)

// fakeCDP fakes the Chrome CDP session surface used by pushFrame: the
// feeder's exit check reads Stopped(); capture is never called here.
type fakeCDP struct{ stopped chan struct{} }

func (f *fakeCDP) Stopped() <-chan struct{} { return f.stopped }

// newReadyAppSrc builds an appsrc that is FLUSHING (pipeline at READY, as
// during a scene-switch freeze).
func newReadyAppSrc(t *testing.T) *gstutil.Pipeline {
	t.Helper()
	gstutil.EnsureInit()
	pip, err := gstutil.FromLaunch("appsrc name=src is-live=true max-buffers=10 ! fakesink")
	if err != nil {
		t.Fatal(err)
	}
	el := pip.As().GetByName("src")
	if el == nil {
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
		t.Fatal("no appsrc element")
	}
	// READY: pushBuffer must answer FLUSHING (0).
	if err := pip.SetState(gst.StateReady); err != nil {
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
		t.Fatal(err)
	}
	return pip
}

// TestPushFrameSurvivesFlushFreeze is the regression test for the browser
// layer disappearing after a scene switch: the pipeline is frozen at READY
// (appsrc FLUSHING) for a while, then resumed. pushFrame must ride out the
// freeze and push once the appsrc accepts again, not give up.
func TestPushFrameSurvivesFlushFreeze(t *testing.T) {
	pip := newReadyAppSrc(t)
	defer func() {
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
	}()
	el := pip.As().GetByName("src")
	if el == nil {
		t.Fatal("no appsrc element")
	}

	b := &BrowserOverlay{
		BaseLayer: layer.BaseLayer{L: scene.Layer{ID: "browser-red"}},
		appSrc:    el,
		stop:      make(chan struct{}),
	}
	sess := &fakeCDP{stopped: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan bool, 1)
	go func() { done <- b.pushFrame(ctx, []byte{0x01}, sess) }()

	// Freeze lasts ~600ms: 3+ backoff rounds at the default 200ms pace.
	time.Sleep(600 * time.Millisecond)
	if err := pip.SetState(gst.StatePlaying); err != nil {
		t.Fatal(err)
	}

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("pushFrame gave up during/after the freeze; want it to survive and push")
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("pushFrame did not complete after the pipeline resumed")
	}
}
