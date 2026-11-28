package image

import (
	"path/filepath"
	"strings"

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
	specs := []layer.Spec{{
		Factory: "filesrc", Name: i.L.ID + "-filesrc",
		Props: map[string]any{"location": i.L.Source},
	}}
	for _, kind := range imageDecodeKinds(i.L.Source) {
		specs = append(specs, layer.Spec{Factory: kind, Name: i.L.ID + "-" + kind})
	}
	head, err := layer.Build(specs...)
	if err != nil {
		return nil, err
	}
	return layer.SurfaceChain(i.L, head...)
}

func imageDecodeKinds(path string) []string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return []string{"jpegparse", "jpegdec"}
	default:
		return []string{"pngparse", "pngdec"}
	}
}
