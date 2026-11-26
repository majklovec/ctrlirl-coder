package scene

import (
	"fmt"
	"math"
)

// FilterSpec is one media filter attached to a scene layer. Filters are
// inserted into the layer's element chain in scene order (video filters
// before the videoconvert/capsfilter tail, audio filters inside the layer's
// audio chain). A filter change forces a chain rebuild (remove + add op in
// the scene diff) — the compositor cannot splice elements into a
// running chain.
type FilterSpec struct {
	Kind   string         `json:"kind"`
	Params map[string]any `json:"params,omitempty"`
	// inPlaceChanged is a diff bookkeeping flag (unexported: never
	// serialized). Set by Diff on the slots whose params changed in an
	// OpUpdateFilter op so ApplyOp updates exactly those.
	inPlaceChanged bool
}

// FilterParamKind is the Go type a filter param arrives as from JSON.
type FilterParamKind int

const (
	ParamFloat FilterParamKind = iota // any JSON number
	ParamInt                          // JSON number with an integral value
	ParamBool
	ParamString
)

// FilterParamDef describes one accepted filter parameter.
type FilterParamDef struct {
	Kind    FilterParamKind
	Default any // value applied when the scene omits the param
}

// FilterFormat is the chain a filter lands in.
type FilterFormat string

// The two filter formats.
const (
	FormatVideo FilterFormat = "video"
	FormatAudio FilterFormat = "audio"
)

// FilterDef is one supported filter kind. The registry is pure data (no
// GStreamer import): the scene package validates against it, the layer
// package builds elements from it.
type FilterDef struct {
	Kind   string
	Format FilterFormat // which chain the filter lands in
	// Factory is the GStreamer element factory; FactoryFromParams, when set,
	// resolves the factory from the effective params instead
	// (equalizer → equalizer-Nbands).
	Factory           string
	FactoryFromParams func(map[string]any) (string, error)
	// Params is the closed set of accepted parameter names. ExtraParams,
	// when set, returns additional valid names derived from the effective
	// params (equalizer band gains g0..gN-1).
	Params      map[string]FilterParamDef
	ExtraParams func(Params map[string]any) []string
	// InPlace, when set, means every accepted param (after SkipProps) is a
	// settable property of the running element: a param-only change is
	// applied in place (UpdateLayerFilter) instead of a chain rebuild.
	// Param changes on non-InPlace filters still rebuild the layer.
	InPlace bool
	// SkipProps are params that are not element properties of the filter's
	// factory (videoscale width/height go to a trailing capsfilter,
	// equalizer bands resolve the factory). They are excluded from the
	// element's property set.
	SkipProps []string
	// TransformParams, when set, rewrites the effective params into the
	// element property values (videorotate stores angle in degrees, the
	// element expects radians).
	TransformParams func(Params map[string]any) map[string]any
	// Check, when set, validates cross-parameter invariants after the
	// per-param type checks (effective params, defaults applied).
	Check func(Params map[string]any) error
}

// filterRegistry is the supported filter set (see FILTERS.md).
var filterRegistry = map[string]FilterDef{
	// --- video filters -------------------------------------------------
	"videoscale": {
		Kind: "videoscale", Format: FormatVideo, Factory: "videoscale",
		// width/height land on a trailing capsfilter (the element only
		// scales to whatever caps it is given) — not element properties.
		SkipProps: []string{"width", "height"},
		Params: map[string]FilterParamDef{
			"width":  {Kind: ParamInt, Default: 0.0},
			"height": {Kind: ParamInt, Default: 0.0},
		},
		Check: func(p map[string]any) error {
			if effFloat(p, "width") <= 0 && effFloat(p, "height") <= 0 {
				return fmt.Errorf("videoscale: set width and/or height")
			}
			return nil
		},
	},
	"videocrop": {
		Kind: "videocrop", Format: FormatVideo, Factory: "videocrop",
		Params: map[string]FilterParamDef{
			"top": {Kind: ParamInt, Default: 0.0}, "bottom": {Kind: ParamInt, Default: 0.0},
			"left": {Kind: ParamInt, Default: 0.0}, "right": {Kind: ParamInt, Default: 0.0},
		},
	},
	"videobalance": {
		Kind: "videobalance", Format: FormatVideo, Factory: "videobalance",
		Params: map[string]FilterParamDef{
			"brightness": {Kind: ParamFloat, Default: 0.0},
			"contrast":   {Kind: ParamFloat, Default: 1.0},
			"hue":        {Kind: ParamFloat, Default: 0.0},
			"saturation": {Kind: ParamFloat, Default: 1.0},
		},
		Check: func(p map[string]any) error {
			for _, k := range []string{"brightness", "hue"} {
				if v := effFloat(p, k); v < -1.0 || v > 1.0 {
					return fmt.Errorf("videobalance: %s must be in [-1, 1], got %v", k, v)
				}
			}
			for _, k := range []string{"contrast", "saturation"} {
				if v := effFloat(p, k); v < 0 {
					return fmt.Errorf("videobalance: %s must be >= 0, got %v", k, v)
				}
			}
			return nil
		},
	},
	"videobox": {
		Kind: "videobox", Format: FormatVideo, Factory: "videobox",
		Params: map[string]FilterParamDef{
			"top": {Kind: ParamInt, Default: 0.0}, "bottom": {Kind: ParamInt, Default: 0.0},
			"left": {Kind: ParamInt, Default: 0.0}, "right": {Kind: ParamInt, Default: 0.0},
		},
	},
	"videoflip": {
		Kind: "videoflip", Format: FormatVideo, Factory: "videoflip",
		// The scene stores friendly direction names; the element property
		// is "video-direction", a GEnum (GStreamer 1.26+; the old "method"
		// enum is deprecated).
		Params: map[string]FilterParamDef{
			"direction": {Kind: ParamString, Default: "horizontal"},
		},
		TransformParams: func(p map[string]any) map[string]any {
			dir, _ := map[string]int64{
				"none": 0, "horizontal": 4, "vertical": 5,
			}[effStr(p, "direction")]
			return map[string]any{"video-direction": dir}
		},
		Check: func(p map[string]any) error {
			switch effStr(p, "direction") {
			case "none", "horizontal", "vertical":
				return nil
			default:
				return fmt.Errorf("videoflip: direction must be none|horizontal|vertical, got %q", effStr(p, "direction"))
			}
		},
	},
	"videorotate": {
		// There is no "videorotate" element upstream — the geometric
		// transform element is "rotate" (plugins-bad) and takes radians.
		Kind: "videorotate", Format: FormatVideo, Factory: "rotate",
		Params: map[string]FilterParamDef{
			"angle": {Kind: ParamFloat, Default: 0.0},
		},
		TransformParams: func(p map[string]any) map[string]any {
			out := make(map[string]any, len(p))
			for k, v := range p {
				out[k] = v
			}
			out["angle"] = effFloat(p, "angle") * math.Pi / 180.0
			return out
		},
	},
	"effectv": {
		Kind: "effectv", Format: FormatVideo, Factory: "effectv",
		Params: map[string]FilterParamDef{
			"aging-tv":    {Kind: ParamBool, Default: false},
			"edge-tv":     {Kind: ParamBool, Default: false},
			"dice-tv":     {Kind: ParamBool, Default: false},
			"vcr-tv":      {Kind: ParamBool, Default: false},
			"vhs-tv":      {Kind: ParamBool, Default: false},
			"rainbow-tv":  {Kind: ParamBool, Default: false},
			"shred-tv":    {Kind: ParamBool, Default: false},
			"mpeg1-tv":    {Kind: ParamBool, Default: false},
			"mpeg2-tv":    {Kind: ParamBool, Default: false},
			"scanline-tv": {Kind: ParamBool, Default: false},
		},
		Check: func(p map[string]any) error {
			for k := range p {
				// p holds only this filter's params (effective values).
				if effBool(p, k) {
					return nil
				}
			}
			return fmt.Errorf("effectv: enable at least one effect (e.g. edge-tv: true)")
		},
	},

	// --- audio filters -------------------------------------------------
	"volume": {
		Kind: "volume", Format: FormatAudio, Factory: "volume",
		// volume/mute are plain element properties: a live knob nudge must
		// not restart the layer's Chrome and reshuffle the mux pads.
		InPlace: true,
		Params: map[string]FilterParamDef{
			"volume": {Kind: ParamFloat, Default: 1.0},
			"mute":   {Kind: ParamBool, Default: false},
		},
	},
	"equalizer": {
		Kind:   "equalizer",
		Format: FormatAudio,
		// bands resolves the factory (equalizer-Nbands); it is not an
		// element property.
		SkipProps: []string{"bands"},
		Params: map[string]FilterParamDef{
			"bands": {Kind: ParamInt, Default: 3.0},
		},
		FactoryFromParams: func(p map[string]any) (string, error) {
			return fmt.Sprintf("equalizer-%dbands", effInt(p, "bands")), nil
		},
		// Band gains are element properties named band0..bandN-1
		// (GStreamer 1.26; range -24 to +12 dB).
		ExtraParams: func(p map[string]any) []string {
			n := effInt(p, "bands")
			out := make([]string, 0, n)
			for i := 0; i < n; i++ {
				out = append(out, fmt.Sprintf("band%d", i))
			}
			return out
		},
		Check: func(p map[string]any) error {
			if bands := effInt(p, "bands"); bands != 3 && bands != 4 && bands != 5 && bands != 10 {
				return fmt.Errorf("equalizer: bands must be 3, 4, 5 or 10, got %v", bands)
			}
			return nil
		},
	},
	"audiodynamic": {
		Kind: "audiodynamic", Format: FormatAudio, Factory: "audiodynamic",
		// mode and characteristics are GEnums (checked against GStreamer
		// 1.26); threshold is normalized (0..1), not decibels.
		Params: map[string]FilterParamDef{
			"mode":            {Kind: ParamInt, Default: 0.0},
			"characteristics": {Kind: ParamInt, Default: 0.0},
			"ratio":           {Kind: ParamFloat, Default: 1.0},
			"threshold":       {Kind: ParamFloat, Default: 0.0},
		},
		Check: func(p map[string]any) error {
			if mode := effInt(p, "mode"); mode != 0 && mode != 1 {
				return fmt.Errorf("audiodynamic: mode must be 0 (compressor) or 1 (expander), got %v", mode)
			}
			if ch := effInt(p, "characteristics"); ch != 0 && ch != 1 {
				return fmt.Errorf("audiodynamic: characteristics must be 0 (hard-knee) or 1 (soft-knee), got %v", ch)
			}
			if v := effFloat(p, "ratio"); v < 0 {
				return fmt.Errorf("audiodynamic: ratio must be >= 0, got %v", v)
			}
			if v := effFloat(p, "threshold"); v < 0 || v > 1 {
				return fmt.Errorf("audiodynamic: threshold must be in [0, 1], got %v", v)
			}
			return nil
		},
	},
	"pitch": {
		Kind: "pitch", Format: FormatAudio, Factory: "pitch",
		// The element exposes pitch (tone), tempo (speed) and rate (both);
		// each accepts 0.1..10 (verified GStreamer 1.26).
		Params: map[string]FilterParamDef{
			"pitch": {Kind: ParamFloat, Default: 1.0},
			"tempo": {Kind: ParamFloat, Default: 1.0},
			"rate":  {Kind: ParamFloat, Default: 1.0},
		},
		Check: func(p map[string]any) error {
			for _, k := range []string{"pitch", "tempo", "rate"} {
				if v := effFloat(p, k); v < 0.1 || v > 10 {
					return fmt.Errorf("pitch: %s must be in [0.1, 10], got %v", k, v)
				}
			}
			return nil
		},
	},
}

// LookupFilter returns the registry entry for kind.
func LookupFilter(kind string) (FilterDef, bool) {
	d, ok := filterRegistry[kind]
	return d, ok
}

// EffectiveParams merges the registry defaults with the scene params (the
// scene wins) and returns the closed set of accepted names, so callers can
// iterate deterministically.
func (d FilterDef) EffectiveParams(in map[string]any) map[string]any {
	out := make(map[string]any, len(d.Params))
	for k, def := range d.Params {
		out[k] = def.Default
	}
	for k, v := range in {
		if _, known := out[k]; known {
			out[k] = v
		}
	}
	// Param-derived names (equalizer band0..bandN-1) have no default; a
	// provided value is carried through.
	if d.ExtraParams != nil {
		for _, k := range d.ExtraParams(out) {
			if v, ok := in[k]; ok {
				out[k] = v
			}
		}
	}
	return out
}

// Typed accessors for effective param values. The per-param type checks
// (checkParamType) run before any Check/Transform sees the map, so the
// assertions here succeed by construction.
func effFloat(p map[string]any, k string) float64 { v, _ := p[k].(float64); return v }
func effInt(p map[string]any, k string) int       { return int(effFloat(p, k)) }
func effBool(p map[string]any, k string) bool     { v, _ := p[k].(bool); return v }
func effStr(p map[string]any, k string) string    { v, _ := p[k].(string); return v }

// FactoryFor resolves the element factory for the effective params
// (registry default, or the param-derived one — equalizer → equalizer-Nbands).
func (d FilterDef) FactoryFor(eff map[string]any) (string, error) {
	if d.FactoryFromParams != nil {
		return d.FactoryFromParams(eff)
	}
	return d.Factory, nil
}

// validateFilters enforces the filter invariants for one layer: known kinds,
// format compatibility with the layer type (only browser layers carry audio),
// known params of the right type, and the per-kind cross-param checks.
func validateFilters(l Layer) error {
	for j, f := range l.Filters {
		def, ok := filterRegistry[f.Kind]
		if !ok {
			return fmt.Errorf("scene: layer %q filter %d: unknown kind %q", l.ID, j, f.Kind)
		}
		if def.Format == FormatAudio && l.Type != LayerTypeBrowser {
			return fmt.Errorf("scene: layer %q filter %d: %q is an audio filter; only browser layers carry audio", l.ID, j, f.Kind)
		}

		// Per-param name + type checks (scene values over registry names).
		valid := map[string]bool{}
		for k, pd := range def.Params {
			if err := checkParamType(l.ID, j, f.Kind, k, pd.Kind, f.Params[k]); err == nil {
				valid[k] = true
			}
		}
		// Effective values (defaults applied) are computed once: the
		// equalizer resolves its band names from them, and the per-kind
		// Check validates on them.
		eff := def.EffectiveParams(f.Params)
		if def.ExtraParams != nil {
			for _, g := range def.ExtraParams(eff) {
				if _, set := f.Params[g]; set {
					if err := checkParamType(l.ID, j, f.Kind, g, ParamFloat, f.Params[g]); err != nil {
						return err
					}
					valid[g] = true
				}
			}
		}
		for k := range f.Params {
			if !valid[k] {
				return fmt.Errorf("scene: layer %q filter %d (%s): unknown param %q", l.ID, j, f.Kind, k)
			}
		}

		// Cross-param check on the effective values (defaults applied).
		if def.Check != nil {
			if err := def.Check(eff); err != nil {
				return fmt.Errorf("scene: layer %q filter %d: %w", l.ID, j, err)
			}
		}
	}
	return nil
}

// checkParamType validates one scene param value against its declared kind.
// Missing values are fine (the default applies). The location prefix is
// built only on error.
func checkParamType(id string, j int, kind, key string, pd FilterParamKind, v any) error {
	if v == nil {
		return nil
	}
	switch pd {
	case ParamFloat, ParamInt:
		n, ok := v.(float64)
		if !ok {
			return paramTypeErr(id, j, kind, key, "a number", v)
		}
		if pd == ParamInt && n != math.Trunc(n) {
			return paramTypeErr(id, j, kind, key, "an integer", n)
		}
	case ParamBool:
		if _, ok := v.(bool); !ok {
			return paramTypeErr(id, j, kind, key, "a boolean", v)
		}
	case ParamString:
		if _, ok := v.(string); !ok {
			return paramTypeErr(id, j, kind, key, "a string", v)
		}
	}
	return nil
}

func paramTypeErr(id string, j int, kind, key, want string, got any) error {
	return fmt.Errorf("scene: layer %q filter %d (%s) param %q: expected %s, got %T", id, j, kind, key, want, got)
}

// FiltersChanged reports whether two layers' filter lists differ (order and
// params), which forces a chain rebuild in the scene diff.
func FiltersChanged(a, b Layer) bool {
	if len(a.Filters) != len(b.Filters) {
		return true
	}
	for i := range a.Filters {
		if a.Filters[i].Kind != b.Filters[i].Kind {
			return true
		}
		if !paramsEqual(a.Filters[i].Params, b.Filters[i].Params) {
			return true
		}
	}
	return false
}

// paramsEqual compares two JSON-decoded param maps (float64/string/bool only,
// per the registry type checks).
func paramsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va != vb {
			return false
		}
	}
	return true
}
