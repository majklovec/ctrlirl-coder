// Package gstpull drains the pipeline's terminal `appsink` and hands each
// encoded buffer to a Go send callback. This is the faithful Go port of the
// original ctrlirl_coder/belacoder appsink data path: a dedicated goroutine calls
// `gst_app_sink_pull_sample` (blocking pull), maps the buffer, and sends the
// bytes over SRT — replacing the C main-loop callback `new_buf_cb`.
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
	"sync/atomic"
	"time"
	"unsafe"

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

// Send is the callback the data path calls for each pulled buffer or batch.
// Return an error (e.g. srt.ErrNotConnected) to drop the buffer and keep
// draining.
//
// b is either a view of a single mapped GstBuffer (legacy) or a batch of
// multiple TS packets accumulated into a Go slice (batched send path). In
// both cases the callee must copy the payload synchronously (srtgo's Write
// does — srt_sendmsg2 copies into the SRT send buffer inside the C call) and
// must never retain b after returning.
type Send func(b []byte) (int, error)

// Puller drives the pull→send data path on one terminal appsink element.
// The element must stay alive for the lifetime of the Puller (the pipeline
// owns it); Detach is a no-op safety seam kept for the ordered teardown.
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
	dropped    atomic.Uint64 // buffers dropped: send failure

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
// has started (the Run loop loads the pointer per buffer).
func (p *Puller) SetPaceRateSupplier(fn func() int64) {
	if fn == nil {
		p.paceRate.Store(nil)
		return
	}
	p.paceRate.Store(&fn)
}

// framerateTriesMax bounds the caps-framerate probes: a mpegts terminal
// (the normal case) reports no framerate fraction, so the probe would
// repeat on every buffer for the whole stream. Try a few buffers (caps are
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

// sinkPointer borrows the raw C sink pointer once. Run snapshots it at
// start, so the hot loop reads a local and Detach (p.el = nil) no longer
// races it.
func (p *Puller) sinkPointer() unsafe.Pointer {
	el := p.el
	if el == nil {
		return nil
	}
	return gstutil.Ptr(el)
}

// srtBatchSize is how many pulled TS bytes are accumulated before one SRT
// send. The send path (sinks/srt) splits a batch into SRT live-safe 1316-byte
// packets inside a single cgo crossing, so a large batch costs ONE send cgo
// call regardless of how many packets it holds. 32 KB ≈ 174 packets ≈ ~44 ms of
// 6 Mbps content — plenty big to make the send cgo cost negligible while the
// added buffering stays far below the SRT latency, and a dropped/dead batch
// only shaves tens of ms of content on reconnect.
const srtBatchSize = 32 * 1024

// paceSleepQuantumMS bounds how often the pacer actually sleeps: sub-millisecond
// debt is accumulated and only converted into a sleep once it reaches this
// quantum (a mpegts terminal streams ~20 kpackets/s at 6 Mbps; a sleep
// syscall per packet would be pure overhead).
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

// Run pulls buffers and sends each one via send until ctx is cancelled and
// the pipeline is frozen (a frozen appsink makes the pull return NULL).
//
// Per buffer: 2 cgo calls (pull+map, unmap+unref). The SRT send is batched:
// pulled TS packets accumulate into a srtBatchSize-byte slice and are shipped
// in a single cgo send that internally chunks to SRT 1316-byte packets. The
// drain is paced to 1× wall clock from the drained byte rate (pacer) — a
// no-op for self-pacing live sources, the real-time clamp for VOD sources.
func (p *Puller) Run(ctx context.Context, send Send) {
	sink := p.sinkPointer() // local snapshot; Detach cannot race the loop
	if sink == nil {
		return
	}
	var (
		winStart int64 = int64(nowMS())
		winBytes uint64
		frTries  int
		pace     pacer
	)
	batch := make([]byte, 0, srtBatchSize)
	for {
		select {
		case <-ctx.Done():
			if len(batch) > 0 {
				send(batch) // flush partial batch on shutdown
			}
			return
		default:
		}
		f := pullMappedFrame(sink)
		if f.handle == 0 {
			if len(batch) > 0 {
				send(batch) // flush partial batch on EOS
			}
			return // pipeline left PLAYING (frozen / EOS)
		}
		b := f.bytes()
		if f.handle < 0 || b == nil {
			f.release() // map failure / empty buffer: skip
			continue
		}

		// Liveness + stats hot path: a few relaxed atomic stores per frame.
		now := int64(nowMS())
		size := uint64(len(b))
		if rateFn := p.paceRate.Load(); rateFn != nil {
			if d := pace.tick(now, size, (*rateFn)()); d > 0 {
				// 1× wall clock at the target bitrate: VOD HLS/file sources
				// would otherwise drain at network speed and hit EOS while
				// the SRT listener is still on the first minute. Live
				// sources never produce positive drift → no sleep.
				time.Sleep(d)
				now = int64(nowMS())
			}
		}
		p.frames.Add(1)
		p.bytesTotal.Add(size)
		p.lastFrame.Store(now)
		p.lastPTS.Store(f.pts)
		winBytes += size
		if now-winStart >= 500 {
			p.throughputKB.Store(winBytes * 8 / uint64(now-winStart)) // rolling ~500 ms window
			winBytes = 0
			winStart = now
		}
		if p.frNum.Load() == 0 && frTries < framerateTriesMax {
			// Caps are negotiated by the first buffers; probe the framerate a
			// bounded number of times (a mpegts terminal reports none).
			frTries++
			num, den := framerate(p.el)
			p.frNum.Store(int32(num))
			p.frDen.Store(int32(den))
		}

		// Accumulate into the batch; flush when full.
		batch = append(batch, b...)
		if len(batch) >= srtBatchSize {
			if _, err := send(batch); err != nil {
				p.dropped.Add(1) // one dropped batch (~174 TS packets)
			} else {
				p.sent.Add(uint64(len(batch)))
			}
			batch = batch[:0]
		}
		f.release() // after copying: the mapping is no longer needed
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

// Dropped is the number of buffers dropped on a send failure (link down /
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

// LastPTS is the last pulled buffer's presentation timestamp in ns (0 if
// unset).
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

// Detach is a no-op safety seam kept for the ordered teardown in run_gst.go
// (no signal to disconnect in the pull model — the element is owned by the
// pipeline and unrefed with it).
func (p *Puller) Detach() { p.el = nil }

// nowMS is the monotonic millisecond clock (the original's getms()).
func nowMS() uint64 {
	return uint64(time.Since(startup) / time.Millisecond)
}
