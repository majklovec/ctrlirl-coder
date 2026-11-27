package layer

import (
	"context"
	"testing"
	"time"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstpull"
	"ctrlirl-coder/internal/gstutil"
	"ctrlirl-coder/internal/scene"
)

// TestRotateTransparentCorners is the end-to-end check for the rotate
// transparency bug: a rotated frame must carry alpha=0 (transparent) in the
// un-covered corners, not opaque black.
//
// The GStreamer geometrictransform (rotate's base) clears its output frame
// with memset(0) before mapping. That clear is transparent only when the
// output format has an alpha channel. buildFilters pins rotate's output to
// BGRA so the un-mapped area is transparent. This test runs the REAL filter
// chain produced by buildFilters on a non-square source with a 90-degree
// rotation (a square rotated 90 degrees covers itself completely, so a
// non-square source is required to leave uncovered corners) through a live
// pipeline and samples the corners of the pulled BGRA frame.
func TestRotateTransparentCorners(t *testing.T) {
	requireFactories(t, "videotestsrc", "rotate", "videoconvert", "capsfilter", "appsink")
	gstutil.EnsureInit()

	l := scene.Layer{
		ID:      "rot",
		Type:    "video",
		Filters: []scene.FilterSpec{{Kind: "videorotate", Params: map[string]any{"angle": 90.0}}},
	}
	filters, err := VideoFilters(l)
	if err != nil {
		t.Fatalf("VideoFilters: %v", err)
	}

	const (
		wantW = 320
		wantH = 240
	)

	// A launch string gives us a real GstPipeline to use as a bin. It must
	// contain at least two elements for gst_parse_launch to return a pipeline
	// (a single bare element is returned as-is), so a throwaway
	// videotestsrc-then-fakesink pair forces the pipeline; it runs harmlessly
	// in the background and is not part of the chain under test. The real
	// chain (source -> filter chain -> appsink) is wired programmatically so
	// this exercises the exact elements buildFilters produces.
	base, err := gstutil.FromLaunch("videotestsrc ! fakesink")
	if err != nil {
		t.Fatalf("FromLaunch: %v", err)
	}
	pipe := base.As()

	// Non-square source so a 90-degree rotation leaves uncovered corners.
	// videotestsrc sizes via caps (not a property); MakeElement routes the
	// int num-buffers through the cgo setter (go-gst drops plain ints).
	src, err := MakeElement("videotestsrc", "rot-src", map[string]any{"is-live": false, "num-buffers": 30})
	if err != nil {
		t.Fatalf("videotestsrc: %v", err)
	}
	sizeCap, err := MakeElement("capsfilter", "rot-sizedim", nil)
	if err != nil {
		t.Fatalf("size capsfilter: %v", err)
	}
	sizeCap.SetObjectProperty("caps", gst.CapsFromString(
		"video/x-raw,width=320,height=240,format=I420"))

	if !pipe.Add(src) {
		t.Fatal("failed to add source")
	}
	if !pipe.Add(sizeCap) {
		t.Fatal("failed to add size capsfilter")
	}
	for _, f := range filters {
		if !pipe.Add(f) {
			t.Fatalf("failed to add filter element %s", f.GetName())
		}
	}
	sink, err := MakeElement("appsink", "rot-sink", map[string]any{"drop": true, "sync": false})
	if err != nil {
		t.Fatalf("appsink: %v", err)
	}
	if !pipe.Add(sink) {
		t.Fatal("failed to add appsink")
	}

	// Link after Add (GStreamer 1.28 breaks pre-insertion links).
	els := append([]gst.Element{src, sizeCap}, filters...)
	els = append(els, sink)
	for i := 0; i+1 < len(els); i++ {
		if !els[i].Link(els[i+1]) {
			t.Fatalf("link %s -> %s failed", els[i].GetName(), els[i+1].GetName())
		}
	}

	// Pull the first frame through the production appsink pull path.
	puller := gstpull.Attach(sink, 1000)

	// A 2 vCPU CI runner under GStreamer load has been observed to stall
	// first-frame delivery far longer than the sub-second it takes locally,
	// so each attempt allows 30 s and the (finite, re-armable) pipeline is
	// retried a second time before failing.
	var frame []byte
	for attempt := 1; attempt <= 2; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		firstFrame := make(chan []byte, 1)
		go puller.Run(ctx, func(b []byte) (int, error) {
			cp := make([]byte, len(b))
			copy(cp, b)
			select {
			case firstFrame <- cp:
			default:
			}
			return len(b), nil
		})
		if err := base.SetState(gst.StatePlaying); err != nil {
			cancel()
			t.Fatalf("pipeline failed to start: %v", err)
		}
		select {
		case frame = <-firstFrame:
			cancel()
			base.SetState(gst.StateNull)
			if attempt > 1 {
				t.Logf("first frame arrived on attempt %d", attempt)
			}
		case <-time.After(30 * time.Second):
			cancel()
			base.SetState(gst.StateNull)
			if attempt == 2 {
				t.Fatal("timed out waiting for a frame from the appsink (2 attempts)")
			}
			t.Log("first frame stalled on attempt 1; re-arming pipeline")
			continue
		}
		break
	}

	assertTransparentCorners(t, frame, wantW, wantH)
}

// assertTransparentCorners interprets a BGRA frame payload and checks that
// the four corners are fully transparent (alpha=0) while the center is opaque.
func assertTransparentCorners(t *testing.T, data []byte, w, h int) {
	t.Helper()

	// Single-plane BGRA: row stride = total size / height.
	rowStride := len(data) / h
	if rowStride < w*4 {
		t.Fatalf("frame of %d bytes: row stride %d too small for %d x 4", len(data), rowStride, w)
	}
	alpha := func(x, y int) byte {
		return data[y*rowStride+x*4+3]
	}

	corners := [][2]int{{0, 0}, {w - 1, 0}, {0, h - 1}, {w - 1, h - 1}}
	for _, c := range corners {
		if a := alpha(c[0], c[1]); a != 0 {
			t.Errorf("corner (%d,%d) alpha = %#x, want 0x0 (transparent)", c[0], c[1], a)
		}
	}
	// The center is covered by the rotated source and must stay opaque.
	if a := alpha(w/2, h/2); a == 0 {
		t.Errorf("center alpha = 0x0, want non-zero (covered, opaque)")
	}
}
