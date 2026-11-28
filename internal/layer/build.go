package layer

import (
	"fmt"
	"slices"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
	"ctrlirl-coder/internal/scene"
)

// MakeElement builds an element factory-produced element with the given name
// and optional initial properties. Integer properties go through the cgo
// g_object_setv path (gstutil.SetNumericProperty): in this GStreamer build
// the varargs g_object_set used by the go-gst binding silently drops int
// GValues, so they would be no-ops (encoder bitrate, queue max sizes, ...).
func MakeElement(kind, name string, props map[string]any) (gst.Element, error) {
	el := gst.ElementFactoryMake(kind, name)
	if el == nil {
		return nil, fmt.Errorf("layer: %s element factory not available", kind)
	}
	var rest map[string]any
	for k, v := range props {
		var n int64
		var isNum bool
		switch val := v.(type) {
		case int:
			n, isNum = int64(val), true
		case int64:
			n, isNum = val, true
		case uint:
			n, isNum = int64(val), true
		case uint32:
			n, isNum = int64(val), true
		case uint64:
			n, isNum = int64(val), true
		}
		if isNum {
			if err := gstutil.SetNumericProperty(el, k, n); err != nil {
				return nil, fmt.Errorf("layer: %s element: %w", kind, err)
			}
			continue
		}
		if rest == nil {
			rest = make(map[string]any, len(props))
		}
		rest[k] = v
	}
	if len(rest) > 0 {
		el.SetObjectProperties(rest)
	}
	return el, nil
}

// Spec describes one element to build.
type Spec struct {
	Factory string         // element factory ("videoconvert", "queue", ...)
	Name    string         // element name, unique in the pipeline
	Props   map[string]any // initial properties (see MakeElement), may be nil
	Caps    string         // when set: the element's "caps" property (capsfilter, appsrc)
}

// Build makes the elements described by specs, in order. It stops at the
// first element that cannot be built.
func Build(specs ...Spec) ([]gst.Element, error) {
	els := make([]gst.Element, 0, len(specs))
	for _, sp := range specs {
		el, err := MakeElement(sp.Factory, sp.Name, sp.Props)
		if err != nil {
			return nil, err
		}
		if sp.Caps != "" {
			el.SetObjectProperty("caps", gst.CapsFromString(sp.Caps))
		}
		els = append(els, el)
	}
	return els, nil
}

// IntProp reads a scene property as a positive int, falling back to def
// when it is absent or not positive.
func IntProp(l scene.Layer, key string, def int) int {
	if v, ok := l.Properties[key]; ok && v > 0 {
		return int(v)
	}
	return def
}

// bgraCapsString builds the output caps for a layer, pinning width/height
// when the scene provides both.
func bgraCapsString(l scene.Layer) string {
	w, wOK := l.Properties["width"]
	h, hOK := l.Properties["height"]
	if wOK && hOK && w > 0 && h > 0 {
		return fmt.Sprintf("video/x-raw,format=BGRA,width=%d,height=%d", int(w), int(h))
	}
	return "video/x-raw,format=BGRA"
}

// SurfaceChain builds head + video filters + BGRA tail — the chain shape
// every layer's video chain has. The chain is static; a layer with a
// runtime-pad source sets DynamicSourceIndex on the result.
func SurfaceChain(l scene.Layer, head ...gst.Element) (*Chain, error) {
	filters, err := VideoFilters(l)
	if err != nil {
		return nil, err
	}
	tail, err := Tail(l)
	if err != nil {
		return nil, err
	}
	return StaticChain(slices.Concat(head, filters, tail)...), nil
}

// StaticChain is a chain without a runtime-pad source. (The -1 matters: the
// zero value of DynamicSourceIndex would make LinkChain treat Elements[0]
// as a dynamic source and skip the first link.)
func StaticChain(els ...gst.Element) *Chain {
	return &Chain{Elements: els, DynamicSourceIndex: -1}
}

// Tail builds the [videoconvert, videoscale, capsfilter(BGRA...), queue]
// tail of a layer chain.
func Tail(l scene.Layer) ([]gst.Element, error) {
	return Build(
		Spec{Factory: "videoconvert", Name: l.ID + "-conv"},
		Spec{Factory: "videoscale", Name: l.ID + "-scale"},
		Spec{Factory: "capsfilter", Name: l.ID + "-caps", Caps: bgraCapsString(l)},
		Spec{Factory: "queue", Name: l.ID + "-queue", Props: map[string]any{"max-size-buffers": 60}},
	)
}
