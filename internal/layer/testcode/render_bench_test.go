package testcode

import (
	"testing"
	"time"
)

// BenchmarkRender benchmarks one 1280x720 testcode frame: frame buffer,
// label/timestamp text, blink square and the QR code.
func BenchmarkRender(b *testing.B) {
	r := &testcodeRenderer{w: 1280, h: 720, fps: 30}
	now := time.Date(2026, 1, 2, 3, 4, 5, 250_000_000, time.UTC)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if frame := r.render(now, uint64(i)); len(frame) != r.w*r.h*4 {
			b.Fatalf("frame length = %d", len(frame))
		}
	}
}
