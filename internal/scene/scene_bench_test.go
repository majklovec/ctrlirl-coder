package scene

import (
	"encoding/json"
	"testing"
)

// benchScene is a realistic descriptor: 8 layers of mixed types with
// properties and filters, like a donation-overlay trunk.
const benchSceneJSON = `{
  "name": "main",
  "canvas": {"width": 1920, "height": 1080},
  "layers": [
    {"id": "camera", "type": "video", "source": "v4l2src device=/dev/video0",
     "properties": {"zorder": 1, "xpos": 0, "ypos": 0, "width": 1920, "height": 1080, "alpha": 1.0},
     "filters": [{"kind": "videocrop", "params": {"top": 16, "bottom": 16}}]},
    {"id": "alert", "type": "browser", "source": "http://alerts.local",
     "properties": {"zorder": 2, "xpos": 32, "ypos": 32, "width": 480, "height": 320, "alpha": 1.0}},
    {"id": "title", "type": "text", "text": "Live",
     "properties": {"zorder": 3, "xpos": 0, "ypos": 1010, "width": 1920, "height": 70},
     "text-options": {"font-desc": "Sans 24", "color": "#ffffff"}},
    {"id": "pip", "type": "video", "source": "v4l2src device=/dev/video1",
     "properties": {"zorder": 4, "xpos": 1520, "ypos": 760, "width": 360, "height": 240}},
    {"id": "logo", "type": "image", "source": "file:///opt/logo.png",
     "properties": {"zorder": 5, "xpos": 1680, "ypos": 0, "width": 200, "height": 100, "alpha": 0.85}},
    {"id": "ticker", "type": "text", "text": "...",
     "properties": {"zorder": 6, "xpos": 0, "ypos": 0, "width": 1920, "height": 40}},
    {"id": "cam2", "type": "video", "source": "v4l2src device=/dev/video2",
     "properties": {"zorder": 7, "xpos": 960, "ypos": 0, "width": 960, "height": 540}},
    {"id": "cam3", "type": "video", "source": "v4l2src device=/dev/video3",
     "properties": {"zorder": 8, "xpos": 0, "ypos": 540, "width": 960, "height": 540}}
  ]
}`

type fataler interface {
	Helper()
	Fatalf(format string, args ...any)
}

func parseBenchScene(t fataler) *Scene {
	t.Helper()
	s, err := Parse([]byte(benchSceneJSON))
	if err != nil {
		t.Fatalf("parse bench scene: %v", err)
	}
	return s
}

// BenchmarkSceneClone benchmarks the clone path the controller uses today:
// JSON marshal + full Parse (validate included).
func BenchmarkSceneClone(b *testing.B) {
	b.ReportAllocs()
	s := parseBenchScene(b)
	var out *Scene
	for i := 0; i < b.N; i++ {
		raw, err := json.Marshal(s)
		if err != nil {
			b.Fatal(err)
		}
		out, err = Parse(raw)
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = out
}

// BenchmarkSceneDiff benchmarks a no-op diff (every layer identical) — the
// common SIGHUP case where only the timestamp changed.
func BenchmarkSceneDiffNoop(b *testing.B) {
	b.ReportAllocs()
	from := parseBenchScene(b)
	raw, _ := json.Marshal(from)
	to, _ := Parse(raw)
	_ = to
	ops := []Op(nil)
	for i := 0; i < b.N; i++ {
		ops = Diff(from, to)
	}
	_ = ops
}

// BenchmarkSceneDiffOnePropChange benchmarks the diff when a single
// compositor-pad property changes (the layer.set RPC case).
func BenchmarkSceneDiffOnePropChange(b *testing.B) {
	b.ReportAllocs()
	from := parseBenchScene(b)
	to := cloneOf(from)
	to.Layers[1].Properties["xpos"] = 48
	for i := 0; i < b.N; i++ {
		_ = Diff(from, to)
	}
}

// cloneOf deep-copies a scene the way the running code does it today
// (marshal + parse) so benchmarks measure the real cost.
func cloneOf(s *Scene) *Scene {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	c, err := Parse(raw)
	if err != nil {
		panic(err)
	}
	return c
}
