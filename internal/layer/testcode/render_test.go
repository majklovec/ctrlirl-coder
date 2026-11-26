package testcode

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/go-gst/go-glib/pkg/gobject/v2"
	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstpull"
	"ctrlirl-coder/internal/gstutil"
	"ctrlirl-coder/internal/scene"
)

func TestQRPayloadUsesUTCAndFrameNumber(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 678_000_000, time.FixedZone("offset", 2*60*60))
	got := qrPayload(now, 6)
	want := "CTRLIRL|2026-01-02T01:04:05.678Z|f=000006"
	if got != want {
		t.Fatalf("qrPayload() = %q, want %q", got, want)
	}
	if len(got) > qrByteCapacity(3) {
		t.Fatalf("payload length %d does not fit QR version 3", len(got))
	}
}

func TestRenderProducesTransparentOverlayAndClipsSmallSurfaces(t *testing.T) {
	r := &testcodeRenderer{w: 1280, h: 720, fps: 30}
	frame := r.render(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), 1)
	if len(frame) != r.w*r.h*4 {
		t.Fatalf("frame length = %d, want %d", len(frame), r.w*r.h*4)
	}
	if got := frame[(300*r.w+900)*4+3]; got != 0 {
		t.Fatalf("uncovered pixel alpha = %d, want transparent", got)
	}
	if got := frame[(21*r.w+25)*4+3]; got != 0xFF {
		t.Fatalf("label panel alpha = %d, want opaque", got)
	}

	small := &testcodeRenderer{w: 50, h: 59, fps: 30}
	if got := small.render(time.Now(), 1); len(got) != 50*59*4 {
		t.Fatalf("small frame length = %d, want %d", len(got), 50*59*4)
	}
}

func TestBuildChainCreatesAppSrcSurface(t *testing.T) {
	gstutil.EnsureInit()
	source := NewTestCodeSource(scene.Layer{
		ID:   "testcode",
		Type: scene.LayerTypeTestCode,
		Properties: map[string]float64{
			"width": 1280, "height": 720, "framerate": 30,
		},
	})
	chain, err := source.BuildChain()
	if err != nil {
		t.Fatalf("BuildChain: %v", err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if len(chain.Elements) != 5 {
		t.Fatalf("chain elements = %d, want appsrc + four surface elements", len(chain.Elements))
	}
}

func TestTestCodeStreamsFrames(t *testing.T) {
	gstutil.EnsureInit()
	pipeline, err := gstutil.FromLaunch("identity name=entry ! appsink name=appsink sync=false")
	if err != nil {
		t.Fatal(err)
	}
	source := NewTestCodeSource(scene.Layer{
		ID: "testcode",
		Properties: map[string]float64{
			"width": 320, "height": 240, "framerate": 30,
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := false
	defer func() {
		cancel()
		source.Close()
		pipeline.SetState(gst.StateNull)
		if started {
			<-done
		}
		gobject.UnsafeObjectUnref(pipeline.As())
	}()
	puller := gstpull.Attach(pipeline.As().GetByName("appsink"), 1000)
	chain, err := source.BuildChain()
	if err != nil {
		t.Fatal(err)
	}
	if !pipeline.As().AddMany(chain.Elements...) {
		t.Fatal("add testcode chain to pipeline")
	}
	if err := chain.LinkChain(); err != nil {
		t.Fatal(err)
	}
	if !chain.Elements[len(chain.Elements)-1].Link(pipeline.As().GetByName("entry")) {
		t.Fatal("link testcode output")
	}
	if err := pipeline.SetState(gst.StatePlaying); err != nil {
		t.Fatal(err)
	}
	started = true
	go func() {
		defer close(done)
		puller.Run(ctx, func(data []byte) (int, error) {
			if len(data) != 320*240*4 {
				return 0, fmt.Errorf("unexpected frame size %d", len(data))
			}
			runtime.GC()
			return len(data), nil
		})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for puller.Frames() < 30 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if puller.Frames() < 30 {
		t.Fatalf("testcode produced %d frames, want at least 30", puller.Frames())
	}
	if puller.Dropped() != 0 {
		t.Fatalf("testcode produced %d invalid frames", puller.Dropped())
	}
}

// Regression: gst_app_src_push_buffer owns the buffer on every flow result.
// A pusher that unref'd after a FLUSHING/OK push double-freed and later
// aborted in gst_app_src_create ("code should not be reached").
func TestPushAppSrcFeedSurvivesFlushingAndStateCycles(t *testing.T) {
	gstutil.EnsureInit()
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
			flow, err := pushAppSrcFeed(src, frame)
			if err != nil {
				t.Fatal(err)
			}
			if flow == gst.FlowOK {
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
			flow, err := pushAppSrcFeed(src, frame)
			if err != nil {
				t.Fatal(err)
			}
			if flow != gst.FlowFlushing {
				t.Fatalf("cycle %d: push into stopped appsrc = %v, want FLUSHING", cycle, flow)
			}
		}
		runtime.GC()
	}
}
