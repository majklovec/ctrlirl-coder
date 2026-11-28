// Package gstpull drains the pipeline's terminal `appsink` and hands the
// encoded bytes to a Go send callback. This is the Go port of the original
// ctrlirl_coder/belacoder appsink data path: a dedicated goroutine pulls with
// `gst_app_sink_pull_sample` and sends the bytes over SRT — replacing the C
// main-loop callback `new_buf_cb`. The terminal mpegtsmux emits one 188-byte
// TS packet per buffer (thousands per second), so the drain collects a whole
// batch of them per cgo call (see Run).
//
// Why the pull model: `gst_app_sink_pull_sample` is an application-thread API
// designed to be called from the app, so all of it runs on an ordinary Go
// goroutine — no cgo callback ever executes on GStreamer's streaming
// pthread. The pull loop is additionally paced to 1× wall clock at the
// adaptive target bitrate (pacer) so VOD sources (HLS EXT-X-ENDLIST, local
// file) are consumed at real time instead of draining at network speed and
// hitting EOS. The terminal appsink runs with drop=false, so the 1×-paced
// drain backpressures the streaming thread and throttles the VOD source to
// real time (the source is VOD-pausable: the "live" GST_CONTEXT is scoped
// to the audio-tail branch head (liveadder), never the source branch — a
// live demux would deadlock on that backpressure at the segment boundary).
// Live sources are self-pacing, so the pacer is a no-op for them.
//
// The puller is also the appsink *stats* surface consumed by the adaptive
// bitrate controller: pulled frame count, pulled-bytes throughput, last
// frame age, and frame-production liveness (stall detection, the original
// `frame_liveness` module) plus caps framerate and last buffer PTS.
package gstpull

import (
	"context"
	"runtime"
	"sync/atomic"
	"time"

	"ctrlirl-coder/internal/gstutil"

	"github.com/go-gst/go-gst/pkg/gst"
)

// startup is the monotonic base for the millisecond clock (the original's
// getms() seeds frame_liveness from "now" at init; anchoring to process start
// keeps the arithmetic overflow-free on a long-lived encoder).
var startup = time.Now()

// DefaultStallMS is the frame-production stall threshold: no pulled frame for
// this long means the pipeline is zombie-encoding (capture stall, encoder
// hang). Mirrors FRAME_LIVENESS_DEFAULT_STALL_MS of the original. 3 s
// comfortably exceeds one frame interval at any sane framerate yet reacts
// well inside a typical supervisor window.
const DefaultStallMS = 3000

// Send is the callback the data path calls for each drained batch: whole
// buffers, in order (a run of TS packets; a buffer larger than the batch
// size arrives alone). Return an error (e.g. srt.ErrNotConnected) to drop
// the batch and keep draining.
//
// b is reused for the next batch: the callee must consume it synchronously
// (the SRT send does — srt_sendmsg2 copies into the SRT send buffer inside
// the C call) and must never retain it after returning.
type Send func(b []byte) (int, error)

// Puller drives the pull→send data path on one terminal appsink element.
// The pipeline owns the element; the Puller holds a reference until Detach.
type Puller struct {
	el gst.Element

	// Frame-production liveness (the original frame_liveness state): the
	// last-frame baseline is seeded at Attach so the interval from startup
	// to the first frame is bounded by the same threshold as a mid-stream
	// stall.
	lastFrame atomic.Int64 // monotonic ms of the last pulled frame (0 = none)
	stallMS   uint64

	frames     atomic.Uint64 // cumulative pulled buffers
	bytesTotal atomic.Uint64 // cumulative pulled bytes
	sent       atomic.Uint64 // cumulative successfully-sent bytes
	dropped    atomic.Uint64 // batches dropped: send failure

	// Throughput: the window state (winStartMS/winBytes) is plain state
	// owned by the Run goroutine; only the published rate is atomic.
	throughputKB atomic.Uint64 // kbit/s of pulled (encoded) bytes

	frNum   atomic.Int32 // caps framerate numerator (0 = unknown)
	frDen   atomic.Int32
	lastPTS atomic.Uint64 // last buffer PTS (ns, 0 if unset)

	// paceRate supplies the VOD drain pace in bits/s — a closure over the
	// adaptive bitrate controller's current target. Nil → no pacing (a live
	// source needs none). Kept as a supplier (not a value) because the
	// balancer changes the target continuously, and as an atomic pointer
	// because the supplier is wired in AFTER Run has started (the controller
	// outlives data-path start).
	paceRate atomic.Pointer[func() int64]
}

// SetPaceRateSupplier wires the drain pacer to a live target-bitrate source
// (bit/s). Pass nil to disable pacing. Thread-safe; may be called after Run
// has started (the Run loop loads the pointer per batch).
func (p *Puller) SetPaceRateSupplier(fn func() int64) {
	if fn == nil {
		p.paceRate.Store(nil)
		return
	}
	p.paceRate.Store(&fn)
}

// framerateTriesMax bounds the caps-framerate probes: a mpegts terminal
// (the normal case) reports no framerate fraction, so the probe would
// repeat on every batch for the whole stream. Try a few batches (caps are
// negotiated early), then give up.
const framerateTriesMax = 10

// Attach prepares the puller on the terminal appsink element. Call before the
// pipeline is set to PLAYING. stallMS selects the liveness threshold (0 →
// DefaultStallMS).
func Attach(el gst.Element, stallMS uint64) *Puller {
	if stallMS == 0 {
		stallMS = DefaultStallMS
	}
	p := &Puller{el: el, stallMS: stallMS}
	p.lastFrame.Store(int64(nowMS())) // ADR-0005: bound the time-to-first-frame too
	return p
}

// srtBatchSize is the most payload one send carries: 25 full SRT live
// packets (1316 bytes = 7 TS packets each) ≈ 44 ms of a 6 Mbit/s stream. The
// send path (sinks/srt) splits a batch into 1316-byte packets inside a single
// cgo crossing, so a full batch costs one send call however many packets it
// holds, and a batch lost to a dead link only costs tens of ms of content.
const srtBatchSize = 25 * 1316

// A batch is shipped as soon as the appsink runs dry, not only when it is
// full: holding bytes back until 32 kB have accumulated delayed a 500 kbit/s
// stream by half a second (and stamped half a second of content with one SRT
// send time).
//
// drainLinger is how long the appsink must stay empty before the batch goes
// out. The muxer hands over each video frame as a quick burst of 188-byte
// buffers; the linger keeps the drain from outrunning that burst and
// shipping it as a stream of undersized SRT packets.
//
// drainMaxHold bounds how long the first buffer of a batch can wait when
// buffers keep trickling in faster than the linger.
const (
	drainLinger  = 2 * time.Millisecond
	drainMaxHold = 20 * time.Millisecond
)

// paceSleepQuantumMS bounds how often the pacer actually sleeps: sub-millisecond
// debt is accumulated and only converted into a sleep once it reaches this
// quantum, so a run of small batches does not turn into a run of tiny
// sleeps.
const paceSleepQuantumMS int64 = 1

// paceReanchorMS re-anchors the pacer when the drain falls this far behind
// wall clock (e.g. after a long SRT send stall): without the re-anchor the
// next tick would demand a huge catch-up sleep and freeze the stream.
const paceReanchorMS int64 = 500

// pacer paces the appsink drain at the encoder's target byte rate so a VOD
// source plays at 1× wall clock.
//
// Why byte-rate, not PTS: the pipeline's terminal appsink sits downstream of
// an mpegtsmux, whose outgoing buffers carry NO presentation timestamp (the
// PTS lives inside the TS payload) — a PTS-based pace can't work here. Bytes
// are what the terminal actually emits, and its rate is the encoder's target
// bitrate. Pacing the drain by bytes (ffmpeg -re for VOD) makes a VOD source
// (HLS playlist with EXT-X-ENDLIST, local file) play at real time: hlsdemux2
// has no live mode in GStreamer 1.28, so without pacing it runs at network
// speed and the un-paced drain consumes a finite playlist in seconds
// (measured: a ~30-min 60-fps HLS master playlist drained in ~4 min). The
// SRT listener then sees a stream already at EOS — OBS plays the first few
// seconds and "stops". The 1×-paced drain therefore consumes the finite
// playlist at real time: with the terminal appsink at drop=false the slow
// drain backpressures the streaming thread, and the VOD-pausable source
// (the "live" context is scoped to the audio-tail branch, not the source
// branch — a live demux deadlocks on that backpressure) yields to it and
// fetches each next segment just in time, until the playlist is exhausted.
// For self-pacing live sources (SRT, camera) the drain never outruns the
// source, so drift stays ≤0 and the pacer is a no-op.
type pacer struct {
	anchored   bool
	anchorWall int64  // monotonic ms at anchor
	anchorCum  uint64 // cumulative bytes drained at anchor
	cum        uint64 // running cumulative bytes drained
	debtMS     int64  // accumulated sub-quantum sleep debt
}

// tick returns how long the drain should sleep, having just drained n bytes
// at monotonic time now (ms), to keep up to rateBps (bits/s). A non-positive
// rate disables pacing (no target yet / live source).
func (pc *pacer) tick(now int64, n uint64, rateBps int64) time.Duration {
	if rateBps <= 0 || n == 0 {
		return 0
	}
	pc.cum += n
	if !pc.anchored {
		pc.anchored, pc.anchorWall = true, now
		pc.anchorCum, pc.debtMS = pc.cum, 0
		return 0
	}
	sentSince := pc.cum - pc.anchorCum
	if sentSince == 0 {
		return 0
	}
	// Wall time legitimately needed to send sentSince bytes at rateBps:
	// bytes * 8 (bit) * 1000 (ms) / bits-per-second.
	due := float64(pc.anchorWall) + float64(sentSince*8000)/float64(rateBps)
	driftMS := int64(due) - now
	switch {
	case driftMS < -paceReanchorMS:
		// Fell well behind wall (a long SRT send stall): re-anchor so the
		// next tick does not demand a huge catch-up sleep and freeze the
		// stream — catch up without sleeping.
		pc.anchorWall, pc.anchorCum, pc.debtMS = now, pc.cum, 0
	case driftMS > 0:
		// Ahead of wall: sleep to hold 1×.
		pc.debtMS += driftMS
		if pc.debtMS >= paceSleepQuantumMS {
			d := time.Duration(pc.debtMS) * time.Millisecond
			pc.debtMS = 0
			return d
		}
	}
	return 0
}

// Run drains the appsink into send until the pipeline reaches EOS or NULL,
// or ctx is cancelled (checked between batches; a drain blocked on an idle
// appsink returns once the pipeline is stopped).
//
// One cgo call per batch: the C drain blocks for the first buffer, then
// collects what follows (see drainLinger / drainMaxHold) into a reused Go
// buffer — no per-buffer crossing, no allocation. The drain is paced to 1×
// wall clock from the drained byte rate (pacer) — a no-op for self-pacing
// live sources, the real-time clamp for VOD sources.
func (p *Puller) Run(ctx context.Context, send Send) {
	el := p.el
	if el == nil {
		return
	}
	// The loop works on the element's raw pointer: keep the Go reference
	// (and with it the element) alive until the loop is done, whatever
	// Detach does meanwhile.
	defer runtime.KeepAlive(el)
	d := newDrainer(gstutil.Ptr(el))
	defer d.close()

	var (
		buf      = make([]byte, srtBatchSize)
		winStart = int64(nowMS())
		winBytes uint64
		frTries  int
		pace     pacer
	)
	for ctx.Err() == nil {
		b := d.drain(buf, drainLinger, drainMaxHold)
		if b.eos {
			return // EOS, or the pipeline was stopped
		}
		if b.need > 0 {
			// One buffer larger than the batch (a raw-video terminal):
			// make room for it; it is still parked in the drainer.
			buf = make([]byte, b.need)
			continue
		}

		now := int64(nowMS())
		size := uint64(b.bytes)
		if rateFn := p.paceRate.Load(); rateFn != nil {
			if sleep := pace.tick(now, size, (*rateFn)()); sleep > 0 {
				// 1× wall clock at the target bitrate: VOD HLS/file sources
				// would otherwise drain at network speed and hit EOS while
				// the SRT listener is still on the first minute. Live
				// sources never produce positive drift → no sleep.
				time.Sleep(sleep)
				now = int64(nowMS())
			}
		}

		// Liveness + stats: a few relaxed atomic stores per batch.
		p.frames.Add(uint64(b.buffers))
		p.bytesTotal.Add(size)
		p.lastFrame.Store(now)
		if b.hasPTS {
			p.lastPTS.Store(b.pts)
		}
		winBytes += size
		if now-winStart >= 500 {
			p.throughputKB.Store(winBytes * 8 / uint64(now-winStart)) // rolling ~500 ms window
			winBytes = 0
			winStart = now
		}
		if p.frNum.Load() == 0 && frTries < framerateTriesMax {
			frTries++
			num, den := framerate(el)
			p.frNum.Store(int32(num))
			p.frDen.Store(int32(den))
		}

		if _, err := send(buf[:b.bytes]); err != nil {
			p.dropped.Add(1)
		} else {
			p.sent.Add(size)
		}
	}
}

// Stalled reports whether frame production has stopped for >= the threshold
// (the original frame_liveness_advancing_at, inverted). A puller that has
// never produced a frame is stalled after the grace window — the startup
// baseline (Attach) is what bounds that.
func (p *Puller) Stalled() bool {
	last := p.lastFrame.Load()
	if last == 0 {
		return false
	}
	now := int64(nowMS())
	return now-last >= int64(p.stallMS)
}

// Frames is the cumulative number of buffers pulled from the appsink.
func (p *Puller) Frames() uint64 { return p.frames.Load() }

// BytesPulled is the cumulative number of pulled bytes.
func (p *Puller) BytesPulled() uint64 { return p.bytesTotal.Load() }

// BytesSent is the cumulative number of bytes handed to a successful send.
func (p *Puller) BytesSent() uint64 { return p.sent.Load() }

// Dropped is the number of batches dropped on a send failure (link down /
// send budget exhausted).
func (p *Puller) Dropped() uint64 { return p.dropped.Load() }

// ThroughputKbps is the recent pulled (encoded output) rate in kbit/s.
func (p *Puller) ThroughputKbps() uint64 { return p.throughputKB.Load() }

// Framerate returns the appsink caps framerate as frames/sec. Returns 0 when
// the caps carry no framerate fraction (e.g. a multiplexed video/mpegts
// terminal — the original read the framerate pre-mux; a raw h264 terminal
// reports it).
func (p *Puller) Framerate() float64 {
	num := int(p.frNum.Load())
	den := int(p.frDen.Load())
	if num == 0 || den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// LastPTS is the presentation timestamp (ns) of the last pulled buffer that
// carried one; 0 if none did (mpegtsmux output is untimed).
func (p *Puller) LastPTS() uint64 { return p.lastPTS.Load() }

// LastFrameAge is the time since the last pulled frame (0 if none yet). The
// startup baseline makes a never-producing pipeline report the full grace
// window as its age.
func (p *Puller) LastFrameAge() time.Duration {
	last := p.lastFrame.Load()
	if last == 0 {
		return 0
	}
	return time.Duration(int64(nowMS())-last) * time.Millisecond
}

// Detach drops the Puller's reference to the appsink. Call it after the
// pipeline has been stopped (the ordered teardown in run_gst.go); a Run that
// is still finishing keeps its own reference.
func (p *Puller) Detach() { p.el = nil }

// nowMS is the monotonic millisecond clock (the original's getms()).
func nowMS() uint64 {
	return uint64(time.Since(startup) / time.Millisecond)
}
