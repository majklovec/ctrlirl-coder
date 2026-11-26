package text

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/layer"
	"ctrlirl-coder/internal/scene"
)

// textSurfaceW/H size the text layer's transparent surface. The compositor
// scales the surface to the pad's width/height, so this is the render
// resolution of the text (the pango raster happens at this size before the
// compositor scales the layer).
const (
	textSurfaceW = 1280
	textSurfaceH = 720
)

// textSurfaceFPS is the transparent source frame rate.
const (
	textSurfaceFPS = 30
)

// TextOverlay is a text layer rendered by the pango `textoverlay` element
// (GstBaseTextOverlay, no Go-side PNG pre-rendering). The element draws the
// text into the video flowing through it, so each text layer feeds it a
// fully transparent BGRA surface (a live videotestsrc with alpha=0); the
// textoverlay paints the text — in the configured color, font,
// outline/shading — into those transparent frames, and the result rides the
// compositor as a normal alpha surface.
//
// The scene's "text-options" map is mapped onto the element's properties
// (font-desc, color, outline-color, shaded-background, halignment, ... —
// see parseTextOptions).
//
// Chain: videotestsrc → alpha → textoverlay → videoconvert →
// capsfilter(BGRA) → queue.
type TextOverlay struct {
	layer.BaseLayer
}

// NewTextOverlay builds a text layer.
func NewTextOverlay(l scene.Layer) *TextOverlay {
	return &TextOverlay{layer.NewBaseLayer(l, scene.LayerTypeText)}
}

// BuildChain creates the transparent surface and textoverlay and returns the
// linked-in-order chain (unlinked; see Layer.BuildChain).
func (t *TextOverlay) BuildChain() (*layer.Chain, error) {
	props, err := parseTextOptions(t.L.TextOptions)
	if err != nil {
		return nil, fmt.Errorf("layer: text options for %q: %w", t.L.ID, err)
	}
	if _, has := props["text"]; !has {
		props["text"] = t.L.Text
	}

	surface, err := layer.MakeElement("videotestsrc", t.L.ID+"-surface", map[string]any{
		"is-live": true,
		"pattern": int32(2),
	})
	if err != nil {
		return nil, err
	}
	surfaceCaps, err := layer.MakeElement("capsfilter", t.L.ID+"-surface-caps", nil)
	if err != nil {
		return nil, err
	}
	surfaceCaps.SetObjectProperty("caps", gst.CapsFromString(
		fmt.Sprintf("video/x-raw,format=BGRA,width=%d,height=%d,framerate=%d/1", textSurfaceW, textSurfaceH, textSurfaceFPS)))
	alpha, err := layer.MakeElement("alpha", t.L.ID+"-alpha", map[string]any{
		"method": int32(0),
		"alpha":  0.0,
	})
	if err != nil {
		return nil, err
	}

	to, err := layer.MakeElement("textoverlay", t.L.ID+"-text", props)
	if err != nil {
		return nil, err
	}

	slog.Info("layer: text overlay", "id", t.L.ID, "text", t.L.Text, "options", props)
	return layer.SurfaceChain(t.L, surface, surfaceCaps, alpha, to)
}

// DefaultProperties returns the scene properties. width/height default to 0
// (native surface size) so the compositor does not scale the text surface
// unless the scene says so.
func (t *TextOverlay) DefaultProperties() map[string]float64 {
	props := layer.DefaultProps(t.L)
	for _, k := range []string{"width", "height"} {
		if _, ok := t.L.Properties[k]; !ok {
			props[k] = 0
		}
	}
	return props
}

// ---------------------------------------------------------------------------
// Scene text-options → element properties
// ---------------------------------------------------------------------------

// textOptions maps a scene layer's "text-options" entries (all strings in the
// scene JSON) onto GStreamer `textoverlay` property values of the right Go
// type for the go-glib setter:
//
//   - font-desc               → string (Pango font description)
//   - color, outline-color    → uint32 (big-endian ARGB, "#AARRGGBB" accepted)
//   - shading-value, xpad,
//     ypad                    → uint32 (unsigned)
//   - deltax, deltay          → int32 (signed)
//   - draw-outline, draw-shadow,
//     shaded-background,
//     auto-resize             → bool
//   - halignment, valignment,
//     line-alignment,
//     wrap-mode, scale-mode   → int32 (GEnum, name or number accepted)
//
// The scene uses these names on purpose so that a scene reads like the
// element's property list (gst-inspect-1.0 textoverlay).
var textOptions = map[string]textOptKind{
	"font-desc": textOptString{},

	"color":         textOptARGB{},
	"outline-color": textOptARGB{},

	"shading-value": textOptUInt{},
	"xpad":          textOptUInt{},
	"ypad":          textOptUInt{},

	"deltax": textOptInt{},
	"deltay": textOptInt{},

	"auto-resize":       textOptBool{},
	"draw-outline":      textOptBool{},
	"draw-shadow":       textOptBool{},
	"shaded-background": textOptBool{},

	"halignment":     textOptEnum{halignValues},
	"valignment":     textOptEnum{valignValues},
	"line-alignment": textOptEnum{lineAlignValues},
	"wrap-mode":      textOptEnum{wrapModeValues},
	"scale-mode":     textOptEnum{scaleModeValues},
}

type textOptKind interface {
	convert(value string) (any, error)
}

type textOptString struct{}

func (textOptString) convert(v string) (any, error) { return v, nil }

type textOptBool struct{}

func (textOptBool) convert(v string) (any, error) {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return nil, fmt.Errorf("not a bool: %q", v)
	}
	return b, nil
}

type textOptInt struct{}

func (textOptInt) convert(v string) (any, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 0, 32)
	if err != nil {
		return nil, fmt.Errorf("not an integer: %q", v)
	}
	return int32(n), nil
}

type textOptUInt struct{}

func (textOptUInt) convert(v string) (any, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(v), 0, 32)
	if err != nil {
		return nil, fmt.Errorf("not an unsigned integer: %q", v)
	}
	return uint32(n), nil
}

// textOptARGB accepts a big-endian ARGB color as "#AARRGGBB", "AARRGGBB",
// "0xAARRGGBB" (or a decimal), and a bare "#RRGGBB"/"RRGGBB" (alpha forced
// to opaque).
type textOptARGB struct{}

func (textOptARGB) convert(v string) (any, error) {
	s := strings.TrimPrefix(strings.TrimSpace(v), "#")
	if len(s) == 6 { // RRGGBB → opaque ARGB
		s = "FF" + s
	}
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		s = "0x" + s
	}
	n, err := strconv.ParseUint(s, 0, 32)
	if err != nil {
		return nil, fmt.Errorf("not an ARGB color: %q", v)
	}
	return uint32(n), nil
}

type textOptEnum struct {
	names map[string]int
}

func (e textOptEnum) convert(v string) (any, error) {
	s := strings.TrimSpace(v)
	if n, err := strconv.ParseInt(s, 0, 32); err == nil {
		return int32(n), nil
	}
	if n, ok := e.names[strings.ToLower(s)]; ok {
		return int32(n), nil
	}
	return nil, fmt.Errorf("unknown enum value %q (names: %s)", v, enumNames(e.names))
}

func enumNames(m map[string]int) string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	return strings.Join(names, "|")
}

// GstBaseTextOverlayHAlign.
var halignValues = map[string]int{"left": 0, "center": 1, "right": 2, "position": 4, "absolute": 5}

// GstBaseTextOverlayVAlign.
var valignValues = map[string]int{"baseline": 0, "bottom": 1, "top": 2, "position": 3, "center": 4, "absolute": 5}

// GstBaseTextOverlayLineAlign.
var lineAlignValues = map[string]int{"left": 0, "center": 1, "right": 2}

// GstBaseTextOverlayWrapMode.
var wrapModeValues = map[string]int{"none": -1, "word": 0, "char": 1, "wordchar": 2}

// GstBaseTextOverlayScaleMode.
var scaleModeValues = map[string]int{"none": 0, "par": 1, "display": 2, "user": 3}

// parseTextOptions converts a scene text layer's "text-options" map into
// element properties. Unknown keys are rejected so a typo in the scene fails
// the layer attach instead of silently doing nothing.
func parseTextOptions(opts map[string]string) (map[string]any, error) {
	out := make(map[string]any, len(opts))
	for k, v := range opts {
		conv, ok := textOptions[k]
		if !ok {
			return nil, fmt.Errorf("unknown text option %q (known: %s)", k, knownTextOptions())
		}
		val, err := conv.convert(v)
		if err != nil {
			return nil, fmt.Errorf("text option %q: %w", k, err)
		}
		out[k] = val
	}
	return out, nil
}

func knownTextOptions() string {
	names := make([]string, 0, len(textOptions))
	for k := range textOptions {
		names = append(names, k)
	}
	return strings.Join(names, " ")
}
