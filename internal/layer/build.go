package layer

import (
	"fmt"

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
		// Integer values take the cgo g_object_setv path (a plain int/uint
		// GValue is silently dropped by the varargs g_object_set in this
		// build); everything else goes to the go-gst property setter.
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
// shared by the video/image/text builders.
func SurfaceChain(l scene.Layer, head ...gst.Element) (*Chain, error) {
	filters, err := VideoFilters(l)
	if err != nil {
		return nil, err
	}
	tailEls, err := Tail(l)
	if err != nil {
		return nil, err
	}
	els := append(append([]gst.Element{}, head...), filters...)
	// DynamicSourceIndex: -1 marks a static chain; the zero value 0 would
	// make LinkChain treat Elements[0] as a dynamic source and skip the
	// first link.
	return &Chain{Elements: append(els, tailEls...), DynamicSourceIndex: -1}, nil
}

// Tail builds the [videoconvert, videoscale, capsfilter(BGRA...), queue]
// tail of a layer chain.
func Tail(l scene.Layer) ([]gst.Element, error) {
	conv, err := MakeElement("videoconvert", l.ID+"-conv", nil)
	if err != nil {
		return nil, err
	}
	scale, err := MakeElement("videoscale", l.ID+"-scale", nil)
	if err != nil {
		return nil, err
	}
	cf, err := MakeElement("capsfilter", l.ID+"-caps", nil)
	if err != nil {
		return nil, err
	}
	cf.SetObjectProperty("caps", gst.CapsFromString(bgraCapsString(l)))
	q, err := MakeElement("queue", l.ID+"-queue", map[string]any{"max-size-buffers": 60})
	if err != nil {
		return nil, err
	}
	return []gst.Element{conv, scale, cf, q}, nil
}
