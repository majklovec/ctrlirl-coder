package browser

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-gst/go-glib/pkg/gobject/v2"
	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
)

// benchFrame is a 300 KB stand-in for a 1280x720 browser screenshot frame.
var benchFrame = make([]byte, 300*1024)

// BenchmarkCDPFrameDecode benchmarks the frame payload path of a
// Page.captureScreenshot response: websocket-frame bytes → JSON envelope
// (map decode — a json.RawMessage result field benchmarks ~2x slower) →
// base64 decode of the "data" field.
func BenchmarkCDPFrameDecode(b *testing.B) {
	b.ReportAllocs()
	b64 := base64.StdEncoding.EncodeToString(benchFrame)
	env, err := json.Marshal(map[string]any{"id": 1, "result": map[string]any{"data": b64}})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		var r cdpResponse
		if err := json.Unmarshal(env, &r); err != nil {
			b.Fatal(err)
		}
		s, ok := r.Result["data"].(string)
		if !ok {
			b.Fatal("no data in result")
		}
		d, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			b.Fatal(err)
		}
		if len(d) != len(benchFrame) {
			b.Fatal("frame size mismatch")
		}
	}
}

// BenchmarkPushBuffer benchmarks the appsrc push path: GstBuffer allocate +
// memcpy + thread-safe push in one C call.
func BenchmarkPushBuffer(b *testing.B) {
	b.ReportAllocs()
	gstutil.EnsureInit()
	pip, err := gstutil.FromLaunch("appsrc name=src is-live=true max-buffers=10 ! fakesink")
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
	}()
	el := pip.As().GetByName("src")
	if el == nil {
		b.Fatal("no appsrc element")
	}
	if err := pip.SetState(gst.StatePlaying); err != nil {
		b.Fatal(err)
	}
	// Pushing before the streaming thread has created the appsrc (the
	// element's own preroll) aborts inside GStreamer — so wait for the
	// appsrc element itself to reach PLAYING.
	deadline := time.Now().Add(10 * time.Second)
	for el.GetCurrentState() != gst.StatePlaying {
		if time.Now().After(deadline) {
			b.Fatal("appsrc never reached PLAYING")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < b.N; i++ {
		if ret := pushBuffer(el, benchFrame); ret != 1 {
			b.Fatalf("push ret = %d, want 1", ret)
		}
	}
}
