package image

import (
	"path/filepath"
	"strings"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/layer"
	"ctrlirl-coder/internal/scene"
)

// ImageOverlay is a static image layer: filesrc → (pngparse|jpegparse) →
// (pngdec|jpegdec) → videoconvert → capsfilter(BGRA) → queue feeding the
// compositor. The explicit parse+dec pair (chosen by file extension) replaces
// decodebin — decodebin instantiates its src pad lazily, so a pre-preroll
// Element.Link fails, and the image-decode factory is not in this build.
type ImageOverlay struct {
	layer.BaseLayer
}

// NewImageOverlay builds an image layer.
func NewImageOverlay(l scene.Layer) *ImageOverlay {
	return &ImageOverlay{layer.NewBaseLayer(l, scene.LayerTypeImage)}
}

// BuildChain links filesrc → (png|jpeg)parse → (png|jpeg)dec →
// videoconvert → capsfilter(BGRA) → queue.
func (i *ImageOverlay) BuildChain() (*layer.Chain, error) {
	head := make([]gst.Element, 0, 3)
	fs, err := layer.MakeElement("filesrc", i.L.ID+"-filesrc", map[string]any{"location": i.L.Source})
	if err != nil {
		return nil, err
	}
	head = append(head, fs)
	for _, kind := range imageDecodeKinds(i.L.Source) {
		el, err := layer.MakeElement(kind, i.L.ID+"-"+kind, nil)
		if err != nil {
			return nil, err
		}
		head = append(head, el)
	}
	// Returned unlinked; the compositor adds the elements to the pipeline
	// and then links the chain (GStreamer 1.28's gst_bin_add breaks links
	// made before insertion).
	return layer.SurfaceChain(i.L, head...)
}

// imageDecodeKinds returns the [parse, dec] element factory names for the
// file's image format (by extension).
func imageDecodeKinds(path string) []string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return []string{"jpegparse", "jpegdec"}
	default:
		return []string{"pngparse", "pngdec"}
	}
}
