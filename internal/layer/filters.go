package layer

import (
	"fmt"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/scene"
)

// buildFilters realizes a layer's filter specs (of one format) as chain
// elements, in scene order. Element names are `<id>-fNN-<kind>` so a scene
// reload that rebuilds the chain cannot collide with live names.
//
// videoscale is the one spec that yields two elements: the scale element
// plus a trailing capsfilter carrying the target width/height (the element
// itself only conforms to the caps it is given).
//
// Video filters ride directly on the decoded caps. Decoders commonly emit
// I420/Y420, and filters like `rotate` refuse those formats — the link
// would fail, the layer's compositor pad would produce no buffer, and the
// compositor (which needs every pad) would go fully black. A videoconvert
// in front of the filter list negotiates a format every filter accepts.
// specBuild is one resolved filter spec: the registry entry's effective
// params, element factory, and element properties — resolved once per spec.
type specBuild struct {
	spec    scene.FilterSpec
	params  map[string]any
	factory string
	props   map[string]any
}

func buildFilters(l scene.Layer, format scene.FilterFormat) ([]gst.Element, error) {
	// Resolve every spec once (registry lookup + effective params + factory
	// + element properties).
	var builds []specBuild
	for i, spec := range l.Filters {
		def, ok := scene.LookupFilter(spec.Kind)
		if !ok || def.Format != format {
			continue
		}
		eff := def.EffectiveParams(spec.Params)
		factory, err := def.FactoryFor(eff)
		if err != nil {
			return nil, fmt.Errorf("layer: %s filter %d (%s): %w", l.ID, i, spec.Kind, err)
		}
		props := FilterElementProps(def, eff)
		builds = append(builds, specBuild{spec: spec, params: eff, factory: factory, props: props})
	}

	var els []gst.Element
	if format == scene.FormatVideo && len(builds) > 0 {
		conv, err := MakeElement("videoconvert", l.ID+"-fconv", nil)
		if err != nil {
			return nil, fmt.Errorf("layer: %s filter preconvert: %w", l.ID, err)
		}
		els = append(els, conv)
	}
	// Element names are `<id>-fNN-<kind>` (NN = position among this
	// format's elements, incl. a videoscale's trailing capsfilter) so a
	// scene reload that rebuilds the chain cannot collide with live names.
	nameIdx := 0
	for i, b := range builds {
		name := fmt.Sprintf("%s-f%02d-%s", l.ID, nameIdx, b.spec.Kind)
		el, err := MakeElement(b.factory, name, b.props)
		if err != nil {
			return nil, fmt.Errorf("layer: %s filter %d: %w", l.ID, i, err)
		}
		els = append(els, el)
		nameIdx++

		// rotate is GstGeometricTransform: it fill-cleans its output
		// frame with memset(0) before mapping. That's transparent only
		// in an alpha format — in an opaque one the corners come out as
		// solid black, and the layer composites as a black box. Pin the
		// rotate's output to BGRA so the un-mapped area carries alpha=0.
		// If a later filter in the list follows, a videoconvert carries
		// the format change (YUV-only filters like effectv can't keep
		// the alpha — in that order the corners stay black, unavoidable).
		if b.spec.Kind == "videorotate" {
			ca, err := MakeElement("capsfilter", fmt.Sprintf("%s-f%02d-alpha", l.ID, nameIdx), nil)
			if err != nil {
				return nil, fmt.Errorf("layer: %s filter %d: rotate alpha capsfilter: %w", l.ID, i, err)
			}
			ca.SetObjectProperty("caps", gst.CapsFromString("video/x-raw,format=BGRA"))
			els = append(els, ca)
			nameIdx++
			if i+1 < len(builds) {
				cc, err := MakeElement("videoconvert", fmt.Sprintf("%s-f%02d-postconv", l.ID, nameIdx), nil)
				if err != nil {
					return nil, fmt.Errorf("layer: %s filter %d: rotate postconvert: %w", l.ID, i, err)
				}
				els = append(els, cc)
				nameIdx++
			}
		}

		// videoscale: the target size is enforced by a capsfilter right
		// after the scaler (width only, height only, or both).
		if b.spec.Kind == "videoscale" {
			cf := capsfilterForScale(l.ID, nameIdx, b.params)
			if cf == nil {
				return nil, fmt.Errorf("layer: %s filter %d: videoscale capsfilter missing", l.ID, i)
			}
			els = append(els, cf)
			nameIdx++
		}
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

// AudioFilterElementName returns the chain element name assigned to audio
// filter index i (NN = position among the layer's audio-format filters,
// matching buildFilters' naming).
func AudioFilterElementName(l scene.Layer, i int) (string, bool) {
	nn := 0
	for j, spec := range l.Filters {
		def, ok := scene.LookupFilter(spec.Kind)
		if !ok || def.Format != scene.FormatAudio {
			continue
		}
		if j == i {
			return fmt.Sprintf("%s-f%02d-%s", l.ID, nn, spec.Kind), true
		}
		nn++
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

// capsfilterForScale builds the trailing capsfilter for a videoscale filter.
// A bare "0" dimension keeps the current value (the scene registry requires
// at least one of the two).
func capsfilterForScale(id string, idx int, eff map[string]any) gst.Element {
	var dim string
	w, h := int(eff["width"].(float64)), int(eff["height"].(float64))
	switch {
	case w > 0 && h > 0:
		dim = fmt.Sprintf("width=%d,height=%d", w, h)
	case w > 0:
		dim = fmt.Sprintf("width=%d", w)
	case h > 0:
		dim = fmt.Sprintf("height=%d", h)
	default:
		// The scene registry rejects a videoscale without a dimension, so
		// a nil result here means an unvalidated spec slipped through.
		return nil
	}
	cf, err := MakeElement("capsfilter", fmt.Sprintf("%s-f%02d-scalecaps", id, idx), nil)
	if err != nil {
		return nil
	}
	cf.SetObjectProperty("caps", gst.CapsFromString("video/x-raw,"+dim))
	return cf
}

// VideoFilters returns the layer's video filter elements (empty, not nil,
// when the layer declares none). Callers append them in front of the
// videoconvert tail.
func VideoFilters(l scene.Layer) ([]gst.Element, error) { return buildFilters(l, scene.FormatVideo) }

// AudioFilters returns the layer's audio filter elements (browser audio
// chain).
func AudioFilters(l scene.Layer) ([]gst.Element, error) { return buildFilters(l, scene.FormatAudio) }
