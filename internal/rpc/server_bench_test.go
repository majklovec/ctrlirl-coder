package rpc

import (
	"context"
	"log/slog"
	"testing"

	"ctrlirl-coder/internal/scene"
)

// benchScene8 mirrors the scene-bench descriptor: 8 layers, so the
// marshal+parse round trip and diff cost the same as in a real trunk.
const benchScene8 = `{"name":"main","canvas":{"width":1920,"height":1080},"layers":[
  {"id":"camera","type":"video","source":"v4l2 device=/dev/video0",
   "properties":{"zorder":1,"xpos":0,"ypos":0,"width":1920,"height":1080}},
  {"id":"alert","type":"browser","source":"http://alerts",
   "properties":{"zorder":2,"xpos":32,"ypos":32,"width":480,"height":320}},
  {"id":"title","type":"text","text":"Live",
   "properties":{"zorder":3,"xpos":0,"ypos":1010,"width":1920,"height":70}},
  {"id":"pip","type":"video","source":"v4l2 device=/dev/video1",
   "properties":{"zorder":4,"xpos":1520,"ypos":760,"width":360,"height":240}},
  {"id":"logo","type":"image","source":"file:///opt/logo.png",
   "properties":{"zorder":5,"xpos":1680,"ypos":0,"width":200,"height":100}},
  {"id":"ticker","type":"text","text":"...",
   "properties":{"zorder":6,"xpos":0,"ypos":0,"width":1920,"height":40}},
  {"id":"cam2","type":"video","source":"v4l2 device=/dev/video2",
   "properties":{"zorder":7,"xpos":960,"ypos":0,"width":960,"height":540}},
  {"id":"cam3","type":"video","source":"v4l2 device=/dev/video3",
   "properties":{"zorder":8,"xpos":0,"ypos":540,"width":960,"height":540}}
]}`

type fataler interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

func benchScene(t fataler) *scene.Scene {
	t.Helper()
	s, err := scene.Parse([]byte(benchScene8))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// BenchmarkModifyRoundTrip benchmarks the full layer.set path: scene clone,
// apply the tx, re-validate, diff and (no-op) apply.
func BenchmarkModifyRoundTrip(b *testing.B) {
	b.ReportAllocs()
	// Discard logger: per-op info logs would drown the bench output.
	ctl := New(&recApplier{}, nil, slog.New(slog.DiscardHandler))
	if _, err := ctl.Load(context.Background(), "main", benchScene(b)); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		_, _, err := ctl.modify(context.Background(), "layer.set alert", func(s *scene.Scene) error {
			return nil
		})
		_ = err
	}
}
