package scene

import (
	"reflect"
	"strings"
	"testing"
)

func TestFiltersParseAndValidate(t *testing.T) {
	_, err := Parse([]byte(`{"layers":[
	  {"id":"cam","type":"browser","source":"https://x","properties":{"zorder":1},
	   "filters":[
	     {"kind":"videocrop","params":{"top":10,"bottom":0,"left":0,"right":0}},
	     {"kind":"videobalance","params":{"brightness":0.1}},
	     {"kind":"videoflip","params":{"direction":"vertical"}},
	     {"kind":"videorotate","params":{"angle":90}},
	     {"kind":"videobox","params":{"top":4,"bottom":4,"left":4,"right":4}},
	     {"kind":"videoscale","params":{"width":640,"height":360}},
	     {"kind":"effectv","params":{"edge-tv":true,"aging-tv":true}},
	     {"kind":"volume","params":{"volume":0.5}},
	     {"kind":"equalizer","params":{"bands":10,"band0":2.0,"band9":-3.0}},
	     {"kind":"audiodynamic","params":{"mode":1,"threshold":0.6}},
	     {"kind":"pitch","params":{"tempo":1.25}}
	   ]}
	]}`))
	if err != nil {
		t.Fatalf("full filter set should validate: %v", err)
	}
}

func TestFilterUnknownKind(t *testing.T) {
	if _, err := Parse([]byte(`{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1},
	   "filters":[{"kind":"vibecolor"}]}
	]}`)); err == nil {
		t.Fatal("expected unknown kind error")
	}
}

func TestFilterAudioOnNonBrowser(t *testing.T) {
	if _, err := Parse([]byte(`{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1},
	   "filters":[{"kind":"volume","params":{"volume":0.5}}]}
	]}`)); err == nil {
		t.Fatal("expected audio-filter-on-video-layer error")
	}
}

func TestFilterUnknownParam(t *testing.T) {
	if _, err := Parse([]byte(`{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1},
	   "filters":[{"kind":"videocrop","params":{"top":10,"diagonal":5}}]}
	]}`)); err == nil {
		t.Fatal("expected unknown param error")
	}
}

func TestFilterParamType(t *testing.T) {
	if _, err := Parse([]byte(`{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1},
	   "filters":[{"kind":"videoflip","params":{"direction":42}}]}
	]}`)); err == nil {
		t.Fatal("expected param type error (string wanted, number given)")
	}
	if _, err := Parse([]byte(`{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1},
	   "filters":[{"kind":"videocrop","params":{"top":1.5}}]}
	]}`)); err == nil {
		t.Fatal("expected integer param error")
	}
}

func TestFilterCrossParam(t *testing.T) {
	cases := []struct {
		name    string
		filters string
	}{
		{"videoscale no dim", `[{"kind":"videoscale"}]`},
		{"balance range", `[{"kind":"videobalance","params":{"brightness":2.0}}]`},
		{"flip direction", `[{"kind":"videoflip","params":{"direction":"upside"}}]`},
		{"effectv none on", `[{"kind":"effectv"}]`},
		{"equalizer bands", `[{"kind":"equalizer","params":{"bands":7}}]`},
		{"equalizer gain band out of range", `[{"kind":"equalizer","params":{"bands":3,"band5":1.0}}]`},
		{"dynamic mode", `[{"kind":"audiodynamic","params":{"mode":9}}]`},
		{"dynamic threshold", `[{"kind":"audiodynamic","params":{"threshold":1.5}}]`},
		{"pitch tempo", `[{"kind":"pitch","params":{"tempo":11.0}}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ := "video"
			if strings.Contains(tc.name, "dynamic") || strings.Contains(tc.name, "pitch") {
				typ = "browser" // audio filters only land on browser layers
			}
			doc := `{"layers":[{"id":"cam","type":"` + typ + `","source":"cam","properties":{"zorder":1},"filters":` + tc.filters + `}]}`
			if _, err := Parse([]byte(doc)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestFilterDiffRebuild(t *testing.T) {
	from := mustParse(t, `{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1},
	   "filters":[{"kind":"videocrop","params":{"top":10}}]}
	]}`)
	// Same filters -> no ops.
	if ops := Diff(from, from); len(ops) != 0 {
		t.Fatalf("identical filters should produce no ops, got %v", ops)
	}
	// A param change -> rebuild (remove + add), not update.
	to := mustParse(t, `{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1},
	   "filters":[{"kind":"videocrop","params":{"top":20}}]}
	]}`)
	ops := Diff(from, to)
	var kinds []string
	for _, op := range ops {
		kinds = append(kinds, op.Kind.String()+":"+op.ID)
	}
	if !reflect.DeepEqual(kinds, []string{"remove:cam", "add:cam"}) {
		t.Fatalf("filter change ops = %v, want [remove:cam add:cam]", kinds)
	}
	// A filter added -> rebuild too.
	to2 := mustParse(t, `{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1},
	   "filters":[{"kind":"videocrop","params":{"top":10}},{"kind":"videoflip"}]}
	]}`)
	ops = Diff(from, to2)
	if len(ops) != 2 || ops[0].Kind != OpRemove || ops[1].Kind != OpAdd {
		t.Fatalf("added filter should rebuild, got %v", ops)
	}
}

func TestFilterDiffVolumeInPlace(t *testing.T) {
	from := mustParse(t, `{"layers":[
	  {"id":"cam","type":"browser","source":"http://x","properties":{"zorder":1},
	   "filters":[{"kind":"volume","params":{"volume":1.0}}]}
	]}`)
	// A volume param change is an in-place filter update, not a rebuild.
	to := mustParse(t, `{"layers":[
	  {"id":"cam","type":"browser","source":"http://x","properties":{"zorder":1},
	   "filters":[{"kind":"volume","params":{"volume":0.5}}]}
	]}`)
	ops := Diff(from, to)
	if len(ops) != 1 || ops[0].Kind != OpUpdateFilter || ops[0].ID != "cam" {
		t.Fatalf("volume param change ops = %v, want [update-filter:cam]", ops)
	}
	// The op flags exactly the changed slot.
	if !ops[0].Layer.Filters[0].inPlaceChanged {
		t.Fatalf("changed slot must be flagged in the op")
	}
	// A filter KIND change (or a non-in-place filter) still rebuilds.
	to2 := mustParse(t, `{"layers":[
	  {"id":"cam","type":"browser","source":"http://x","properties":{"zorder":1},
	   "filters":[{"kind":"equalizer","params":{}}]}
	]}`)
	ops = Diff(from, to2)
	if len(ops) != 2 || ops[0].Kind != OpRemove || ops[1].Kind != OpAdd {
		t.Fatalf("filter kind change must rebuild, got %v", ops)
	}
	// A video filter param change still rebuilds (no in-place path).
	from2 := mustParse(t, `{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1},
	   "filters":[{"kind":"videocrop","params":{"top":10}}]}
	]}`)
	to3 := mustParse(t, `{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1},
	   "filters":[{"kind":"videocrop","params":{"top":40}}]}
	]}`)
	ops = Diff(from2, to3)
	if len(ops) != 2 || ops[0].Kind != OpRemove || ops[1].Kind != OpAdd {
		t.Fatalf("video filter param change must rebuild, got %v", ops)
	}
}

func TestFilterDiffUpdateOnlyProps(t *testing.T) {
	from := mustParse(t, `{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1,"xpos":0},
	   "filters":[{"kind":"videoflip"}]}
	]}`)
	to := mustParse(t, `{"layers":[
	  {"id":"cam","type":"video","source":"cam","properties":{"zorder":1,"xpos":64,"width":1024},
	   "filters":[{"kind":"videoflip"}]}
	]}`)
	ops := Diff(from, to)
	if len(ops) != 1 || ops[0].Kind != OpUpdateProps {
		t.Fatalf("props-only change should stay an update, got %v", ops)
	}
}

func TestLookupFilterAndFactory(t *testing.T) {
	if _, ok := LookupFilter("volume"); !ok {
		t.Fatal("volume should be registered")
	}
	if _, ok := LookupFilter("nope"); ok {
		t.Fatal("nope should not be registered")
	}
	eq, _ := LookupFilter("equalizer")
	f, err := eq.FactoryFor(map[string]any{"bands": 10.0})
	if err != nil || f != "equalizer-10bands" {
		t.Fatalf("equalizer factory = %q (%v), want equalizer-10bands", f, err)
	}
	vb, _ := LookupFilter("videobalance")
	eff := vb.EffectiveParams(map[string]any{"brightness": 0.2})
	if eff["contrast"] != 1.0 || eff["brightness"] != 0.2 || eff["hue"] != 0.0 {
		t.Fatalf("EffectiveParams = %v, want defaults + override", eff)
	}
}
