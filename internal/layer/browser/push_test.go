package browser

import (
	"runtime"
	"testing"
	"time"

	"github.com/go-gst/go-glib/pkg/gobject/v2"
	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
)

// Regression: gst_app_src_push_buffer owns the buffer on every flow result,
// so pushBuffer must not unref after a FLUSHING push (scene switch). The old
// unref double-freed and aborted in gst_app_src_create.
func TestPushBufferSurvivesFlushingAndStateCycles(t *testing.T) {
	pipeline, err := gstutil.FromLaunch(
		"appsrc name=src is-live=true do-timestamp=true format=time block=false " +
			"caps=video/x-raw,format=BGRA,width=64,height=48,framerate=30/1 ! fakesink sync=false")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		pipeline.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pipeline.As())
	}()
	src := pipeline.As().GetByName("src")
	frame := make([]byte, 64*48*4)

	for cycle := 0; cycle < 3; cycle++ {
		if err := pipeline.SetState(gst.StatePlaying); err != nil {
			t.Fatal(err)
		}
		ok := 0
		for i := 0; i < 60; i++ {
			if pushBuffer(src, frame) == 1 {
				ok++
			}
			time.Sleep(time.Millisecond)
		}
		if ok == 0 {
			t.Fatalf("cycle %d: no buffer accepted while PLAYING", cycle)
		}
		if err := pipeline.SetState(gst.StateReady); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 200; i++ {
			if n := pushBuffer(src, frame); n != 0 {
				t.Fatalf("cycle %d: push into stopped appsrc = %d, want 0 (FLUSHING)", cycle, n)
			}
		}
		runtime.GC()
	}
}
