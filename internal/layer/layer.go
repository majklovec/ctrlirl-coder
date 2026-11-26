// Package layer is the base layer framework. It defines the Layer
// interface, the BaseLayer base class with the common behavior every
// concrete layer type inherits (id/type accessors, default compositor-pad
// properties, no-op Close), the shared Chain type that all layer chains are
// built from, and the shared chain-building helpers (MakeElement,
// SurfaceChain, Tail, Video/AudioFilters).
//
// Concrete layer types live in subpackages — internal/layer/video,
// internal/layer/image, internal/layer/text, internal/layer/browser, and
// internal/layer/testcode — and
// embed layer.BaseLayer. The compositor selects the concrete constructor
// per scene layer type (see compositor.AddLayer).
//
// A layer is a flat, pre-ordered element chain (source → convert →
// capsfilter → queue). Chains are flat (not *gst.Bin subtrees) because
// go-gst v1.4.1 does not expose gst_bin_add_ghost_pad, so a bin's source pad
// cannot be reached from Go to link into the compositor (spec §5.4
// revision). The compositor adds the elements to the main pipeline, then
// links them: GStreamer 1.28's gst_bin_add breaks links made before
// insertion, so the internal links MUST be (re-)created after all elements
// are in the bin (LinkChain).
//
// Surface chains (video/image) end with a capsfilter forcing BGRA (the
// compositor-friendly caps, spec §7.10). Text layers use a transparent
// live source plus pango `textoverlay`, then feed the rendered alpha surface
// into the compositor.
package layer

import (
	"ctrlirl-coder/internal/scene"
)

// Layer is a single composable layer. BuildChain creates the elements in
// link order (unlinked); the compositor adds them to the main pipeline,
// links the chain (Chain.LinkChain), and links the chain output into a
// requested compositor sink pad.
type Layer interface {
	// ID returns the scene layer id.
	ID() string
	// Type returns a supported scene layer type.
	Type() string
	// BuildChain creates the elements. Call it exactly once.
	BuildChain() (*Chain, error)
	// DefaultProperties returns the compositor-pad properties (zorder, xpos,
	// ypos, width, height, alpha) as float64, matching the scene schema.
	DefaultProperties() map[string]float64
	// Close releases resources. After the compositor has added the chain to
	// the pipeline, this is a no-op (the pipeline owns the elements).
	Close() error
}

// BaseLayer is the base class every concrete layer type embeds. It carries
// the scene descriptor and the layer kind, and provides the shared
// boilerplate: the id/type accessors, the default compositor-pad
// properties, and a no-op Close. Concrete layers embed it and override the
// methods they need (browser: Close kills Chrome and the pulse sink;
// text/browser: filtered DefaultProperties).
type BaseLayer struct {
	// L is the scene layer descriptor.
	L scene.Layer
	// Kind is the scene layer type ("video", "image", "text", "browser").
	Kind scene.LayerType
}

// NewBaseLayer builds the BaseLayer for a scene descriptor and layer kind.
func NewBaseLayer(l scene.Layer, kind scene.LayerType) BaseLayer {
	return BaseLayer{L: l, Kind: kind}
}

func (b *BaseLayer) ID() string { return b.L.ID }

func (b *BaseLayer) Type() string { return string(b.Kind) }

func (b *BaseLayer) DefaultProperties() map[string]float64 { return DefaultProps(b.L) }

// Close is a no-op (the pipeline owns the elements once attached).
func (b *BaseLayer) Close() error { return nil }

// DefaultProps merges the scene properties over the sensible fallbacks.
func DefaultProps(l scene.Layer) map[string]float64 {
	defs := map[string]float64{"zorder": 1, "xpos": 0, "ypos": 0, "width": 1280, "height": 720, "alpha": 1.0}
	out := make(map[string]float64, len(defs))
	for k, v := range defs {
		out[k] = v
	}
	for k, v := range l.Properties {
		if _, exists := defs[k]; exists {
			out[k] = v
		}
	}
	return out
}
