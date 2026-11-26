package compositor

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
	"ctrlirl-coder/internal/layer"
	"ctrlirl-coder/internal/layer/browser"
	"ctrlirl-coder/internal/layer/image"
	"ctrlirl-coder/internal/layer/testcode"
	"ctrlirl-coder/internal/layer/text"
	"ctrlirl-coder/internal/layer/video"
	"ctrlirl-coder/internal/scene"
)

// buildLayer constructs the concrete layer for a scene descriptor. The base
// layer package holds the Layer interface and the BaseLayer base class;
// the concrete implementations live in its subpackages.
func buildLayer(sl scene.Layer) (layer.Layer, error) {
	switch sl.Type {
	case scene.LayerTypeVideo:
		return video.NewVideoSource(sl), nil
	case scene.LayerTypeImage:
		return image.NewImageOverlay(sl), nil
	case scene.LayerTypeText:
		return text.NewTextOverlay(sl), nil
	case scene.LayerTypeBrowser:
		return browser.NewBrowserOverlay(sl), nil
	case scene.LayerTypeTestCode:
		return testcode.NewTestCodeSource(sl), nil
	default:
		return nil, fmt.Errorf("layer %q: unknown type %q", sl.ID, sl.Type)
	}
}

// audioProvider is implemented by scene layers that carry their own audio
// (browser layers feed captured page audio, video layers feed their
// source's audio track; both mix into the mpegtsmux).
type audioProvider interface {
	AudioChain() (*layer.Chain, error)
}

// AddLayer adds a scene layer to the pipeline: builds the layer's element
// chain (video sources, image decodebin, pango textoverlay over a
// transparent surface), inserts
// the elements into the main pipeline, links the chain (after insertion,
// because gst_bin_add breaks pre-insertion links), requests a sink_%u pad on
// the compositor, links the chain output into it, sets the pad properties
// (zorder/xpos/ypos/width/height/alpha), and brings the chain to state.
// Audio-providing layers (browser, video media sources) additionally attach
// an audio chain to the mpegtsmux `mux` sink_%d pads.
//
// This is a command, so it is serialized with all other pipeline mutations.
// It is safe to call before Start (layers attached while PAUSED preroll with
// the pipeline) or after (elements are SyncStateWithParent'ed).
func (cp *CompositorPipeline) AddLayer(ctx context.Context, sl scene.Layer) error {
	if cp.compositor == nil {
		return ErrNoCompositor
	}
	l, err := buildLayer(sl)
	if err != nil {
		return err
	}
	// Every failure path closes the layer (browser layers own a Chrome
	// process + a pulse null sink — a leaked layer leaks both).
	chain, err := l.BuildChain()
	if err != nil {
		_ = l.Close()
		return err
	}
	if chain == nil || len(chain.Elements) == 0 {
		_ = l.Close()
		return fmt.Errorf("compositor: layer %s produced an empty chain", sl.ID)
	}

	var audio *layer.Chain
	if ap, ok := l.(audioProvider); ok {
		if a, aerr := ap.AudioChain(); aerr != nil {
			_ = l.Close()
			return aerr
		} else if a != nil {
			audio = a
		}
	}

	props := l.DefaultProperties()
	// Only valid compositor-pad properties are written onto the pad; type-
	// specific extras (e.g. browser framerate/quality) must not leak in.
	for k, v := range sl.Properties {
		if _, known := props[k]; known {
			props[k] = v
		}
	}

	return cp.doCtx(ctx, func() error {
		// Duplicate-id check on the command goroutine, before anything is
		// built: two concurrent AddLayer calls race otherwise (the old check
		// read the map off-goroutine, before the chain was even built).
		if _, dup := cp.layers[sl.ID]; dup {
			_ = l.Close()
			return fmt.Errorf("compositor: layer %q already attached", sl.ID)
		}

		bin := cp.pipeline.As()
		pad, err := attachChain(bin, cp.compositor, "sink_%u", chain)
		if err != nil {
			return fmt.Errorf("compositor: layer %s: %w", sl.ID, err)
		}
		out := pad.GetPeer()

		// HLS sources (uridecodebin3 → urisourcebin → hlsdemux2) prefetch in
		// VOD mode: GStreamer 1.28 gives no live switch for this path, so the
		// demux buffers to its 30 s watermark and the SRT-paced send side
		// overflows at every segment boundary (200-300 ms content hole →
		// the "glitch every 10 s"). Clamp its watermarks after preroll.
		if hls := findUrIDecodeBin(chain); hls != nil {
			cp.tuneHlsPrefetch(hls)
		}

		if err := setPadProps(pad, props); err != nil {
			out.Unlink(pad) // src pad first (gst_pad_unlink asserts src)
			cp.compositor.ReleaseRequestPad(pad)
			bin.RemoveMany(chain.Elements...)
			return err
		}

		h := &layerHandle{
			id:        sl.ID,
			pad:       pad,
			chain:     chain.Elements,
			outputPad: out,
			props:     props,
			layer:     l,
			spec:      sl,
		}

		h.volFilter = h.volumeFilterIndex()

		// Audio: attach the layer's PCM chain to the shared audiomixer tail
		// (one mixed track into a single mpegtsmux pad — MPEG-TS demuxers
		// like OBS only play the stream's first program, so per-layer AAC
		// programs would drop every layer after the first).
		if audio != nil && cp.muxEl != nil {
			mixer, merr := cp.ensureAudioTail(bin)
			if merr != nil {
				cp.teardownLayer(h)
				_ = l.Close()
				return fmt.Errorf("compositor: layer %s audio tail: %w", sl.ID, merr)
			}
			audioPad, aerr := attachChain(bin, mixer, "sink_%u", audio)
			if aerr != nil {
				// Roll back the whole layer (incl. audio, if partially
				// added) and the browser process.
				cp.teardownLayer(h)
				_ = l.Close()
				return fmt.Errorf("compositor: layer %s audio attach failed: %w", sl.ID, aerr)
			}
			h.audioChain = audio.Elements
			h.audioPad = audioPad
		}

		// A layer attached hidden (alpha 0) must not mix its audio into the
		// stream: the compositor's pad alpha only affects the video overlay.
		if h.audioChain != nil && props["alpha"] == 0 {
			cp.applyAudioMute(h, true)
		}

		cp.layers[sl.ID] = h
		slog.Info("compositor: layer attached", "id", sl.ID, "pad", pad.GetName(), "audio", h.audioPad != nil)
		return nil
	})
}

// ensureAudioTail builds the shared audio tail on first use:
// audiomixer -> audioconvert -> voaacenc -> aacparse -> one mpegtsmux pad.
// All layer audio chains mix into the mixer, so the stream carries exactly
// one audio program. Must be called on the command goroutine.
func (cp *CompositorPipeline) ensureAudioTail(bin gst.Bin) (gst.Element, error) {
	if cp.mixer != nil {
		return cp.mixer, nil
	}
	// NOTE: liveadder (not audiomixer). audiomixer fills any input timestamp
	// gap with digital silence — hlsdemux2 emits a ~0.4 s audio PTS jump at
	// each 10 s TS segment (CC-reset) boundary, and the mixer turned that into
	// the recurring 10 s audio glitch. liveadder (same sink_%u request-pad
	// mix model, built for live pipelines) sums its inputs and passes gaps
	// through without inserting silence, so the per-segment jump stops
	// manifesting as dead air.
	mixer, err := layer.MakeElement("liveadder", "amixer", nil)
	if err != nil {
		return nil, fmt.Errorf("compositor: %v", err)
	}
	conv, err := layer.MakeElement("audioconvert", "atailconv", nil)
	if err != nil {
		return nil, fmt.Errorf("compositor: %v", err)
	}
	enc, err := layer.MakeElement("voaacenc", "atailenc", map[string]any{"bitrate": 128000})
	if err != nil {
		return nil, fmt.Errorf("compositor: %v", err)
	}
	parse, err := layer.MakeElement("aacparse", "atailparse", nil)
	if err != nil {
		return nil, fmt.Errorf("compositor: %v", err)
	}
	els := []gst.Element{mixer, conv, enc, parse}
	var muxPad gst.Pad
	rollback := func() {
		if muxPad != nil {
			cp.muxEl.ReleaseRequestPad(muxPad)
		}
		bin.RemoveMany(els...)
	}
	for _, el := range els {
		if !bin.Add(el) {
			bin.RemoveMany(els...)
			return nil, fmt.Errorf("compositor: add %s to pipeline failed", el.GetName())
		}
	}
	if r := mixer.GetStaticPad("src").Link(conv.GetStaticPad("sink")); r != gst.PadLinkOK {
		rollback()
		return nil, fmt.Errorf("compositor: link amixer -> atailconv failed (%v)", r)
	}
	if r := conv.GetStaticPad("src").Link(enc.GetStaticPad("sink")); r != gst.PadLinkOK {
		rollback()
		return nil, fmt.Errorf("compositor: link atailconv -> atailenc failed (%v)", r)
	}
	if r := enc.GetStaticPad("src").Link(parse.GetStaticPad("sink")); r != gst.PadLinkOK {
		rollback()
		return nil, fmt.Errorf("compositor: link atailenc -> atailparse failed (%v)", r)
	}
	muxPad = cp.muxEl.RequestPadSimple("sink_%d")
	if muxPad == nil {
		rollback()
		return nil, fmt.Errorf("compositor: request of a mux audio pad failed")
	}
	if r := parse.GetStaticPad("src").Link(muxPad); r != gst.PadLinkOK {
		rollback()
		return nil, fmt.Errorf("compositor: link atailparse -> mux failed (%v)", r)
	}
	// Sink-first, like attachChain: the mixer must be ready before a layer
	// chain links in.
	for i := len(els) - 1; i >= 0; i-- {
		els[i].SyncStateWithParent()
	}
	cp.mixer = mixer
	slog.Info("compositor: audio tail attached (single-track mix)")
	return mixer, nil
}

// attachChain inserts chain's elements into bin (BEFORE any pad links:
// GStreamer 1.28's gst_bin_add breaks pre-insertion links), links the chain
// internally, requests an on-demand sink pad (padTemplate) from target,
// links the chain output into it, and brings the chain to the pipeline's
// state (deepest first). On any failure the pipeline is left exactly as it
// was (single rollback). Must be called on the command goroutine.
func attachChain(bin gst.Bin, target gst.Element, padTemplate string, chain *layer.Chain) (gst.Pad, error) {
	var (
		added []gst.Element
		pad   gst.Pad
		out   gst.Pad
	)
	rollback := func() {
		if out != nil && pad != nil {
			out.Unlink(pad)
		}
		if pad != nil {
			target.ReleaseRequestPad(pad)
		}
		bin.RemoveMany(added...)
	}

	for _, el := range chain.Elements {
		if !bin.Add(el) {
			bin.RemoveMany(added...)
			return nil, fmt.Errorf("add %s to pipeline failed", el.GetName())
		}
		added = append(added, el)
	}
	if err := chain.LinkChain(); err != nil {
		rollback()
		return nil, fmt.Errorf("link chain failed: %w", err)
	}
	pad = target.RequestPadSimple(padTemplate)
	if pad == nil {
		rollback()
		return nil, fmt.Errorf("request of a %q pad failed", padTemplate)
	}
	out = chain.SrcPad()
	if out == nil {
		rollback()
		return nil, fmt.Errorf("chain has no src pad")
	}
	if r := out.Link(pad); r != gst.PadLinkOK {
		rollback()
		return nil, fmt.Errorf("link chain -> %q pad failed (%v)", padTemplate, r)
	}
	for i := len(chain.Elements) - 1; i >= 0; i-- {
		chain.Elements[i].SyncStateWithParent()
	}
	return pad, nil
}

// teardownLayer NULLs and removes a layer's elements (video + audio chains)
// and releases its pads. Must be called on the command goroutine.
func (cp *CompositorPipeline) teardownLayer(h *layerHandle) {
	bin := cp.pipeline.As()
	for _, el := range h.chain {
		_ = gstutil.SetStateSafe(el, gst.StateNull)
	}
	if len(h.audioChain) > 0 {
		for _, el := range h.audioChain {
			_ = gstutil.SetStateSafe(el, gst.StateNull)
		}
		if h.audioPad != nil {
			src := h.audioChain[len(h.audioChain)-1].GetStaticPad("src")
			if src != nil {
				src.Unlink(h.audioPad)
			}
			cp.mixer.ReleaseRequestPad(h.audioPad)
		}
		bin.RemoveMany(h.audioChain...)
	}
	// Unlink from the src pad side: gst_pad_unlink expects the src pad first
	// (h.pad is the compositor's requested *sink* pad).
	h.outputPad.Unlink(h.pad)
	cp.compositor.ReleaseRequestPad(h.pad)
	bin.RemoveMany(h.chain...)
}

// RemoveLayer detaches a scene layer: sets its elements to NULL, unlinks and
// releases the requested compositor pad, and removes the elements from the
// pipeline (spec §7.5: always NULL before remove).
//
// While the pipeline is PLAYING the source is first frozen (READY): a live
// source (uridecodebin over HLS) can block forever inside
// gst_element_set_state(NULL) draining its in-flight network read, and that
// stuck C call wedges the command goroutine — every later command,
// including Stop() on SIGINT, then hangs forever.
func (cp *CompositorPipeline) RemoveLayer(ctx context.Context, id string) error {
	if cp.compositor == nil {
		return ErrNoCompositor
	}
	return cp.doCtx(ctx, func() error {
		h, ok := cp.layers[id]
		if !ok {
			return fmt.Errorf("compositor: no layer %q", id)
		}
		// A suspended batch already holds the pipeline at READY — one
		// freeze/thaw for the whole scene switch, not per layer.
		freeze := cp.started.Load() && cp.suspended.Load() == 0 && !noFreeze
		if freeze {
			_ = cp.pipeline.SetState(gst.StateReady)
		}
		cp.teardownLayer(h)
		_ = h.layer.Close() // browser: kills Chrome + unloads the pulse sink
		delete(cp.layers, id)
		if freeze {
			_ = cp.pipeline.SetState(gst.StatePlaying)
		}
		slog.Info("compositor: layer removed", "id", id)
		return nil
	})
}

// UpdateLayerProps updates an attached layer in place: compositor-pad
// properties (zorder/xpos/ypos/width/height/alpha).
func (cp *CompositorPipeline) UpdateLayerProps(ctx context.Context, id string, props map[string]float64) error {
	if cp.compositor == nil {
		return ErrNoCompositor
	}
	return cp.doCtx(ctx, func() error {
		h, ok := cp.layers[id]
		if !ok {
			return fmt.Errorf("compositor: no layer %q", id)
		}
		// Write only the pad properties whose value actually changed; the
		// rest are re-asserted on every scene apply.
		changed := make(map[string]float64, len(props))
		for k, v := range props {
			if cur, known := h.props[k]; known && cur != v {
				h.props[k] = v
				changed[k] = v
			}
		}
		if err := setPadProps(h.pad, changed); err != nil {
			return err
		}
		// Pad alpha only hides the video overlay; the audio chain is
		// independent. Hide/show must follow through to the audio.
		if a, ok := changed["alpha"]; ok {
			cp.applyAudioMute(h, a == 0)
		}
		return nil
	})
}

// volumeFilterIndex returns the index of the layer's volume filter in
// spec.Filters, or -1 if the layer has none.
func (h *layerHandle) volumeFilterIndex() int {
	for i, f := range h.spec.Filters {
		if f.Kind == "volume" {
			return i
		}
	}
	return -1
}

// applyAudioMute mutes (alpha 0) or un-mutes (alpha > 0) a layer's audio
// chain in place: the layer's volume filter element gets its `mute` property
// set. Same element the volume ops write, so no chain churn; the element's
// level is untouched, so un-muting restores the configured volume. A layer
// without a volume filter is left untouched.
func (cp *CompositorPipeline) applyAudioMute(h *layerHandle, muted bool) {
	if len(h.audioChain) == 0 || h.volFilter < 0 {
		return
	}
	name, ok := layer.AudioFilterElementName(h.spec, h.volFilter)
	if !ok {
		return
	}
	el := cp.pipeline.As().GetByName(name)
	if el == nil {
		return
	}
	el.SetObjectProperty("mute", muted)
	slog.Info("compositor: layer audio muted in place", "id", h.id, "muted", muted)
}

// UpdateLayerFilter re-applies a filter spec to the layer's running filter
// element in place (in-place-capable filters only — volume-level nudges).
// No chain teardown: the browser layer's Chrome process is untouched and
// the mpegtsmux audio pads keep their PIDs (a remove+add rebuild renumbers
// them and reshuffles the stream's audio programs).
func (cp *CompositorPipeline) UpdateLayerFilter(ctx context.Context, id string, index int, spec scene.FilterSpec) error {
	if cp.compositor == nil {
		return ErrNoCompositor
	}
	return cp.doCtx(ctx, func() error {
		h, ok := cp.layers[id]
		if !ok {
			return fmt.Errorf("compositor: no layer %q", id)
		}
		if len(h.audioChain) == 0 {
			return fmt.Errorf("compositor: layer %q has no audio chain", id)
		}
		name, ok := layer.AudioFilterElementName(h.spec, index)
		if !ok {
			return fmt.Errorf("compositor: layer %q has no audio filter at index %d", id, index)
		}
		el := cp.pipeline.As().GetByName(name)
		if el == nil {
			return fmt.Errorf("compositor: filter element %q not found in pipeline", name)
		}
		if err := layer.SetFilterElementProps(el, spec); err != nil {
			return fmt.Errorf("compositor: layer %q filter %d: %w", id, index, err)
		}
		// Keep the handle's spec current: a later alpha -> un-mute restores
		// the volume element to this level. index is audio-filter-only
		// numbering, so resolve the full spec index by element name.
		for i := range h.spec.Filters {
			if n, ok := layer.AudioFilterElementName(h.spec, i); ok && n == name {
				h.spec.Filters[i] = spec
				if spec.Kind == "volume" {
					h.volFilter = i
				}
				break
			}
		}
		slog.Info("compositor: filter updated in place", "id", id, "index", index, "kind", spec.Kind)
		return nil
	})
}

// setPadProps writes the compositor pad properties via the pad's Object
// property setter. All compositor pad properties (zorder/xpos/ypos/width/
// height/alpha) are float64-typed in GStreamer.
func setPadProps(pad gst.Pad, props map[string]float64) error {
	for k, v := range props {
		pad.SetObjectProperty(k, v)
	}
	return nil
}
