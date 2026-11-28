package gstpull

import (
	"context"
	"strconv"
	"testing"

	"github.com/go-gst/go-glib/pkg/gobject/v2"
	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
)

// benchPerBuffers is the buffers per timed drain; b.N iterations re-run the
// whole pipeline so per-op costs stay proportional.
const benchPerBuffers = 500

// BenchmarkPullSendLoop drives the real cgo pull→send path over a fixed-size
// fakesrc pipeline (4096-byte buffers): the full per-buffer cost of pull,
// copy, send callback and stats bookkeeping.
func BenchmarkPullSendLoop(b *testing.B) {
	benchPullLoop(b, benchPerBuffers, 4096)
}

// BenchmarkPullSendLoopTS is the same drain over the buffers the production
// pipeline actually ends in: mpegtsmux emits one 188-byte TS packet per
// buffer (~4,400 buffers/s at 6 Mbit/s). One op drains 5000 of them — about
// 1.1 s of a 6 Mbit/s stream.
func BenchmarkPullSendLoopTS(b *testing.B) {
	benchPullLoop(b, 5000, tsPacketSize)
}

// tsPacketSize is one MPEG-TS packet, the buffer size mpegtsmux emits.
const tsPacketSize = 188

func benchPullLoop(b *testing.B, buffers, size int) {
	b.ReportAllocs()
	gstutil.EnsureInit()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		pip, el := fakesrcPipeline(b, buffers, size)
		p := Attach(el, 1000)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			p.Run(ctx, func(bs []byte) (int, error) { return len(bs), nil })
		}()
		if err := pip.SetState(gst.StatePlaying); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		<-done
		b.StopTimer()
		cancel()
		if got := p.Frames(); got != uint64(buffers) {
			b.Fatalf("frames = %d, want %d", got, buffers)
		}
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
	}
}

// fakesrcPipeline builds `fakesrc ! appsink` emitting `buffers` buffers of
// `size` bytes as fast as the appsink takes them, and returns the appsink.
func fakesrcPipeline(tb testing.TB, buffers, size int) (*gstutil.Pipeline, gst.Element) {
	tb.Helper()
	pip, err := gstutil.FromLaunch("fakesrc sizetype=fixed filltype=pattern sizemax=" + strconv.Itoa(size) +
		" num-buffers=" + strconv.Itoa(buffers) + " ! appsink name=appsink")
	if err != nil {
		tb.Fatal(err)
	}
	el := pip.As().GetByName("appsink")
	if el == nil {
		pip.SetState(gst.StateNull)
		tb.Fatal("no appsink element")
	}
	return pip, el
}
