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
// fakesrc pipeline: pull sample, map, copy, unmap, unref, send callback,
// stats bookkeeping — the full per-buffer cost.
func BenchmarkPullSendLoop(b *testing.B) {
	b.ReportAllocs()
	gstutil.EnsureInit()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		launch := "fakesrc sizetype=fixed num-buffers=" +
			strconv.Itoa(benchPerBuffers) +
			" ! appsink name=appsink"
		pip, err := gstutil.FromLaunch(launch)
		if err != nil {
			b.Fatal(err)
		}
		el := pip.As().GetByName("appsink")
		if el == nil {
			pip.SetState(gst.StateNull)
			b.Fatal("no appsink element")
		}
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
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
	}
}
