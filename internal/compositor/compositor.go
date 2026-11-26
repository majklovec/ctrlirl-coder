// Package compositor owns the running GStreamer pipeline. It is the only
// code allowed to touch GStreamer state, and it serializes every mutation
// through a single command goroutine (GStreamer is not thread-safe, spec
// §7.1). The controller and scene applier talk to it through the public API;
// it implements scene.Applier and sinks.Applier.
package compositor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/go-gst/go-glib/pkg/gobject/v2"
	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
	"ctrlirl-coder/internal/layer"
	"ctrlirl-coder/internal/scene"
)

// ErrStopped is returned when a command is submitted after Stop.
var ErrStopped = errors.New("compositor: pipeline stopped")

// ErrNoCompositor is returned by layer operations when the pipeline template
// has no `compositor` element (layerless single-chain pipelines are legal).
var ErrNoCompositor = errors.New("compositor: pipeline has no compositor element")

// layerHandle is one attached layer: a requested sink pad on the compositor
// plus the elements feeding it (video, image, pango-text surfaces) and, for
// audio-producing layers (browser), the separate audio chain attached to the
// mpegtsmux.
type layerHandle struct {
	id         string
	pad        gst.Pad
	chain      []gst.Element
	outputPad  gst.Pad
	props      map[string]float64
	audioChain []gst.Element // nil if the layer carries no audio
	audioPad   gst.Pad       // the mux sink_%d pad the audio chain is linked to
	layer      layer.Layer   // the scene layer (kept for Close on teardown)
	spec       scene.Layer   // the scene descriptor (filter names for in-place updates)
	volFilter  int           // index of the volume filter in spec.Filters (-1 = none)
}

// command is one serialized unit of pipeline work.
type command struct {
	stop bool
	fn   func() error
	res  chan error
}

// CompositorPipeline owns the whole *gst.Pipeline (main pipeline), not just
// the compositor element. It is the only struct allowed to touch GStreamer
// state from outside its owning goroutine.
type CompositorPipeline struct {
	pipeline *gstutil.Pipeline

	compositor gst.Element // nil for layerless pipelines
	vencEl     gst.Element // venc_bps or venc_kbps, nil if absent
	tapEl      gst.Element // terminal `appsink` the pipeline ends in, nil if absent
	overlayEl  gst.Element // textoverlay "overlay", nil if absent
	muxEl      gst.Element // mpegtsmux "mux" (layer audio lands here), nil if absent
	mixer      gst.Element // shared audiomixer tail head (one audio track total), nil until first audio layer

	encoderName  string // "venc_bps" | "venc_kbps" | ""
	bitrateUnits int    // 1 (bps) or 1000 (kbps), from sinks.EncoderUnits

	layers map[string]*layerHandle // guarded by the command goroutine
	cmdCh  chan command            // one serialized unit of pipeline work
	doneCh chan struct{}

	busDrainStop context.CancelFunc // nil until Start; set/invoked on the
	// caller goroutine (no GStreamer state involved).
	busLog *slog.Logger // nil = slog.Default(); SetBusLog injects

	started   atomic.Bool
	stopping  atomic.Bool
	suspended atomic.Int32 // >0: a multi-op batch holds the pipeline at READY

	// Last values successfully applied, so idempotent re-apply loops
	// (the controller re-sends the same bitrate/text on each tick) skip
	// the command round trip + property write. Written on the command
	// goroutine, read from the controller goroutine — atomics.
	lastBitrate     atomic.Int64 // encoder property value in cached units (0 = never)
	lastOverlayText atomic.Pointer[string]
}

// New parses the pipeline launch string (template content) and performs the
// startup contract check (§2.1): discover the `compositor` (optional), the
// encoder `venc_bps`/`venc_kbps` (required for adaptive bitrate), the
// terminal `appsink` (required for SRT), and the optional `overlay`. Element
// names are resolvable once the pipeline has been parsed, so the check runs
// in the NULL state (§7.9).
//
// The pipeline is left in the NULL state. This matters for layered
// pipelines: a compositor with no sources linked cannot preroll, so setting
// PAUSED here would block forever. Attach layers (AddLayer) while in NULL,
// then Start, which transitions NULL → PLAYING in one step.
func New(launch string) (*CompositorPipeline, error) {
	pip, err := gstutil.FromLaunch(launch)
	if err != nil {
		return nil, fmt.Errorf("compositor: %w", err)
	}

	cp := &CompositorPipeline{
		pipeline: pip,
		layers:   make(map[string]*layerHandle),
		cmdCh:    make(chan command, 16),
		doneCh:   make(chan struct{}),
	}

	cp.checkContract()

	go cp.run()
	return cp, nil
}

// Start moves the pipeline to PLAYING and starts the bus-message drain
// (errors/warnings/chatter → the log, spec §6 Phase 6).
func (cp *CompositorPipeline) Start() error {
	// Mark the audio-tail branch head live before the pipeline plays. The
	// "live" GST_CONTEXT propagates UP the pad graph from any element that
	// carries it (sources read their downstream context to decide live
	// mode), so its scope must exclude the source side of every pad:
	//   - the shared audio tail head (liveadder "amixer") and everything
	//     downstream (audioconvert → voaacenc → aacparse → mpegtsmux →
	//     appsink): a live audio chain resyncs the per-segment hlsdemux2
	//     audio PTS discontinuity (TS CC-reset) so the mixer does not turn
	//     it into running-time dead air — the 10 s glitch fix.
	// Everything else stays NON-LIVE on purpose. A live-marked VOD source
	// (uridecodebin → hlsdemux2, soup) cannot pause and stalls forever at
	// the low-watermark re-buffer (measured: the whole-pipeline live
	// context froze the stream at the first segment boundary; even
	// live-marking just the terminal appsink put the source branch in live
	// mode because contexts flow up the pad graph). Keeping the source
	// non-live makes it VOD-pausable: the appsink's backpressure (drop=false)
	// pauses the paused-able pipeline, the 1×-paced Go drain keeps
	// consuming, and the VOD demux re-fetches the next segment just in
	// time — a finite playlist plays at real time instead of draining to
	// EOS at network speed. Must be set while the pipeline is not playing.
	makeLive := func() error {
		if cp.mixer == nil {
			return nil // no-audio scene: nothing live, source stays VOD
		}
		return gstutil.MakeLive(cp.mixer)
	}
	if err := cp.do(makeLive); err != nil {
		return err
	}
	err := cp.do(func() error {
		return cp.pipeline.SetState(gst.StatePlaying)
	})
	if err == nil {
		if cp.busDrainStop == nil {
			cp.busDrainStop = cp.startBusDrain(cp.busLog)
		}
		cp.started.Store(true)
	}
	return err
}

// SetBusLog selects the logger the bus-message drain writes to. Must be
// called before Start (the drain starts with Start). Nil restores
// slog.Default().
func (cp *CompositorPipeline) SetBusLog(log *slog.Logger) {
	cp.busLog = log
}

// Stop moves the pipeline to NULL and shuts the command goroutine down.
// Subsequent commands return ErrStopped.
func (cp *CompositorPipeline) Stop() error {
	if !cp.stopping.CompareAndSwap(false, true) {
		return nil
	}
	var stateErr error
	if cp.started.Load() {
		stateErr = cp.do(func() error {
			return cp.pipeline.SetState(gst.StateNull)
		})
	}
	if cp.busDrainStop != nil {
		cp.busDrainStop()
		cp.busDrainStop = nil
	}
	// Tear down all layers (browser layers kill their Chrome process and
	// unload their pulse null sinks; the freeze above has already stopped the
	// streaming thread).
	cp.do(func() error {
		for id, h := range cp.layers {
			cp.teardownLayer(h)
			_ = h.layer.Close()
			delete(cp.layers, id)
		}
		return nil
	})
	// Sentinel command: run() exits, closes doneCh. cmdCh is never closed
	// (avoiding send-on-closed-channel for in-flight callers).
	cp.cmdCh <- command{stop: true}
	<-cp.doneCh
	gobject.UnsafeObjectUnref(cp.pipeline.As())
	return stateErr
}

// noFreeze disables the READY freeze around layer swaps when set
// (CTRLIRL_NO_FREEZE); used to measure freeze cost vs swap cost.
var noFreeze = os.Getenv("CTRLIRL_NO_FREEZE") != ""

// SuspendStream holds the pipeline at READY for the duration of a
// multi-op batch (scene.Batcher). The first call freezes a started
// pipeline; nested calls only bump the counter. While suspended,
// RemoveLayer skips its per-op freeze (the pipeline is already READY).
// Streaming is paused for the whole batch — that is the point: one
// re-preroll on resume instead of one per removed layer.
func (cp *CompositorPipeline) SuspendStream(ctx context.Context) error {
	if noFreeze {
		return nil
	}
	return cp.doCtx(ctx, func() error {
		n := cp.suspended.Add(1)
		if n == 1 && cp.started.Load() {
			return cp.pipeline.SetState(gst.StateReady)
		}
		return nil
	})
}

// ResumeStream undoes one SuspendStream; the last release restores PLAYING.
func (cp *CompositorPipeline) ResumeStream(ctx context.Context) error {
	if noFreeze {
		return nil
	}
	return cp.doCtx(ctx, func() error {
		n := cp.suspended.Add(-1)
		if n < 0 {
			cp.suspended.Add(1)
			return errors.New("compositor: resume without suspend")
		}
		if n == 0 && cp.started.Load() {
			return cp.pipeline.SetState(gst.StatePlaying)
		}
		return nil
	})
}

// do submits a closure to the command goroutine and waits for its result.
func (cp *CompositorPipeline) do(fn func() error) error {
	return cp.doCtx(context.Background(), fn)
}

// doCtx is do with a caller context (layer operations).
//
// The doneCh is checked before enqueueing: the cmdCh is buffered, so after
// Stop a plain select would randomly win the send branch and park the
// command in the buffer, hanging the caller until its context deadline
// (16 commands could wedge before the send itself fails).
func (cp *CompositorPipeline) doCtx(ctx context.Context, fn func() error) error {
	select {
	case <-cp.doneCh:
		return ErrStopped
	default:
	}
	ch := make(chan error, 1)
	select {
	case cp.cmdCh <- command{fn: fn, res: ch}:
	case <-ctx.Done():
		return ctx.Err()
	case <-cp.doneCh:
		return ErrStopped
	}
	// If the result is already in (the command ran to completion), take it
	// before the shutdown branches can report a false failure.
	select {
	case err := <-ch:
		return err
	default:
	}
	select {
	case err := <-ch:
		return err
	case <-cp.doneCh:
		return ErrStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run is the single pipeline-owning goroutine. All GStreamer calls in this
// package execute here.
func (cp *CompositorPipeline) run() {
	defer close(cp.doneCh)
	for cmd := range cp.cmdCh {
		if cmd.stop {
			return
		}
		var err error
		if cmd.fn != nil {
			err = cmd.fn()
		}
		if cmd.res != nil {
			cmd.res <- err
		}
	}
}
