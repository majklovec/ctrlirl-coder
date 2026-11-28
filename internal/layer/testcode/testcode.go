// TestCode layer: a live video source that renders a human-readable
// timestamp, a frame counter, a 1 Hz blink square, and a QR code encoding
// the same data into every frame. Streams it into the compositor through a
// Go-fed appsrc so tests (and a camera pointed at the stream) can verify
// what is on air and when.
package testcode

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
	"ctrlirl-coder/internal/layer"
	"ctrlirl-coder/internal/scene"
)

// TestCodeSource is a testcode layer: appsrc fed by a render goroutine.
//
// Chain: appsrc → videoconvert → videoscale → capsfilter(BGRA) → queue.
// The appsrc is live (its own clock frames the render loop), so the push
// goroutine feeds it at the configured frame rate; frames rejected while
// the pipeline is idle or flushing are dropped.
type TestCodeSource struct {
	layer.BaseLayer

	renderer *testcodeRenderer
	src      gst.Element

	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// NewTestCodeSource builds a testcode layer from its scene descriptor.
func NewTestCodeSource(l scene.Layer) *TestCodeSource {
	return &TestCodeSource{
		BaseLayer: layer.NewBaseLayer(l, scene.LayerTypeTestCode),
		renderer:  newTestcodeRenderer(l),
		stop:      make(chan struct{}),
	}
}

// BuildChain creates the appsrc, starts the render/push goroutine, and
// returns the chain (unlinked; see Layer.BuildChain).
func (t *TestCodeSource) BuildChain() (*layer.Chain, error) {
	r := t.renderer
	head, err := layer.Build(layer.Spec{
		Factory: "appsrc", Name: t.L.ID + "-src",
		Props: map[string]any{
			"is-live":      true,
			"do-timestamp": true,
			"format":       gst.FormatTime,
			"max-bytes":    16 << 20,
			"block":        false,
			"emit-signals": false,
			"duration":     r.frameDuration().Nanoseconds(),
		},
		Caps: fmt.Sprintf("video/x-raw,format=BGRA,width=%d,height=%d,framerate=%d/1", r.w, r.h, r.fps),
	})
	if err != nil {
		return nil, err
	}

	slog.Info("layer: testcode source",
		"id", t.L.ID, "width", r.w, "height", r.h, "fps", r.fps)

	chain, err := layer.SurfaceChain(t.L, head...)
	if err != nil {
		return nil, err
	}

	t.src = head[0]
	t.wg.Add(1)
	go t.pushLoop()

	return chain, nil
}

// pushLoop renders and pushes one frame per tick until Close stops it.
func (t *TestCodeSource) pushLoop() {
	defer t.wg.Done()
	tick := time.NewTicker(t.renderer.frameDuration())
	defer tick.Stop()

	var frame uint64
	// Render the first frame immediately (do not wait a tick).
	t.pushFrame(&frame)
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			t.pushFrame(&frame)
		}
	}
}

// pushFrame renders one frame and pushes it. Non-OK flow results (pipeline
// idle, flushing, EOS) drop the frame — expected outside play state.
func (t *TestCodeSource) pushFrame(frame *uint64) {
	*frame++
	data := t.renderer.render(time.Now(), *frame)
	switch flow := gstutil.PushBytes(t.src, data); flow {
	case gst.FlowOK, gst.FlowFlushing:
		// Flushing (pause/stop) is a normal transient — drop silently.
	default:
		slog.Debug("layer: testcode frame dropped", "id", t.L.ID, "flow", flow)
	}
}

// Close stops the push goroutine. Safe to call multiple times.
func (t *TestCodeSource) Close() error {
	t.once.Do(func() {
		close(t.stop)
	})
	t.wg.Wait()
	return nil
}
