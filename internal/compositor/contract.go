package compositor

import (
	"fmt"
	"log/slog"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
	"ctrlirl-coder/internal/sinks"
)

// checkContract implements the §2.1 startup validation. It runs on the freshly
// parsed pipeline, still in NULL (names resolve once parsed, §7.9). Missing
// optional elements are logged, not fatal; a missing encoder disables
// adaptive bitrate; a missing terminal `appsink` on a pipeline expected to
// stream is an error.
//
// It is called from New, before the command goroutine is handling commands;
// reads of discovered fields (encoderName, bitrateUnits, tapEl, …) are safe
// because New has completed.
func (cp *CompositorPipeline) checkContract() {
	find := cp.pipeline.As().GetByName

	cp.findEncoder(find)
	cp.findAppsink(find("appsink"))

	// Mux — the shared audio tail of the layer audio chains lands on one of
	// its sink_%d pads.
	if cp.muxEl = find("mux"); cp.muxEl != nil {
		slog.Info("pipeline: mux found", "name", "mux")
	} else {
		slog.Warn("pipeline: no element named mux; layer audio is unavailable")
	}

	if cp.compositor = find("compositor"); cp.compositor != nil {
		slog.Info("pipeline: compositor found", "name", "compositor")
	} else {
		slog.Warn("pipeline: no compositor element; scene layers are unavailable")
	}

	if cp.overlayEl = find("overlay"); cp.overlayEl != nil {
		if gstutil.IsFactoryType(cp.overlayEl, "textoverlay") {
			slog.Info("pipeline: textoverlay found", "name", "overlay")
		} else {
			slog.Warn("pipeline: overlay element found but is not a textoverlay; overlay updates skipped")
		}
	}

	// Informational elements — presence is logged, no action taken.
	for _, info := range [][2]string{
		{"v_delay", "pipeline: v_delay present (A/V sync handle)"},
		{"a_delay", "pipeline: a_delay present (A/V sync handle)"},
		{"ptsfixup", "pipeline: ptsfixup present (OBS-compat, no action needed)"},
	} {
		if find(info[0]) != nil {
			slog.Info(info[1])
		}
	}
}

// findEncoder locates the encoder — exactly one of venc_bps / venc_kbps; the
// name's suffix declares the units of its bitrate property.
func (cp *CompositorPipeline) findEncoder(find func(string) gst.Element) {
	for _, name := range []string{"venc_bps", "venc_kbps"} {
		if el := find(name); el != nil {
			cp.vencEl, cp.encoderName = el, name
			cp.bitrateUnits = sinks.EncoderUnits(name)
			slog.Info("pipeline: encoder found", "name", name, "units", cp.bitrateUnits)
			return
		}
	}
	slog.Error("pipeline: no encoder named venc_bps or venc_kbps; adaptive bitrate disabled")
}

// findAppsink adopts the terminal appsink — the pipeline's terminal element,
// mandatory for SRT output (the original ctrlirl_coder/belacoder contract: an
// element named `appsink`, type appsink). Go pulls it with
// gst_app_sink_pull_sample and sends the bytes over the Go-owned SRT socket
// (see internal/gstpull); SRT is NOT sent in-pipeline.
func (cp *CompositorPipeline) findAppsink(el gst.Element) {
	switch {
	case el == nil:
		slog.Error("pipeline: no element named appsink (terminal sink); this pipeline cannot stream over SRT")
		return
	case !gstutil.IsFactoryType(el, "appsink"):
		slog.Error("pipeline: element named appsink is not an appsink; this pipeline cannot stream over SRT")
		return
	}
	cp.tapEl = el
	// drop=false: the streaming thread MAY block on a slow drain — that
	// backpressure is what makes VOD sources (HLS EXT-X-ENDLIST, local file)
	// play at 1× real time instead of draining at network speed and hitting
	// EOS in seconds. The 1×-paced Go drain (pacer) is the consumer; when it
	// keeps up the appsink never fills. A live source that fills it (send
	// stall) blocks the pipeline — a correct live behaviour, not a drop. The
	// earlier seg1→seg2 deadlock with drop=false came from the "live"
	// GST_CONTEXT reaching the VOD hlsdemux2/soup source (whole-pipeline
	// live, and — because contexts flow up the pad graph — live-marking even
	// just the terminal appsink), which made the source unpauseable (a live
	// source never yields to backpressure). The live context is now scoped
	// to the audio-tail branch head only (Start), so the source branch stays
	// VOD-pausable and backpressure works.
	el.SetObjectProperty("drop", false)
	// One buffer is one 188-byte TS packet, so 600 buffers ≈ 113 kB ≈ 150 ms
	// at 6 Mbit/s. The default (200, ~50 ms) fills during the CC-reset burst
	// at every HLS segment boundary (~10 s) and stalls the streaming thread;
	// 600 gives the burst headroom.
	if err := gstutil.SetNumericProperty(el, "max-buffers", int64(600)); err != nil {
		slog.Warn("pipeline: appsink max-buffers set failed", "err", err)
	}
	slog.Info("pipeline: terminal appsink found", "name", "appsink", "drop", false, "max-buffers", 600)
}

// HasCompositor reports whether the pipeline template carries a compositor
// element (layered multi-source operation).
func (cp *CompositorPipeline) HasCompositor() bool { return cp.compositor != nil }

// HasEncoder reports whether adaptive bitrate is available.
func (cp *CompositorPipeline) HasEncoder() bool { return cp.vencEl != nil }

// HasTap reports whether the terminal appsink element was found.
func (cp *CompositorPipeline) HasTap() bool { return cp.tapEl != nil }

// TapElement returns the terminal appsink element (the `appsink` the pipeline
// ends in) so the run path can attach the puller. Nil if the tap is absent.
func (cp *CompositorPipeline) TapElement() gst.Element { return cp.tapEl }

// SetBitrate sets the encoder's bitrate property in the cached units
// (bps for venc_bps, kbps for venc_kbps — spec §7.4). It is a command, so it
// is safe to call from the controller goroutine.
//
// The write goes through the cgo g_object_setv path
// (gstutil.SetNumericProperty), which types the GValue after the property's
// GParamSpec (x264enc's "bitrate" is a guint in kbit/s): the varargs
// g_object_set used by the Go binding silently drops int GValues in this
// GStreamer build, so the plain property setter would be a no-op.
func (cp *CompositorPipeline) SetBitrate(bps int) error {
	if cp.vencEl == nil {
		return fmt.Errorf("compositor: no encoder (bitrate change ignored)")
	}
	value := int64(bps / cp.bitrateUnits)
	// The controller re-applies the target every tick; when it has not
	// changed, skip the command round trip + g_object_setv entirely.
	if cur := cp.lastBitrate.Load(); cur != 0 && cur == value {
		return nil
	}
	if err := cp.do(func() error {
		if err := gstutil.SetNumericProperty(cp.vencEl, "bitrate", value); err != nil {
			return err
		}
		slog.Debug("encoder: bitrate set", "bps", bps, "property", value)
		return nil
	}); err != nil {
		return err
	}
	cp.lastBitrate.Store(value)
	return nil
}

// SetOverlayText updates the `overlay` textoverlay's text property. No-op
// (with a one-shot warning) if the pipeline has no textoverlay. Unchanged
// text skips the command round trip.
func (cp *CompositorPipeline) SetOverlayText(text string) {
	if cp.overlayEl == nil {
		return
	}
	if last := cp.lastOverlayText.Load(); last != nil && *last == text {
		return
	}
	if cp.do(func() error {
		cp.overlayEl.SetObjectProperty("text", text)
		return nil
	}) != nil {
		return
	}
	stored := text
	cp.lastOverlayText.Store(&stored)
}
