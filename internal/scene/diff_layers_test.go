package scene

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func browserScene(volume, alpha float64) *Scene {
	return &Scene{Layers: []Layer{{
		ID: "alerts", Type: LayerTypeBrowser, Source: "https://example.org",
		Properties: map[string]float64{"zorder": 1, "alpha": alpha, "width": 1280, "height": 720},
		Filters:    []FilterSpec{{Kind: "volume", Params: map[string]any{"volume": volume}}},
	}}}
}

// One edit can change a filter level AND a pad property (a scene switch that
// hides a browser layer and turns it down). Both must reach the pipeline: the
// diff used to emit only the filter update and silently drop the pad change,
// leaving the pipeline out of step with the scene.
func TestDiffEmitsFilterAndPropsUpdates(t *testing.T) {
	ops := Diff(browserScene(1.0, 1), browserScene(0.5, 0))
	kinds := make([]string, len(ops))
	for i, op := range ops {
		kinds[i] = op.Kind.String()
	}
	if got := strings.Join(kinds, ","); got != "update,update-filter" {
		t.Fatalf("ops = %s, want update,update-filter (pad properties and filter level)", got)
	}
	if ops[0].Layer.Properties["alpha"] != 0 {
		t.Errorf("props op carries alpha %v, want 0", ops[0].Layer.Properties["alpha"])
	}
	if !ops[1].Layer.Filters[0].inPlaceChanged {
		t.Error("filter op does not flag the changed slot")
	}
}

func TestDiffClassifiesEachKindOfChange(t *testing.T) {
	base := func() Layer {
		return Layer{
			ID: "l", Type: LayerTypeBrowser, Source: "https://example.org", Text: "",
			Properties: map[string]float64{"zorder": 1, "xpos": 0, "width": 1280, "height": 720, "framerate": 30},
			Filters: []FilterSpec{
				{Kind: "volume", Params: map[string]any{"volume": 1.0}},
			},
		}
	}
	for name, tc := range map[string]struct {
		edit func(*Layer)
		want string
	}{
		"nothing":                 {func(*Layer) {}, ""},
		"pad position":            {func(l *Layer) { l.Properties["xpos"] = 10 }, "update"},
		"in-place filter param":   {func(l *Layer) { l.Filters[0].Params["volume"] = 0.2 }, "update-filter"},
		"source":                  {func(l *Layer) { l.Source = "https://example.com" }, "remove,add"},
		"browser capture size":    {func(l *Layer) { l.Properties["width"] = 640 }, "remove,add"},
		"browser framerate":       {func(l *Layer) { l.Properties["framerate"] = 60 }, "remove,add"},
		"filter added":            {func(l *Layer) { l.Filters = append(l.Filters, FilterSpec{Kind: "pitch"}) }, "remove,add"},
		"filter kind":             {func(l *Layer) { l.Filters[0].Kind = "pitch" }, "remove,add"},
		"text options":            {func(l *Layer) { l.TextOptions = map[string]string{"color": "#fff"} }, "remove,add"},
		"empty text options":      {func(l *Layer) { l.TextOptions = map[string]string{} }, ""},
		"rebuild wins over props": {func(l *Layer) { l.Source = "x"; l.Properties["xpos"] = 5 }, "remove,add"},
	} {
		from, to := base(), base()
		tc.edit(&to)
		ops := Diff(&Scene{Layers: []Layer{from}}, &Scene{Layers: []Layer{to}})
		kinds := make([]string, len(ops))
		for i, op := range ops {
			kinds[i] = op.Kind.String()
		}
		if got := strings.Join(kinds, ","); got != tc.want {
			t.Errorf("%s: ops = %q, want %q", name, got, tc.want)
		}
	}

	// A param change on a filter that is not in-place capable rebuilds, and
	// so does an in-place one sharing the list with such a filter (a
	// rollback re-applies every slot).
	mixed := func(volume, pitch float64) *Scene {
		l := base()
		l.Filters = []FilterSpec{
			{Kind: "volume", Params: map[string]any{"volume": volume}},
			{Kind: "pitch", Params: map[string]any{"pitch": pitch}},
		}
		return &Scene{Layers: []Layer{l}}
	}
	for name, to := range map[string]*Scene{"pitch param": mixed(1, 1.5), "volume next to pitch": mixed(0.5, 1)} {
		if ops := Diff(mixed(1, 1), to); len(ops) != 2 || ops[0].Kind != OpRemove || ops[1].Kind != OpAdd {
			t.Errorf("%s: ops = %v, want remove+add", name, ops)
		}
	}
}

// A param of the wrong type is reported as such, not as an unknown param.
func TestFilterParamTypeErrorNamesTheType(t *testing.T) {
	_, err := Parse([]byte(`{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1},
	   "filters":[{"kind":"videoflip","params":{"direction":42}}]}
	]}`))
	if err == nil || !strings.Contains(err.Error(), "expected a string") {
		t.Fatalf("err = %v, want a type error (expected a string)", err)
	}
	_, err = Parse([]byte(`{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1},
	   "filters":[{"kind":"videocrop","params":{"top":1.5}}]}
	]}`))
	if err == nil || !strings.Contains(err.Error(), "expected an integer") {
		t.Fatalf("err = %v, want a type error (expected an integer)", err)
	}
}

// batchApplier counts ops and stream freezes.
type batchApplier struct {
	ops, suspends, resumes int
	suspendErr             error
}

func (b *batchApplier) AddLayer(context.Context, Layer) error     { b.ops++; return nil }
func (b *batchApplier) RemoveLayer(context.Context, string) error { b.ops++; return nil }
func (b *batchApplier) SuspendStream(context.Context) error       { b.suspends++; return b.suspendErr }
func (b *batchApplier) ResumeStream(context.Context) error        { b.resumes++; return nil }
func (b *batchApplier) UpdateLayerProps(context.Context, string, map[string]float64) error {
	b.ops++
	return nil
}
func (b *batchApplier) UpdateLayerFilter(context.Context, string, int, FilterSpec) error {
	b.ops++
	return nil
}

// The stream is frozen only for batches that attach or detach more than one
// chain. In-place updates — however many — must not stall it.
func TestApplyDiffSuspendsOnlyStructuralBatches(t *testing.T) {
	vol := Layer{ID: "l", Filters: []FilterSpec{{Kind: "volume", inPlaceChanged: true}}}
	for name, tc := range map[string]struct {
		ops     []Op
		suspend int
	}{
		"pad + filter update":  {[]Op{{Kind: OpUpdateProps, ID: "l"}, {Kind: OpUpdateFilter, ID: "l", Layer: vol}}, 0},
		"three pad updates":    {[]Op{{Kind: OpUpdateProps, ID: "a"}, {Kind: OpUpdateProps, ID: "b"}, {Kind: OpUpdateProps, ID: "c"}}, 0},
		"single add":           {[]Op{{Kind: OpAdd, ID: "a"}}, 0},
		"update + add":         {[]Op{{Kind: OpUpdateProps, ID: "a"}, {Kind: OpAdd, ID: "b"}}, 0},
		"rebuild (remove+add)": {[]Op{{Kind: OpRemove, ID: "a"}, {Kind: OpAdd, ID: "a"}}, 1},
		"two adds":             {[]Op{{Kind: OpAdd, ID: "a"}, {Kind: OpAdd, ID: "b"}}, 1},
	} {
		a := &batchApplier{}
		if err := ApplyDiff(context.Background(), a, tc.ops); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if a.suspends != tc.suspend || a.resumes != tc.suspend {
			t.Errorf("%s: %d suspends / %d resumes, want %d each", name, a.suspends, a.resumes, tc.suspend)
		}
		if a.ops != len(tc.ops) {
			t.Errorf("%s: %d ops applied, want %d", name, a.ops, len(tc.ops))
		}
	}

	failing := &batchApplier{suspendErr: errors.New("stopped")}
	err := ApplyDiff(context.Background(), failing, []Op{{Kind: OpRemove, ID: "a"}, {Kind: OpAdd, ID: "a"}})
	if err == nil || failing.ops != 0 || failing.resumes != 0 {
		t.Errorf("failed suspend: err=%v ops=%d resumes=%d, want an error and nothing applied or resumed", err, failing.ops, failing.resumes)
	}
}
