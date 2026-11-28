package gstpull

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-gst/go-glib/pkg/gobject/v2"
	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
)

// BenchmarkPullSendLoop times the drain of 500 4096-byte buffers.
func BenchmarkPullSendLoop(b *testing.B) {
	benchPullLoop(b, 500, 4096)
}

// BenchmarkPullSendLoopTS times the drain of the buffers the production
// pipeline actually ends in: mpegtsmux emits one 188-byte TS packet per
// buffer (~4,400 buffers/s at 6 Mbit/s). One op drains 5000 of them — about
// 1.1 s of a 6 Mbit/s stream.
func BenchmarkPullSendLoopTS(b *testing.B) {
	benchPullLoop(b, 5000, tsPacketSize)
}

// tsPacketSize is one MPEG-TS packet, the buffer size mpegtsmux emits.
const tsPacketSize = 188

// benchPullLoop times the real cgo pull→send path — pull, copy, send
// callback, stats bookkeeping — over an appsink that already holds every
// buffer. The source runs to EOS before the clock starts: racing a live
// source would time the source and the thread wake-ups between the two, not
// the drain.
func benchPullLoop(b *testing.B, buffers, size int) {
	b.ReportAllocs()
	gstutil.EnsureInit()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// wait-on-eos=false: by default the appsink withholds EOS until the
		// application has pulled everything — here nothing pulls yet.
		pip, el := fakesrcPipeline(b, buffers, size, "wait-on-eos=false")
		p := Attach(el, 1000)
		if err := pip.SetState(gst.StatePlaying); err != nil {
			b.Fatal(err)
		}
		waitEOS(b, pip)
		b.StartTimer()
		p.Run(context.Background(), func(bs []byte) (int, error) { return len(bs), nil })
		b.StopTimer()
		if got := p.Frames(); got != uint64(buffers) {
			b.Fatalf("frames = %d, want %d", got, buffers)
		}
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
	}
}

// waitEOS blocks until the pipeline reports EOS on its bus: the source is
// done and the appsink holds everything it produced.
func waitEOS(tb testing.TB, pip *gstutil.Pipeline) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for msg := range pip.As().GetBus().Messages(ctx) {
		if msg.Type() == gst.MessageEOS {
			return
		}
	}
	tb.Fatal("pipeline never reached EOS")
}

// fakesrcPipeline builds `fakesrc ! appsink` emitting `buffers` buffers of
// `size` bytes as fast as the appsink takes them, and returns the appsink.
// appsinkProps are extra launch-syntax properties for the appsink.
func fakesrcPipeline(tb testing.TB, buffers, size int, appsinkProps ...string) (*gstutil.Pipeline, gst.Element) {
	tb.Helper()
	pip, err := gstutil.FromLaunch("fakesrc sizetype=fixed filltype=pattern sizemax=" + strconv.Itoa(size) +
		" num-buffers=" + strconv.Itoa(buffers) + " ! appsink name=appsink " + strings.Join(appsinkProps, " "))
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
