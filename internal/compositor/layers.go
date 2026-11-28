package compositor

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"

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

// preparedLayer is a layer whose element chains are built but not yet in the
// pipeline.
type preparedLayer struct {
	spec  scene.Layer
	layer layer.Layer
	chain *layer.Chain       // video chain, ends in the compositor
	audio *layer.Chain       // audio chain, nil when the layer has none
	props map[string]float64 // compositor-pad properties

	// claimed settles who cleans up when AddLayer's command is abandoned
	// (pipeline stopped, context expired): whoever flips it first — the
	// command goroutine about to attach, or the caller giving up.
	claimed atomic.Bool
}

// prepareLayer builds the layer and its chains. It touches no pipeline
// state, so it runs on the caller's goroutine (a browser layer starts its
// Chrome here). On failure the layer is closed.
func prepareLayer(sl scene.Layer) (_ *preparedLayer, err error) {
	l, err := buildLayer(sl)
	if err != nil {
		return nil, err
	}
	// A leaked layer leaks what it owns: browser layers a Chrome process
	// and a pulse null sink.
	defer func() {
		if err != nil {
			_ = l.Close()
		}
	}()
	p := &preparedLayer{spec: sl, layer: l}
	if p.chain, err = l.BuildChain(); err != nil {
		return nil, err
	}
	if p.chain == nil || len(p.chain.Elements) == 0 {
		return nil, fmt.Errorf("compositor: layer %s produced an empty chain", sl.ID)
	}
	if ap, ok := l.(audioProvider); ok {
		if p.audio, err = ap.AudioChain(); err != nil {
			return nil, err
		}
	}
	// Only valid compositor-pad properties are written onto the pad; type-
	// specific extras (e.g. browser framerate/quality) must not leak in.
	p.props = l.DefaultProperties()
	for k, v := range sl.Properties {
		if _, known := p.props[k]; known {
			p.props[k] = v
		}
	}
	return p, nil
}

// AddLayer adds a scene layer to the pipeline: builds the layer's element
// chain (video sources, image decodebin, pango textoverlay over a
// transparent surface), inserts the elements into the main pipeline, links
// the chain (after insertion, because gst_bin_add breaks pre-insertion
// links), requests a sink_%u pad on the compositor, links the chain output
// into it, sets the pad properties (zorder/xpos/ypos/width/height/alpha),
// and brings the chain to state. Audio-providing layers (browser, video
// media sources) additionally attach an audio chain to the shared audio
// tail.
//
// The pipeline work is a command, so it is serialized with all other
// pipeline mutations. It is safe to call before Start (layers attached while
// PAUSED preroll with the pipeline) or after (elements are
// SyncStateWithParent'ed). Whatever fails, the layer is closed.
func (cp *CompositorPipeline) AddLayer(ctx context.Context, sl scene.Layer) error {
	if cp.compositor == nil {
		return ErrNoCompositor
	}
	p, err := prepareLayer(sl)
	if err != nil {
		return err
	}
	return cp.addPrepared(ctx, p)
}

// addPrepared attaches a prepared layer on the command goroutine and closes
// it if that fails — or never happens.
func (cp *CompositorPipeline) addPrepared(ctx context.Context, p *preparedLayer) error {
	err := cp.doCtx(ctx, func() error {
		if !p.claimed.CompareAndSwap(false, true) {
			return nil // the caller gave up on this command and closed the layer
		}
		if err := cp.attachLayer(p); err != nil {
			_ = p.layer.Close()
			return err
		}
		return nil
	})
	if err != nil && p.claimed.CompareAndSwap(false, true) {
		_ = p.layer.Close() // the command never ran
	}
	return err
}

// attachLayer puts a prepared layer into the pipeline. On failure the
// pipeline is left as it was. Must be called on the command goroutine.
func (cp *CompositorPipeline) attachLayer(p *preparedLayer) error {
	id := p.spec.ID
	// The duplicate check belongs here, on the command goroutine: two
	// concurrent AddLayer calls would both pass a check made earlier.
	if _, dup := cp.layers[id]; dup {
		return fmt.Errorf("compositor: layer %q already attached", id)
	}
	bin := cp.pipeline.As()
	pad, err := attachChain(bin, cp.compositor, "sink_%u", p.chain)
	if err != nil {
		return fmt.Errorf("compositor: layer %s: %w", id, err)
	}
	h := &layerHandle{
		id:        id,
		pad:       pad,
		chain:     p.chain.Elements,
		outputPad: pad.GetPeer(),
		props:     p.props,
		layer:     p.layer,
		spec:      p.spec,
	}
	h.volFilter = h.volumeFilterIndex()

	// HLS sources (uridecodebin3 → urisourcebin → hlsdemux2) prefetch in
	// VOD mode: GStreamer 1.28 gives no live switch for this path, so the
	// demux buffers to its 30 s watermark and the SRT-paced send side
	// overflows at every segment boundary (200-300 ms content hole →
	// the "glitch every 10 s"). Clamp its watermarks after preroll.
	if hls := findUrIDecodeBin(p.chain); hls != nil {
		cp.tuneHlsPrefetch(hls)
	}
	setPadProps(pad, p.props)

	if p.audio != nil && cp.muxEl != nil {
		if err := cp.attachAudio(h, p.audio); err != nil {
			cp.teardownLayer(h)
			return fmt.Errorf("compositor: layer %s audio: %w", id, err)
		}
		// A layer attached hidden (alpha 0) must not mix its audio into the
		// stream: the compositor's pad alpha only affects the video overlay.
		if p.props["alpha"] == 0 {
			cp.applyAudioMute(h, true)
		}
	}

	cp.layers[id] = h
	slog.Info("compositor: layer attached", "id", id, "pad", pad.GetName(), "audio", h.audioPad != nil)
	return nil
}

// attachAudio links the layer's PCM chain into the shared audio tail (one
// mixed track into a single mpegtsmux pad — MPEG-TS demuxers like OBS only
// play the stream's first program, so per-layer AAC programs would drop
// every layer after the first). Must be called on the command goroutine.
func (cp *CompositorPipeline) attachAudio(h *layerHandle, audio *layer.Chain) error {
	bin := cp.pipeline.As()
	mixer, err := cp.ensureAudioTail(bin)
	if err != nil {
		return err
	}
	pad, err := attachChain(bin, mixer, "sink_%u", audio)
	if err != nil {
		return err
	}
	h.audioChain, h.audioPad = audio.Elements, pad
	return nil
}

// ensureAudioTail builds the shared audio tail on first use:
// liveadder -> audioconvert -> voaacenc -> aacparse -> one mpegtsmux pad.
// All layer audio chains mix into the adder, so the stream carries exactly
// one audio program. Must be called on the command goroutine.
func (cp *CompositorPipeline) ensureAudioTail(bin gst.Bin) (gst.Element, error) {
	if cp.mixer != nil {
		return cp.mixer, nil
	}
	els, err := layer.Build(
		// liveadder, not audiomixer: audiomixer fills any input timestamp
		// gap with digital silence — hlsdemux2 emits a ~0.4 s audio PTS jump
		// at each 10 s TS segment (CC-reset) boundary, and the mixer turned
		// that into the recurring 10 s audio glitch. liveadder (same sink_%u
		// request-pad mix model, built for live pipelines) sums its inputs
		// and passes gaps through without inserting silence, so the
		// per-segment jump stops manifesting as dead air.
		layer.Spec{Factory: "liveadder", Name: "amixer"},
		layer.Spec{Factory: "audioconvert", Name: "atailconv"},
		layer.Spec{Factory: "voaacenc", Name: "atailenc", Props: map[string]any{"bitrate": 128000}},
		layer.Spec{Factory: "aacparse", Name: "atailparse"},
	)
	if err != nil {
		return nil, err
	}
	// The tail is a chain like any other, ending in a mux pad. attachChain
	// brings it up sink-first, so the adder is ready before a layer chain
	// links in.
	if _, err := attachChain(bin, cp.muxEl, "sink_%d", layer.StaticChain(els...)); err != nil {
		return nil, fmt.Errorf("audio tail: %w", err)
	}
	cp.mixer = els[0]
	slog.Info("compositor: audio tail attached (single-track mix)")
	return cp.mixer, nil
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

// withLayer runs fn on the command goroutine with the attached layer id.
func (cp *CompositorPipeline) withLayer(ctx context.Context, id string, fn func(h *layerHandle) error) error {
	if cp.compositor == nil {
		return ErrNoCompositor
	}
	return cp.doCtx(ctx, func() error {
		h, ok := cp.layers[id]
		if !ok {
			return fmt.Errorf("compositor: no layer %q", id)
		}
		return fn(h)
	})
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
	return cp.withLayer(ctx, id, func(h *layerHandle) error {
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
	return cp.withLayer(ctx, id, func(h *layerHandle) error {
		// Write only the pad properties whose value actually changed; the
		// rest are re-asserted on every scene apply.
		changed := make(map[string]float64, len(props))
		for k, v := range props {
			if cur, known := h.props[k]; known && cur != v {
				h.props[k] = v
				changed[k] = v
			}
		}
		setPadProps(h.pad, changed)
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
	return slices.IndexFunc(h.spec.Filters, func(f scene.FilterSpec) bool { return f.Kind == "volume" })
}

// audioFilterElement finds the running element of the layer's audio filter
// at index (an index into the layer's Filters).
func (cp *CompositorPipeline) audioFilterElement(h *layerHandle, index int) (gst.Element, error) {
	if len(h.audioChain) == 0 {
		return nil, fmt.Errorf("compositor: layer %q has no audio chain", h.id)
	}
	name, ok := layer.AudioFilterElementName(h.spec, index)
	if !ok {
		return nil, fmt.Errorf("compositor: layer %q has no audio filter at index %d", h.id, index)
	}
	el := cp.pipeline.As().GetByName(name)
	if el == nil {
		return nil, fmt.Errorf("compositor: filter element %q not found in pipeline", name)
	}
	return el, nil
}

// applyAudioMute mutes (alpha 0) or un-mutes (alpha > 0) a layer's audio
// chain in place: the layer's volume filter element gets its `mute` property
// set. Same element the volume ops write, so no chain churn; the element's
// level is untouched, so un-muting restores the configured volume. A layer
// without audio or without a volume filter is left untouched.
func (cp *CompositorPipeline) applyAudioMute(h *layerHandle, muted bool) {
	if h.volFilter < 0 {
		return
	}
	el, err := cp.audioFilterElement(h, h.volFilter)
	if err != nil {
		return
	}
	el.SetObjectProperty("mute", muted)
	slog.Info("compositor: layer audio muted in place", "id", h.id, "muted", muted)
}

// UpdateLayerFilter re-applies a filter spec to the layer's running filter
// element in place (in-place-capable filters only — volume-level nudges).
// index is the filter's position in the layer's filter list. No chain
// teardown: the browser layer's Chrome process is untouched and the
// mpegtsmux audio pads keep their PIDs (a remove+add rebuild renumbers them
// and reshuffles the stream's audio programs).
func (cp *CompositorPipeline) UpdateLayerFilter(ctx context.Context, id string, index int, spec scene.FilterSpec) error {
	return cp.withLayer(ctx, id, func(h *layerHandle) error {
		el, err := cp.audioFilterElement(h, index)
		if err != nil {
			return err
		}
		if err := layer.SetFilterElementProps(el, spec); err != nil {
			return fmt.Errorf("compositor: layer %q filter %d: %w", id, index, err)
		}
		// Keep the handle's spec current: a later alpha → un-mute restores
		// the volume element to this level.
		h.spec.Filters[index] = spec
		slog.Info("compositor: filter updated in place", "id", id, "index", index, "kind", spec.Kind)
		return nil
	})
}

// setPadProps writes compositor pad properties (zorder/xpos/ypos/width/
// height/alpha — all float64-typed in GStreamer).
func setPadProps(pad gst.Pad, props map[string]float64) {
	for k, v := range props {
		pad.SetObjectProperty(k, v)
	}
}
