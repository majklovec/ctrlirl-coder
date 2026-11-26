package scene

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func mustParse(t *testing.T, s string) *Scene {
	t.Helper()
	out, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return out
}

func TestParseAndDefaults(t *testing.T) {
	s := mustParse(t, `{
	  "name":"main",
	  "canvas":{"width":1280,"height":720},
	  "layers":[
	    {"id":"camera","type":"video","source":"v4l2src device=/dev/video0",
	     "properties":{"zorder":1,"xpos":0,"ypos":0,"width":1280,"height":720,"alpha":1.0}},
	    {"id":"logo","type":"image","source":"file:///opt/logo.png",
	     "properties":{"zorder":10,"xpos":1080,"ypos":0,"width":200,"height":100,"alpha":0.85}}
	  ]
	}`)
	if s.Name != "main" || s.Canvas.Width != 1280 || s.Canvas.Height != 720 {
		t.Fatalf("canvas/name = %+v", s)
	}
	if len(s.Layers) != 2 {
		t.Fatalf("layers = %d", len(s.Layers))
	}
}

func TestParseTestCodeLayer(t *testing.T) {
	s := mustParse(t, `{"layers":[
	  {"id":"timestamp","type":"testcode","source":"",
	   "properties":{"width":1280,"height":720}}
	]}`)
	if got := s.Layers[0].Type; got != LayerTypeTestCode {
		t.Fatalf("layer type = %q, want %q", got, LayerTypeTestCode)
	}
}

func TestNormalizeZorder(t *testing.T) {
	// Duplicates get renumbered to the next free value; missing zorder is derived from index.
	s := mustParse(t, `{
	  "layers":[
	    {"id":"a","type":"image","source":"f://a","properties":{"zorder":1}},
	    {"id":"b","type":"image","source":"f://b","properties":{"zorder":1}},
	    {"id":"c","type":"image","source":"f://c"}
	  ]
	}`)
	z := []float64{s.Layers[0].Properties["zorder"], s.Layers[1].Properties["zorder"], s.Layers[2].Properties["zorder"]}
	// a=1, b bumped to 2, c (index 2 -> 3) stays 3 (free).
	if z[0] != 1 || z[1] != 2 || z[2] != 3 {
		t.Fatalf("zorder after normalize = %v, want [1 2 3]", z)
	}
}

func TestValidateDuplicateID(t *testing.T) {
	if _, err := Parse([]byte(`{"layers":[
		{"id":"a","type":"image","source":"f://a","properties":{"zorder":1}},
		{"id":"a","type":"image","source":"f://b","properties":{"zorder":2}}
	]}`)); err == nil {
		t.Fatalf("expected duplicate id error")
	}
}

func TestValidateUnknownType(t *testing.T) {
	if _, err := Parse([]byte(`{"layers":[
		{"id":"a","type":"hologram","source":"x","properties":{"zorder":1}}
	]}`)); err == nil {
		t.Fatalf("expected unknown type error")
	}
}

func TestValidateCanvasDefault(t *testing.T) {
	// A scene without an explicit canvas gets the default canvas size.
	s := mustParse(t, `{"layers":[
	  {"id":"a","type":"video","source":"cam","properties":{"zorder":1}}
	]}`)
	if s.Canvas.Width != 1280 || s.Canvas.Height != 720 {
		t.Fatalf("default canvas = %+v, want 1280x720", s.Canvas)
	}
}

func TestValidateGeometryOffsetTextLayer(t *testing.T) {
	// The test-obs regression: an unsized text layer at ypos=20 makes the
	// compositor canvas the union (1280x740), which dead-ends caps
	// negotiation against the fixed trunk capsfilter → zero frames, black
	// stream, no GStreamer warnings. Must be rejected at parse time.
	if _, err := Parse([]byte(`{
	  "canvas":{"width":1280,"height":720},
	  "layers":[
	    {"id":"title","type":"text","text":"hi",
	     "properties":{"zorder":1,"xpos":0,"ypos":20}}
	  ]
	}`)); err == nil {
		t.Fatalf("expected offset unsized text layer to fail validation")
	}
}

func TestValidateGeometryRectExceedsCanvas(t *testing.T) {
	if _, err := Parse([]byte(`{
	  "canvas":{"width":1280,"height":720},
	  "layers":[
	    {"id":"pip","type":"video","source":"cam",
	     "properties":{"zorder":1,"xpos":1000,"ypos":0,"width":400,"height":360}}
	  ]
	}`)); err == nil {
		t.Fatalf("expected out-of-canvas rect to fail validation")
	}
}

func TestValidateGeometryNegativeProps(t *testing.T) {
	for _, props := range []string{
		`{"zorder":1,"xpos":-1}`,
		`{"zorder":1,"ypos":-5}`,
		`{"zorder":1,"width":-10}`,
		`{"zorder":1,"height":-1}`,
	} {
		if _, err := Parse([]byte(`{"layers":[
		  {"id":"a","type":"video","source":"cam","properties":` + props + `}
		]}`)); err == nil {
			t.Fatalf("expected %s to fail validation", props)
		}
	}
}

func TestValidateGeometryExactFitOK(t *testing.T) {
	// A layer whose rect ends exactly at the canvas edge is legal
	// (union == canvas, trunk capsfilter matches).
	mustParse(t, `{"layers":[
	  {"id":"a","type":"video","source":"cam","properties":{"zorder":1,"xpos":0,"ypos":0,"width":1280,"height":720}}
	]}`)
}

// fakeApplier records the operations it receives, in order.
type fakeApplier struct {
	ops []string
}

func (f *fakeApplier) AddLayer(_ context.Context, l Layer) error {
	f.ops = append(f.ops, "add:"+l.ID)
	return nil
}
func (f *fakeApplier) RemoveLayer(_ context.Context, id string) error {
	f.ops = append(f.ops, "remove:"+id)
	return nil
}
func (f *fakeApplier) UpdateLayerProps(_ context.Context, id string, _ map[string]float64) error {
	f.ops = append(f.ops, "update:"+id)
	return nil
}
func (f *fakeApplier) UpdateLayerFilter(_ context.Context, id string, index int, _ FilterSpec) error {
	f.ops = append(f.ops, fmt.Sprintf("update-filter:%s:%d", id, index))
	return nil
}

func TestDiffAddRemoveUpdate(t *testing.T) {
	from := mustParse(t, `{
	  "layers":[
	    {"id":"keep","type":"video","source":"cam","properties":{"zorder":1}},
	    {"id":"move","type":"image","source":"f://x","properties":{"zorder":2,"xpos":0}},
	    {"id":"gone","type":"image","source":"f://y","properties":{"zorder":3}}
	  ]
	}`)
	to := mustParse(t, `{
	  "layers":[
	    {"id":"keep","type":"video","source":"cam","properties":{"zorder":1}},
	    {"id":"move","type":"image","source":"f://x","properties":{"zorder":2,"xpos":100,"width":1000}},
	    {"id":"new","type":"text","source":"","text":"hi","properties":{"zorder":4}}
	  ]
	}`)
	ops := Diff(from, to)
	var kinds []string
	for _, op := range ops {
		kinds = append(kinds, op.Kind.String()+":"+op.ID)
	}
	want := []string{"remove:gone", "update:move", "add:new"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("diff ops = %v, want %v", kinds, want)
	}
}

func TestDiffOrderRemovesBeforeAdds(t *testing.T) {
	// A removed layer frees a zorder; a new layer reuses a low zorder. Removes
	// must precede adds so there is no transient zorder collision.
	from := mustParse(t, `{
	  "layers":[
	    {"id":"base","type":"video","source":"cam","properties":{"zorder":1}},
	    {"id":"old","type":"image","source":"f://o","properties":{"zorder":5}}
	  ]
	}`)
	to := mustParse(t, `{
	  "layers":[
	    {"id":"base","type":"video","source":"cam","properties":{"zorder":1}},
	    {"id":"fresh","type":"image","source":"f://n","properties":{"zorder":5}}
	  ]
	}`)
	ops := Diff(from, to)
	if ops[0].Kind != OpRemove {
		t.Fatalf("first op should be a remove, got %v", ops[0])
	}
}

func TestDiffNoChange(t *testing.T) {
	s := mustParse(t, `{
	  "layers":[{"id":"a","type":"video","source":"cam","properties":{"zorder":1,"xpos":0}}]
	}`)
	if ops := Diff(s, s); len(ops) != 0 {
		t.Fatalf("expected no ops for identical scenes, got %v", ops)
	}
}

func browserDiff(t *testing.T, fromProps, toProps string) []Op {
	t.Helper()
	from := mustParse(t, fmt.Sprintf(`{"layers":[
	  {"id":"b","type":"browser","source":"https://x","properties":%s}
	]}`, fromProps))
	to := mustParse(t, fmt.Sprintf(`{"layers":[
	  {"id":"b","type":"browser","source":"https://x","properties":%s}
	]}`, toProps))
	return Diff(from, to)
}

func wantRebuild(t *testing.T, ops []Op, what string) {
	t.Helper()
	if len(ops) != 2 || ops[0].Kind != OpRemove || ops[1].Kind != OpAdd ||
		ops[0].ID != "b" || ops[1].ID != "b" {
		t.Fatalf("%s change should be a remove+add rebuild, got %+v", what, ops)
	}
}

func TestDiffBrowserChainPropsRebuild(t *testing.T) {
	base := `{"zorder":1,"width":640,"height":480,"framerate":30}`

	// width/height/framerate are baked into the running chain (Chrome
	// viewport, appsrc caps, feeder cadence) — each must rebuild.
	for name, toProps := range map[string]string{
		"width":     `{"zorder":1,"width":1280,"height":480,"framerate":30}`,
		"height":    `{"zorder":1,"width":640,"height":720,"framerate":30}`,
		"framerate": `{"zorder":1,"width":640,"height":480,"framerate":60}`,
		"added":     `{"zorder":1,"framerate":30}`,
		"removed":   `{"zorder":1,"width":640,"height":480}`,
	} {
		wantRebuild(t, browserDiff(t, base, toProps), name)
	}
}

func TestDiffBrowserPadPropsInPlace(t *testing.T) {
	base := `{"zorder":1,"xpos":0,"ypos":0,"width":640,"height":480,"framerate":30}`

	// The compositor pad box (x/y/alpha/zorder) scales the running capture —
	// no rebuild.
	for name, toProps := range map[string]string{
		"xpos":   `{"zorder":1,"xpos":64,"ypos":0,"width":640,"height":480,"framerate":30}`,
		"ypos":   `{"zorder":1,"xpos":0,"ypos":32,"width":640,"height":480,"framerate":30}`,
		"alpha":  `{"zorder":1,"xpos":0,"ypos":0,"width":640,"height":480,"alpha":0.5,"framerate":30}`,
		"zorder": `{"zorder":2,"xpos":0,"ypos":0,"width":640,"height":480,"framerate":30}`,
	} {
		ops := browserDiff(t, base, toProps)
		if len(ops) != 1 || ops[0].Kind != OpUpdateProps || ops[0].ID != "b" {
			t.Fatalf("%s change should be an in-place update, got %+v", name, ops)
		}
	}
	// Identical props: no ops.
	if ops := browserDiff(t, base, base); len(ops) != 0 {
		t.Fatalf("identical browser layer should not diff, got %+v", ops)
	}
}

func TestDiffTextOptionsCarriedAndChanged(t *testing.T) {
	from := mustParse(t, `{
	  "layers":[
	    {"id":"t1","type":"text","text":"hi",
	     "text-options":{"color":"#FF0000","font-desc":"Sans 12"},
	     "properties":{"zorder":1}}
	  ]
	}`)
	// Changing one text option must produce an update op.
	to := mustParse(t, `{
	  "layers":[
	    {"id":"t1","type":"text","text":"hi",
	     "text-options":{"color":"#00FF00FF","font-desc":"Sans 12"},
	     "properties":{"zorder":1}}
	  ]
	}`)
	// Text options are baked into the element chain at build time, so an
	// option change is a remove+add rebuild, not an in-place update.
	ops := Diff(from, to)
	if len(ops) != 2 || ops[0].Kind != OpRemove || ops[1].Kind != OpAdd ||
		ops[0].ID != "t1" || ops[1].ID != "t1" {
		t.Fatalf("option change should be a rebuild, got %+v", ops)
	}
	if got := ops[1].Layer.TextOptions["color"]; got != "#00FF00FF" {
		t.Fatalf("re-add must carry new color, got %q", got)
	}

	// Adding a text layer carries its text-options into the add op.
	add := mustParse(t, `{
	  "layers":[
	    {"id":"t2","type":"text","text":"hey",
	     "text-options":{"shaded-background":"true"},
	     "properties":{"zorder":5}}
	  ]
	}`)
	ops = Diff(from, add)
	var addOp *Op
	for i := range ops {
		if ops[i].Kind == OpAdd && ops[i].ID == "t2" {
			addOp = &ops[i]
		}
	}
	if addOp == nil || addOp.Layer.TextOptions["shaded-background"] != "true" {
		t.Fatalf("add op should carry text-options, got %+v", ops)
	}
}

func TestApplyDiff(t *testing.T) {
	from := mustParse(t, `{
	  "layers":[
	    {"id":"a","type":"video","source":"cam","properties":{"zorder":1}},
	    {"id":"b","type":"image","source":"f://b","properties":{"zorder":2}}
	  ]
	}`)
	to := mustParse(t, `{
	  "layers":[
	    {"id":"a","type":"video","source":"cam","properties":{"zorder":1}},
	    {"id":"c","type":"image","source":"f://c","properties":{"zorder":3}}
	  ]
	}`)
	fa := &fakeApplier{}
	if err := ApplyDiff(context.Background(), fa, Diff(from, to)); err != nil {
		t.Fatalf("ApplyDiff: %v", err)
	}
	want := []string{"remove:b", "add:c"}
	if !reflect.DeepEqual(fa.ops, want) {
		t.Fatalf("applied = %v, want %v", fa.ops, want)
	}
}

func TestApplyOp(t *testing.T) {
	fa := &fakeApplier{}
	if err := ApplyOp(context.Background(), fa, Op{Kind: OpUpdateProps, ID: "a"}); err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}
	if err := ApplyOp(context.Background(), fa, Op{Kind: OpAdd, ID: "b", Layer: Layer{ID: "b"}}); err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}
	if !reflect.DeepEqual(fa.ops, []string{"update:a", "add:b"}) {
		t.Fatalf("applied = %v", fa.ops)
	}
	if err := ApplyOp(context.Background(), fa, Op{Kind: OpKind(99), ID: "x"}); err == nil {
		t.Fatalf("expected an error for an unknown op kind")
	}
}

func TestCloneDeepCopy(t *testing.T) {
	s := mustParse(t, `{
	  "layers":[
	    {"id":"t1","type":"text","text":"hi",
	     "text-options":{"color":"#FF0000"},
	     "filters":[{"kind":"videobalance","params":{"brightness":0.2}}],
	     "properties":{"zorder":1}},
	    {"id":"a","type":"video","source":"cam","properties":{"zorder":2}}
	  ]
	}`)
	c := s.Clone()
	if c == s || !reflect.DeepEqual(s, c) {
		t.Fatalf("Clone() = %+v, want an equal copy", c)
	}
	// Deep: mutating the clone must not touch the original.
	c.Layers[0].Properties["zorder"] = 99
	c.Layers[0].TextOptions["color"] = "#000000"
	c.Layers[0].Filters[0].Params["brightness"] = 0.9
	if s.Layers[0].Properties["zorder"] != 1 || s.Layers[0].TextOptions["color"] != "#FF0000" ||
		s.Layers[0].Filters[0].Params["brightness"] != 0.2 {
		t.Fatalf("clone mutation leaked into the original: %+v", s.Layers[0])
	}
	// Nil-vs-empty is preserved: a layer without filters/text-options
	// stays nil in the clone.
	if c.Layers[1].Filters != nil || c.Layers[1].TextOptions != nil {
		t.Fatalf("nil maps must stay nil after Clone: %+v", c.Layers[1])
	}
	var nilS *Scene
	if nilS.Clone() != nil {
		t.Fatal("(*Scene)(nil).Clone() must return nil")
	}
}

func TestDiffEmptyTextOptionsNoRebuild(t *testing.T) {
	from := mustParse(t, `{"layers":[{"id":"t1","type":"text","text":"hi","properties":{"zorder":1}}]}`)
	// "text-options":{} decodes to an empty (not nil) map. nil vs empty
	// is not a chain change, so no ops at all.
	to := mustParse(t, `{"layers":[{"id":"t1","type":"text","text":"hi","text-options":{},"properties":{"zorder":1}}]}`)
	if ops := Diff(from, to); len(ops) != 0 {
		t.Fatalf("nil vs empty text-options must not rebuild, got %+v", ops)
	}
}

func TestBrowserLayerParseAndDiff(t *testing.T) {
	s := mustParse(t, `{
	  "layers":[
	    {"id":"cam","type":"video","source":"cam","properties":{"zorder":1}},
	    {"id":"web","type":"browser","source":"https://example.com",
	     "properties":{"zorder":2,"width":640,"height":360,"framerate":30,"quality":75}}
	  ]
	}`)
	if len(s.Layers) != 2 || s.Layers[1].Type != "browser" {
		t.Fatalf("parse = %+v", s.Layers)
	}

	// A browser layer added to a running scene produces an add op that
	// carries the source URL.
	from := mustParse(t, `{"layers":[{"id":"cam","type":"video","source":"cam","properties":{"zorder":1}}]}`)
	ops := Diff(from, s)
	var add *Op
	for i := range ops {
		if ops[i].Kind == OpAdd {
			add = &ops[i]
		}
	}
	if add == nil || add.ID != "web" || add.Layer.Source != "https://example.com" {
		t.Fatalf("add op = %+v, want web / https://example.com", add)
	}
}
