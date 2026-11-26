package layer

import (
	"math"
	"strings"
	"testing"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
	"ctrlirl-coder/internal/scene"
)

// requireFactories skips the test when the GStreamer plugins for the given
// factories are not installed (bare dev-header hosts); Docker/CI images carry
// the full plugin set. EnsureInit loads the plugin registry first (without it
// ElementFactoryFind sees nothing and the test would skip by accident).
func requireFactories(t *testing.T, names ...string) {
	t.Helper()
	gstutil.EnsureInit()
	for _, n := range names {
		if gst.ElementFactoryFind(n) == nil {
			t.Skipf("gstreamer factory %q not available on this host", n)
		}
	}
}

func TestBuildFiltersVideo(t *testing.T) {
	// "rotate" is the element behind the "videorotate" kind.
	requireFactories(t, "videocrop", "videobalance", "videobox", "videoflip", "rotate", "videoscale", "capsfilter")
	l := scene.Layer{
		ID:   "cam",
		Type: "video",
		Filters: []scene.FilterSpec{
			{Kind: "videocrop", Params: map[string]any{"top": 10.0}},
			{Kind: "videobalance", Params: map[string]any{"brightness": 0.1}},
			{Kind: "videobox"},
			{Kind: "videoflip", Params: map[string]any{"direction": "vertical"}},
			{Kind: "videorotate", Params: map[string]any{"angle": 90.0}},
			{Kind: "videoscale", Params: map[string]any{"width": 640.0, "height": 360.0}},
		},
	}
	els, err := VideoFilters(l)
	if err != nil {
		t.Fatalf("VideoFilters: %v", err)
	}
	// Preconvert + 5 single elements + rotate's trailing alpha capsfilter
	// + postconvert + videoscale (element + capsfilter) = 10.
	if len(els) != 10 {
		t.Fatalf("video filter elements = %d, want 10", len(els))
	}
	// Element names: <id>-fNN-<kind>, with the leading preconvert named
	// <id>-fconv and the scale capsfilter <id>-fNN-scalecaps. NN counts
	// element positions (a videoscale's capsfilter consumes one).
	wantSuffixes := []string{
		"fconv", "f00-videocrop", "f01-videobalance", "f02-videobox",
		"f03-videoflip", "f04-videorotate", "f05-alpha", "f06-postconv",
		"f07-videoscale", "f08-scalecaps",
	}
	for i, el := range els {
		name := el.GetName()
		if name == "" {
			t.Fatalf("element %d has no name", i)
		}
		if !strings.HasPrefix(name, "cam-f") || !strings.Contains(name, wantSuffixes[i]) {
			t.Fatalf("element %d name = %q, want cam-fNN-%s", i, name, wantSuffixes[i])
		}
	}
	// The spec stores degrees; the element property must be radians.
	af, ok := els[5].ObjectProperty("angle").(float64)
	if !ok || math.Abs(af-math.Pi/2) > 1e-6 {
		t.Errorf("rotate angle = %v, want pi/2 rad (90 degrees converted)", els[5].ObjectProperty("angle"))
	}
	// The alpha capsfilter right after rotate must pin an alpha-carrying
	// format (BGRA) so the un-rotated corners stay transparent.
	if caps := gstutil.CapsString(els[6], "caps"); !strings.Contains(caps, "BGRA") {
		t.Errorf("rotate alpha capsfilter = %q, want to pin a BGRA (alpha) format", caps)
	}
}

// TestBuildFiltersEffectv covers the effectv kind separately: on stock
// Debian (bookworm/trixie) the effectv plugin does not register in the
// GStreamer registry, so the factory is missing and this test skips. On
// hosts where the registry serves effectv it must build a boolean-property
// element.
func TestBuildFiltersEffectv(t *testing.T) {
	requireFactories(t, "effectv")
	els, err := VideoFilters(scene.Layer{
		ID:   "cam",
		Type: "video",
		Filters: []scene.FilterSpec{
			{Kind: "effectv", Params: map[string]any{"edge-tv": true}},
		},
	})
	if err != nil {
		t.Fatalf("VideoFilters: %v", err)
	}
	if len(els) != 1 {
		t.Fatalf("effectv elements = %d, want 1", len(els))
	}
}

func TestBuildFiltersAudio(t *testing.T) {
	requireFactories(t, "volume", "equalizer-3bands", "equalizer-10bands", "audiodynamic", "pitch")
	l := scene.Layer{
		ID:   "web",
		Type: "browser",
		Filters: []scene.FilterSpec{
			{Kind: "volume", Params: map[string]any{"volume": 0.5}},
			{Kind: "equalizer", Params: map[string]any{"bands": 10.0, "band0": 2.0}},
			{Kind: "audiodynamic", Params: map[string]any{"mode": 0}},
			{Kind: "pitch", Params: map[string]any{"tempo": 1.25}},
		},
	}
	els, err := AudioFilters(l)
	if err != nil {
		t.Fatalf("AudioFilters: %v", err)
	}
	if len(els) != 4 {
		t.Fatalf("audio filter elements = %d, want 4", len(els))
	}
	if got := els[1].GetName(); got == "" {
		t.Fatal("equalizer element unnamed")
	}
	// Video filters of a browser layer must not leak into the audio chain.
	l.Filters = append(l.Filters, scene.FilterSpec{Kind: "videocrop"})
	els2, err := AudioFilters(l)
	if err != nil {
		t.Fatalf("AudioFilters with video filter present: %v", err)
	}
	if len(els2) != 4 {
		t.Fatalf("audio chain picked up video filters: %d elements, want 4", len(els2))
	}
}

func TestBuildFiltersNone(t *testing.T) {
	l := scene.Layer{ID: "cam", Type: "video"}
	v, err := VideoFilters(l)
	if err != nil {
		t.Fatalf("VideoFilters: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("no filters should yield no elements, got %d", len(v))
	}
	a, err := AudioFilters(l)
	if err != nil {
		t.Fatalf("AudioFilters: %v", err)
	}
	if len(a) != 0 {
		t.Fatalf("no filters should yield no elements, got %d", len(a))
	}
}
