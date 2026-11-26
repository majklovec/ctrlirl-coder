package compositor

import (
	"log/slog"
	"time"

	"ctrlirl-coder/internal/gstutil"
	"ctrlirl-coder/internal/layer"

	"github.com/go-gst/go-gst/pkg/gst"
)

// hlsPrefetchHighNS / hlsPrefetchLowNS clamp the inner hlsdemux2's VOD
// download watermarks to 8 s / 50 ms.
//
// Why: GStreamer 1.28 exposes no way to put the pipeline into live mode for
// a uridecodebin-fed HLS stream (gst_pipeline "live" is a read-only property
// and no gst_pipeline_set_live() exists; the source decides, and urisourcebin
// is not a live source). The demux therefore runs VOD-style. Its default
// 30 s `high-watermark-time` lets the demux buffer 30 s of data, which
// overflows the SRT-paced send path and causes chronic appsink drops
// (14-20 %).
//
// The high watermark must be SECONDS, not milliseconds: the terminal
// appsink runs drop=false, so its backpressure pauses the WHOLE pipeline —
// including the VOD source. While paused the demux can only serve from its
// local buffer; a 400 ms buffer is exhausted in under a second and the
// next-segment fetch (a source seek + download that needs the pipeline to
// be resumable) never completes: the measured freeze at the first segment
// boundary. 8 s ≈ one 6 s TS segment + headroom, so the pipeline runs out
// of local data only after the demux has already resumed and fetched the
// next segment. The low watermark (50 ms) is the resume trigger; it stays
// small so the demux resumes promptly.
//
// The appsink `max-buffers` (set to 600 in contract.go) adds downstream
// headroom so the CC-reset burst at each HLS segment boundary is absorbed
// without dropping frames.
const (
	hlsPrefetchHighNS = int64(8_000_000_000) // 8 s
	hlsPrefetchLowNS  = int64(50_000_000)    // 50 ms
)

// tuneHlsPrefetch clamps the download watermarks of every hlsdemux2 living
// inside srcEl (a uridecodebin/uridecodebin3 for an HLS URI). The demux is
// created asynchronously during preroll, so the lookup polls for up to ~10 s.
// Sources without an hlsdemux2 (plain files, SRT) exit silently on timeout.
//
// Note: the pipeline's "live" GST_CONTEXT is deliberately NOT attached to the
// pipeline root (CompositorPipeline.Start scopes it to branch heads only) so
// this VOD source and its hlsdemux2 stay pausable — a live source never
// yields to the terminal appsink's backpressure (drop=false), which is how
// the 1×-paced drain consumes a finite playlist at real time. No per-source
// context work is needed here.
func (cp *CompositorPipeline) tuneHlsPrefetch(srcEl gst.Element) {
	if bin, ok := srcEl.(gst.Bin); ok {
		go cp.pollHlsWatermarks(bin)
	}
}

// pollHlsWatermarks retries the watermark clamp until the demux exists (the
// playlist fetch + demux construction is asynchronous) or the window closes.
// The property set must happen on a goroutine that does not block the
// command channel; element property access from outside the streaming
// threads is safe (GStreamer's property locking).
func (cp *CompositorPipeline) pollHlsWatermarks(bin gst.Bin) {
	const attempts = 40 // 40 x 250 ms = 10 s
	for i := 0; i < attempts; i++ {
		time.Sleep(250 * time.Millisecond)
		demux := findHlsDemux2(bin)
		if demux == nil {
			continue
		}
		id := demux.GetName()
		if err := gstutil.SetNumericProperty(demux, "high-watermark-time", hlsPrefetchHighNS); err != nil {
			slog.Warn("hls prefetch clamp: high watermark set failed", "element", id, "err", err)
			return
		}
		if err := gstutil.SetNumericProperty(demux, "low-watermark-time", hlsPrefetchLowNS); err != nil {
			slog.Warn("hls prefetch clamp: low watermark set failed", "element", id, "err", err)
			return
		}
		slog.Info("hls prefetch clamp: watermarks set", "element", id,
			"high_ms", hlsPrefetchHighNS/1_000_000, "low_ms", hlsPrefetchLowNS/1_000_000)
		return
	}
}

// findUrIDecodeBin returns the uridecodebin/uridecodebin3 element in a layer
// chain (the one that spawns hlsdemux2 for HLS URIs), nil when the chain has
// no such source.
func findUrIDecodeBin(chain *layer.Chain) gst.Element {
	candidates := append(append([]gst.Element{}, chain.Elements...), chain.SharedSource)
	for _, el := range candidates {
		if el == nil {
			continue
		}
		if gstutil.IsFactoryType(el, "uridecodebin3") || gstutil.IsFactoryType(el, "uridecodebin") {
			return el
		}
	}
	return nil
}

// findHlsDemux2 returns the first hlsdemux2 below the bin (recursive), nil
// when absent. The GstAdaptiveDemux2 factory name for the HLS demux is
// "hlsdemux2".
func findHlsDemux2(bin gst.Bin) gst.Element {
	return gstutil.FindDeepByFactory(bin, "hlsdemux2")
}
