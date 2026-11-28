package gstutil

import (
	"runtime"
	"testing"
	"time"

	"github.com/go-gst/go-glib/pkg/gobject/v2"
	"github.com/go-gst/go-gst/pkg/gst"
)

func playingAppSrc(tb testing.TB, launch string) (*Pipeline, gst.Element) {
	tb.Helper()
	EnsureInit()
	pip, err := FromLaunch(launch)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
	})
	src := pip.As().GetByName("src")
	if src == nil {
		tb.Fatal("no appsrc element")
	}
	return pip, src
}

// Regression: gst_app_src_push_buffer owns the buffer on every flow result,
// so PushBytes must not unref after a FLUSHING push (scene switch). The old
// unref double-freed and aborted in gst_app_src_create.
func TestPushBytesSurvivesFlushingAndStateCycles(t *testing.T) {
	pipeline, src := playingAppSrc(t,
		"appsrc name=src is-live=true do-timestamp=true format=time block=false "+
			"caps=video/x-raw,format=BGRA,width=64,height=48,framerate=30/1 ! fakesink sync=false")
	frame := make([]byte, 64*48*4)

	for cycle := 0; cycle < 3; cycle++ {
		if err := pipeline.SetState(gst.StatePlaying); err != nil {
			t.Fatal(err)
		}
		ok := 0
		for i := 0; i < 60; i++ {
			if PushBytes(src, frame) == gst.FlowOK {
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
			if flow := PushBytes(src, frame); flow != gst.FlowFlushing {
				t.Fatalf("cycle %d: push into stopped appsrc = %v, want FLUSHING", cycle, flow)
			}
		}
		runtime.GC()
	}
}

func TestPushBytesRejectsNothing(t *testing.T) {
	_, src := playingAppSrc(t, "appsrc name=src ! fakesink")
	if flow := PushBytes(src, nil); flow != gst.FlowError {
		t.Errorf("empty data: flow = %v, want ERROR", flow)
	}
	if flow := PushBytes(nil, []byte{1}); flow != gst.FlowError {
		t.Errorf("nil appsrc: flow = %v, want ERROR", flow)
	}
}

// BenchmarkPushBytes benchmarks the appsrc push path for a 300 KB frame:
// GstBuffer allocate + copy + thread-safe push in one C call.
func BenchmarkPushBytes(b *testing.B) {
	b.ReportAllocs()
	pip, src := playingAppSrc(b, "appsrc name=src is-live=true max-buffers=10 ! fakesink")
	if err := pip.SetState(gst.StatePlaying); err != nil {
		b.Fatal(err)
	}
	// Pushing before the appsrc itself has reached PLAYING aborts inside
	// GStreamer, so wait for the element (not just the pipeline call).
	deadline := time.Now().Add(10 * time.Second)
	for src.GetCurrentState() != gst.StatePlaying {
		if time.Now().After(deadline) {
			b.Fatal("appsrc never reached PLAYING")
		}
		time.Sleep(10 * time.Millisecond)
	}
	frame := make([]byte, 300*1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if flow := PushBytes(src, frame); flow != gst.FlowOK {
			b.Fatalf("push flow = %v, want OK", flow)
		}
	}
}
