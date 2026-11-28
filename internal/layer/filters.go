package layer

import (
	"fmt"
	"slices"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/scene"
)

// filterElementName is the name of a filter chain element: `<id>-fNN-<what>`,
// NN being its position among the elements of its format's filter list. The
// names are stable per scene layer, so an in-place update can find the
// running element (AudioFilterElementName) and a rebuilt chain cannot
// collide with live names.
func filterElementName(id string, pos int, what string) string {
	return fmt.Sprintf("%s-f%02d-%s", id, pos, what)
}

// buildFilters realizes a layer's filter specs (of one format) as chain
// elements, in scene order.
//
// Most filters are one element. Three things add more:
//
//   - Video filters ride directly on the decoded caps. Decoders commonly
//     emit I420/Y420, and filters like `rotate` refuse those formats — the
//     link would fail, the layer's compositor pad would produce no buffer,
//     and the compositor (which needs every pad) would go fully black. A
//     videoconvert in front of the list negotiates a format every filter
//     accepts.
//   - videoscale only conforms to the caps it is given, so a trailing
//     capsfilter carries the target width/height.
//   - videorotate gets a BGRA capsfilter (and a videoconvert when another
//     filter follows), see below.
func buildFilters(l scene.Layer, format scene.FilterFormat) ([]gst.Element, error) {
	var specs []Spec
	add := func(factory, what string, props map[string]any, caps string) {
		specs = append(specs, Spec{Factory: factory, Name: filterElementName(l.ID, len(specs), what), Props: props, Caps: caps})
	}
	var filters []scene.FilterSpec
	for _, f := range l.Filters {
		if def, ok := scene.LookupFilter(f.Kind); ok && def.Format == format {
			filters = append(filters, f)
		}
	}
	for i, f := range filters {
		def, _ := scene.LookupFilter(f.Kind)
		eff := def.EffectiveParams(f.Params)
		factory, err := def.FactoryFor(eff)
		if err != nil {
			return nil, fmt.Errorf("layer: %s filter %d (%s): %w", l.ID, i, f.Kind, err)
		}
		add(factory, f.Kind, FilterElementProps(def, eff), "")

		switch f.Kind {
		case "videorotate":
			// rotate is GstGeometricTransform: it fill-cleans its output
			// frame with memset(0) before mapping. That's transparent only
			// in an alpha format — in an opaque one the corners come out as
			// solid black, and the layer composites as a black box. Pin the
			// rotate's output to BGRA so the un-mapped area carries alpha=0.
			// If a later filter in the list follows, a videoconvert carries
			// the format change (YUV-only filters like effectv can't keep
			// the alpha — in that order the corners stay black, unavoidable).
			add("capsfilter", "alpha", nil, "video/x-raw,format=BGRA")
			if i+1 < len(filters) {
				add("videoconvert", "postconv", nil, "")
			}
		case "videoscale":
			caps, err := scaleCaps(eff)
			if err != nil {
				return nil, fmt.Errorf("layer: %s filter %d: %w", l.ID, i, err)
			}
			add("capsfilter", "scalecaps", nil, caps)
		}
	}
	if format == scene.FormatVideo && len(specs) > 0 {
		// The pre-convert is named outside the fNN sequence.
		specs = slices.Insert(specs, 0, Spec{Factory: "videoconvert", Name: l.ID + "-fconv"})
	}
	els, err := Build(specs...)
	if err != nil {
		return nil, fmt.Errorf("layer: %s filters: %w", l.ID, err)
	}
	return els, nil
}

// FilterElementProps resolves a filter's effective params into the element
// property set: SkipProps removed (videoscale size, equalizer bands are not
// element properties), per-kind unit rewrites applied (videorotate degrees
// → radians), and int-typed params normalized to int64 so the cgo path
// routes them through g_object_setv per the property's GParamSpec
// (gdouble values are rejected by GStreamer int/uint/enum properties).
// Shared by chain building and in-place updates so both paths agree.
func FilterElementProps(def scene.FilterDef, eff map[string]any) map[string]any {
	skip := make(map[string]bool, len(def.SkipProps))
	for _, k := range def.SkipProps {
		skip[k] = true
	}
	params := make(map[string]any, len(eff))
	src := eff
	if def.TransformParams != nil {
		src = def.TransformParams(eff)
	}
	for k, v := range src {
		if skip[k] {
			continue
		}
		if pd, ok := def.Params[k]; ok && pd.Kind == scene.ParamInt {
			switch val := v.(type) {
			case float64:
				v = int64(val)
			case int:
				v = int64(val)
			}
		}
		params[k] = v
	}
	return params
}

// AudioFilterElementName returns the chain element name of the filter at
// index i of l.Filters, which must be an audio filter. (Audio filters are
// one element each, so the position among the audio filters is the NN of
// the name buildFilters gave it.)
func AudioFilterElementName(l scene.Layer, i int) (string, bool) {
	pos := 0
	for j, f := range l.Filters {
		def, ok := scene.LookupFilter(f.Kind)
		if !ok || def.Format != scene.FormatAudio {
			continue
		}
		if j == i {
			return filterElementName(l.ID, pos, f.Kind), true
		}
		pos++
	}
	return "", false
}

// SetFilterElementProps applies a filter spec to a running element in place
// (in-place-capable registry kinds only — the scene diff never routes
// anything else down this path).
func SetFilterElementProps(el gst.Element, spec scene.FilterSpec) error {
	def, ok := scene.LookupFilter(spec.Kind)
	if !ok || !def.InPlace {
		return fmt.Errorf("layer: filter %q is not in-place updatable", spec.Kind)
	}
	for k, v := range FilterElementProps(def, def.EffectiveParams(spec.Params)) {
		el.SetObjectProperty(k, v)
	}
	return nil
}

// scaleCaps is the caps a videoscale filter is pinned to: width only, height
// only, or both (a 0 dimension keeps the incoming value).
func scaleCaps(eff map[string]any) (string, error) {
	w, _ := eff["width"].(float64)
	h, _ := eff["height"].(float64)
	switch {
	case w > 0 && h > 0:
		return fmt.Sprintf("video/x-raw,width=%d,height=%d", int(w), int(h)), nil
	case w > 0:
		return fmt.Sprintf("video/x-raw,width=%d", int(w)), nil
	case h > 0:
		return fmt.Sprintf("video/x-raw,height=%d", int(h)), nil
	}
	// The scene registry rejects a videoscale without a dimension, so this
	// is an unvalidated spec.
	return "", fmt.Errorf("videoscale: neither width nor height set")
}

// VideoFilters returns the layer's video filter elements (empty, not nil,
// when the layer declares none). Callers append them in front of the
// videoconvert tail.
func VideoFilters(l scene.Layer) ([]gst.Element, error) { return buildFilters(l, scene.FormatVideo) }

// AudioFilters returns the layer's audio filter elements (browser audio
// chain).
func AudioFilters(l scene.Layer) ([]gst.Element, error) { return buildFilters(l, scene.FormatAudio) }
