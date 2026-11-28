package compositor

import (
	"strings"
	"testing"
	"time"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/layer"
	"ctrlirl-coder/internal/scene"
)

// muxPipeline is a full layered trunk: compositor → x264 → mpegtsmux →
// appsink, the shape the shared audio tail attaches to.
const muxPipeline = "videotestsrc is-live=true pattern=black " +
	"! video/x-raw,format=I420,width=320,height=180,framerate=30/1 " +
	"! videoconvert ! video/x-raw,format=BGRA " +
	"! compositor name=compositor background=black " +
	"! videoconvert ! video/x-raw,format=I420 " +
	"! x264enc name=venc_kbps tune=zerolatency speed-preset=ultrafast bitrate=500 " +
	"! h264parse ! mpegtsmux name=mux ! appsink name=appsink"

func toneLayer(id string, z float64) scene.Layer {
	return scene.Layer{
		ID: id, Type: scene.LayerTypeVideo, Source: "testsrc://red?freq=440",
		Properties: map[string]float64{"zorder": z, "width": 160, "height": 90},
	}
}

func linkedSinkPads(el gst.Element) int {
	n := 0
	el.ForEachSinkPad(func(_ gst.Element, p gst.Pad) bool {
		if p.IsLinked() {
			n++
		}
		return true
	})
	return n
}

// TestLayerAudioSharesOneTail covers the audio side of AddLayer/RemoveLayer:
// the first audio layer builds the shared tail (adder → encoder → one mux
// pad), later layers mix into the same adder, audio really flows, and
// removing a layer takes its chain out but leaves the tail.
func TestLayerAudioSharesOneTail(t *testing.T) {
	cp, err := New(muxPipeline)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Stop() })
	ctx := ctxWithDeadline(t, 30*time.Second)
	bin := cp.pipeline.As()

	if err := cp.AddLayer(ctx, toneLayer("a1", 2)); err != nil {
		t.Fatalf("AddLayer a1: %v", err)
	}
	mixer := bin.GetByName("amixer")
	if mixer == nil || cp.mixer == nil {
		t.Fatal("first audio layer did not build the shared audio tail")
	}
	if h := cp.layers["a1"]; h.audioPad == nil || len(h.audioChain) != 3 {
		t.Fatalf("a1 audio not attached: pad=%v chain=%d elements", h.audioPad, len(h.audioChain))
	}
	// The mux carries the video and exactly one audio pad.
	if got := linkedSinkPads(cp.muxEl); got != 2 {
		t.Fatalf("mux has %d linked sink pads, want 2 (video + one audio track)", got)
	}

	if err := cp.AddLayer(ctx, toneLayer("a2", 3)); err != nil {
		t.Fatalf("AddLayer a2: %v", err)
	}
	if got := linkedSinkPads(cp.muxEl); got != 2 {
		t.Fatalf("second audio layer changed the mux to %d linked sink pads, want still 2", got)
	}
	if got := linkedSinkPads(mixer); got != 2 {
		t.Fatalf("adder has %d linked inputs, want 2", got)
	}

	if err := cp.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Audio flows through the tail: the AAC parser negotiates its output.
	parse := bin.GetByName("atailparse").GetStaticPad("src")
	deadline := time.Now().Add(15 * time.Second)
	for parse.GetCurrentCaps() == nil {
		if time.Now().After(deadline) {
			t.Fatal("no audio reached the end of the shared tail")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := cp.RemoveLayer(ctx, "a1"); err != nil {
		t.Fatalf("RemoveLayer a1: %v", err)
	}
	if bin.GetByName("a1-tone") != nil || bin.GetByName("a1-videotestsrc") != nil {
		t.Fatal("removed layer left elements in the pipeline")
	}
	if got := linkedSinkPads(mixer); got != 1 {
		t.Fatalf("adder has %d linked inputs after the removal, want 1", got)
	}
	if bin.GetByName("amixer") == nil || linkedSinkPads(cp.muxEl) != 2 {
		t.Fatal("removing a layer must leave the shared audio tail in place")
	}
}

// closeCounter is a layer that only records Close.
type closeCounter struct {
	layer.BaseLayer
	closed int
}

func (c *closeCounter) BuildChain() (*layer.Chain, error) { return nil, nil }
func (c *closeCounter) Close() error                      { c.closed++; return nil }

// preparedText is a prepared (real) text chain owned by a closeCounter.
func preparedText(t *testing.T, id string) (*preparedLayer, *closeCounter) {
	t.Helper()
	p, err := prepareLayer(textLayer(id, 0, 0))
	if err != nil {
		t.Fatalf("prepareLayer: %v", err)
	}
	cc := &closeCounter{BaseLayer: layer.NewBaseLayer(p.spec, scene.LayerTypeText)}
	p.layer = cc
	return p, cc
}

// A layer owns resources from the moment it is prepared (a browser layer:
// a Chrome process). Whatever happens to the attach, it must be closed
// exactly once unless it ends up in the pipeline.
func TestAddLayerClosesWhatItDoesNotAttach(t *testing.T) {
	cp, err := New(testPipeline)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := ctxWithDeadline(t, 15*time.Second)

	ok, okLayer := preparedText(t, "ok")
	if err := cp.addPrepared(ctx, ok); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if okLayer.closed != 0 {
		t.Fatalf("attached layer was closed %d times", okLayer.closed)
	}

	dup, dupLayer := preparedText(t, "ok")
	if err := cp.addPrepared(ctx, dup); err == nil || !strings.Contains(err.Error(), "already attached") {
		t.Fatalf("duplicate attach err = %v", err)
	}
	if dupLayer.closed != 1 {
		t.Fatalf("rejected duplicate closed %d times, want 1", dupLayer.closed)
	}

	if err := cp.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if okLayer.closed != 1 {
		t.Fatalf("Stop closed the attached layer %d times, want 1", okLayer.closed)
	}
	// After Stop the command never runs: the caller has to close the layer.
	late, lateLayer := preparedText(t, "late")
	if err := cp.addPrepared(ctx, late); err != ErrStopped {
		t.Fatalf("attach after Stop = %v, want ErrStopped", err)
	}
	if lateLayer.closed != 1 {
		t.Fatalf("layer abandoned after Stop closed %d times, want 1 (it would leak its Chrome)", lateLayer.closed)
	}
}
