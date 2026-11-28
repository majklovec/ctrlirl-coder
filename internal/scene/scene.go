// Package scene parses the JSON multi-layer scene descriptor and computes the
// add/remove/update operations needed to move from one scene to another.
//
// It is deliberately pure-Go (no GStreamer): the diff logic is unit-tested on
// every commit. Applying the operations is delegated to an Applier interface
// that the (GStreamer-backed) compositor implements, so this package never
// imports the multimedia stack.
package scene

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
)

// LayerType is the kind of a scene layer.
type LayerType string

// The supported layer types.
const (
	LayerTypeVideo    LayerType = "video"
	LayerTypeImage    LayerType = "image"
	LayerTypeText     LayerType = "text"
	LayerTypeBrowser  LayerType = "browser"
	LayerTypeTestCode LayerType = "testcode"
)

var knownLayerTypes = map[LayerType]bool{
	LayerTypeVideo:    true,
	LayerTypeImage:    true,
	LayerTypeText:     true,
	LayerTypeBrowser:  true,
	LayerTypeTestCode: true,
}

// Layer is a single scene layer as described in the JSON file.
type Layer struct {
	ID         string             `json:"id"`
	Type       LayerType          `json:"type"`
	Source     string             `json:"source"`
	Properties map[string]float64 `json:"properties"`
	Text       string             `json:"text"` // for type == "text"

	// TextOptions carries the pango textoverlay options of a text layer
	// (font-desc, color, shaded-background, ...). String-typed on purpose:
	// the compositor's numeric `properties` map cannot carry them, and the
	// layer package maps each key onto the matching element property.
	TextOptions map[string]string `json:"text-options"`

	// Filters are the media filters applied to this layer's chain, in scene
	// order (video filters before the videoconvert tail, audio filters in
	// the layer's audio chain). See filters.go for the registry.
	Filters []FilterSpec `json:"filters"`
}

// Canvas is the composition canvas size.
type Canvas struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Scene is the full descriptor.
type Scene struct {
	Name   string  `json:"name"`
	Canvas Canvas  `json:"canvas"`
	Layers []Layer `json:"layers"`
}

// Load reads and validates a scene JSON file.
func Load(path string) (*Scene, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("scene: read %s: %w", path, err)
	}
	return Parse(data)
}

// Parse decodes a scene descriptor from JSON bytes.
func Parse(data []byte) (*Scene, error) {
	s := &Scene{}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("scene: parse: %w", err)
	}
	if err := s.Finalize(); err != nil {
		return nil, err
	}
	return s, nil
}

// Finalize normalizes the scene (canvas defaults, unique z-orders) and
// validates it. Parse calls it after decoding; callers that build or edit
// scenes programmatically (RPC scene editing) do the same before applying.
func (s *Scene) Finalize() error {
	s.normalize()
	return s.Validate()
}

// Clone deep-copies the scene (layers, property maps, filters, param maps),
// preserving nil-vs-empty maps (maps.Clone keeps nil nil), so the caller can
// mutate the clone without touching the original.
func (s *Scene) Clone() *Scene {
	if s == nil {
		return nil
	}
	c := *s
	c.Layers = make([]Layer, len(s.Layers))
	for i, l := range s.Layers {
		l.Properties = maps.Clone(l.Properties)
		l.TextOptions = maps.Clone(l.TextOptions)
		l.Filters = cloneFilters(l.Filters)
		c.Layers[i] = l
	}
	return &c
}

func cloneFilters(fs []FilterSpec) []FilterSpec {
	if fs == nil {
		return nil
	}
	out := make([]FilterSpec, len(fs))
	for i, f := range fs {
		f.Params = maps.Clone(f.Params)
		out[i] = f
	}
	return out
}

// normalize assigns a zorder to any layer missing one, using its index, and
// makes zorder unique by renumbering any duplicate to the next free value
// (see pitfall 7.6: the compositor cannot break z-order ties deterministically,
// so the scene must guarantee uniqueness before applying). It also fills in
// the default canvas when the scene omits one.
func (s *Scene) normalize() {
	if s.Canvas.Width == 0 && s.Canvas.Height == 0 {
		s.Canvas.Width, s.Canvas.Height = 1280, 720
	}
	zorder := make(map[float64]bool)
	for i := range s.Layers {
		l := &s.Layers[i]
		if l.Properties == nil {
			l.Properties = make(map[string]float64)
		}
		z, ok := l.Properties["zorder"]
		if !ok {
			z = float64(i + 1)
			l.Properties["zorder"] = z
		}
		for zorder[z] {
			z++
		}
		zorder[z] = true
		l.Properties["zorder"] = z
	}
}

// Validate enforces structural invariants.
func (s *Scene) Validate() error {
	seen := make(map[string]bool)
	zorder := make(map[float64]int)
	for i, l := range s.Layers {
		if l.ID == "" {
			return fmt.Errorf("scene: layer[%d] has empty id", i)
		}
		if seen[l.ID] {
			return fmt.Errorf("scene: duplicate layer id %q", l.ID)
		}
		seen[l.ID] = true

		if !knownLayerTypes[l.Type] {
			return fmt.Errorf("scene: layer %q has unknown type %q", l.ID, l.Type)
		}

		z := l.Properties["zorder"]
		if prev, dup := zorder[z]; dup {
			return fmt.Errorf("scene: layers %q and %q share zorder %v", l.ID, s.Layers[prev].ID, z)
		}
		zorder[z] = i

		if err := validateFilters(l); err != nil {
			return err
		}

		if err := s.validateGeometry(l); err != nil {
			return err
		}
	}
	return nil
}

// validateGeometry enforces the canvas invariant: no layer's rect may extend
// past the canvas. The compositor canvas is the union of the layer geometry;
// a layer that overflows it dead-ends caps negotiation against the fixed
// trunk capsfilter → zero frames, black stream, no GStreamer warnings.
//
// Layers without explicit width/height are treated as canvas-sized (the
// compositor pad defaults), so an unsized layer with any offset overflows.
func (s *Scene) validateGeometry(l Layer) error {
	x, y := l.Properties["xpos"], l.Properties["ypos"]
	w, h := l.Properties["width"], l.Properties["height"]
	if x < 0 || y < 0 || w < 0 || h < 0 {
		return fmt.Errorf("scene: layer %q has negative geometry (xpos=%v ypos=%v width=%v height=%v)",
			l.ID, x, y, w, h)
	}
	if w == 0 {
		w = float64(s.Canvas.Width)
	}
	if h == 0 {
		h = float64(s.Canvas.Height)
	}
	if x+w > float64(s.Canvas.Width) || y+h > float64(s.Canvas.Height) {
		return fmt.Errorf("scene: layer %q rect (%v..%v, %v..%v) exceeds canvas %dx%d",
			l.ID, x, x+w, y, y+h, s.Canvas.Width, s.Canvas.Height)
	}
	return nil
}

// OpKind is the type of a scene diff operation.
type OpKind int

const (
	OpAdd OpKind = iota
	OpRemove
	OpUpdateProps
	OpUpdateFilter
)

// String returns a short name for logging.
func (k OpKind) String() string {
	switch k {
	case OpAdd:
		return "add"
	case OpRemove:
		return "remove"
	case OpUpdateProps:
		return "update"
	case OpUpdateFilter:
		return "update-filter"
	default:
		return "?"
	}
}

// Op is one atomic change to apply to the running scene. Layer carries the
// complete target layer (all fields incl. filters): Add ops apply it as-is,
// remove+add rebuild pairs rely on it to re-materialize the chain with the
// new filters, and update ops carry it so the compositor's layer reference
// stays in sync (its Close-on-teardown path uses it).
type Op struct {
	Kind  OpKind
	ID    string
	Layer Layer
}

// Diff computes the operations to transform from "from" to "to".
//
// Order matters for z-order safety: removes are emitted before adds, so a
// layer that is both removed and re-added (or whose z-order is freed up) does
// not collide during the transition (see spec §5.5 / pitfall 7.6).
func Diff(from, to *Scene) []Op {
	fromL := indexByID(from)
	toL := indexByID(to)

	var removes, updates, adds []Op
	for id, nl := range toL {
		ol, existed := fromL[id]
		if !existed {
			adds = append(adds, Op{Kind: OpAdd, ID: id, Layer: nl})
			continue
		}
		d := diffLayers(ol, nl)
		if d.rebuild {
			// Removes are ordered before adds, so the z-order is
			// re-established by the add.
			removes = append(removes, Op{Kind: OpRemove, ID: id})
			adds = append(adds, Op{Kind: OpAdd, ID: id, Layer: nl})
			continue
		}
		if d.props {
			// Compositor-pad properties: an in-place pad update.
			updates = append(updates, Op{Kind: OpUpdateProps, ID: id, Layer: nl})
		}
		if len(d.filters) > 0 {
			// Only params of in-place-updatable filters changed. No
			// teardown: the running elements get their properties set, so
			// no Chrome restart, no mux pad churn (rebuilds renumber the
			// mpegtsmux audio pads and reshuffle the TS programs), and no
			// stream gap. Flag the changed slots on a copy so ApplyOp
			// updates exactly those.
			flagged := nl
			flagged.Filters = slices.Clone(nl.Filters)
			for _, i := range d.filters {
				flagged.Filters[i].inPlaceChanged = true
			}
			updates = append(updates, Op{Kind: OpUpdateFilter, ID: id, Layer: flagged})
		}
	}
	for id := range fromL {
		if _, ok := toL[id]; !ok {
			removes = append(removes, Op{Kind: OpRemove, ID: id})
		}
	}

	// Deterministic ordering: removes (by id), then updates (by id, pad
	// properties before filters), then adds (by ascending zorder so the base
	// layer is attached first).
	byID := func(a, b Op) int { return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.Kind, b.Kind)) }
	slices.SortFunc(removes, byID)
	slices.SortFunc(updates, byID)
	slices.SortFunc(adds, func(a, b Op) int { return cmp.Compare(zof(a), zof(b)) })

	return slices.Concat(removes, updates, adds)
}

func indexByID(s *Scene) map[string]Layer {
	if s == nil {
		return make(map[string]Layer)
	}
	m := make(map[string]Layer, len(s.Layers))
	for _, l := range s.Layers {
		m[l.ID] = l
	}
	return m
}

func zof(op Op) float64 { return op.Layer.Properties["zorder"] }

// layerDiff is how one layer differs between two scenes.
type layerDiff struct {
	rebuild bool  // the element chain must be torn down and rebuilt
	props   bool  // compositor-pad properties changed
	filters []int // filter slots whose params changed, updatable in place
}

// diffLayers classifies the change from a to b.
//
// Type, source, text, text options and the filter list's shape are baked
// into the element chain at build time — the compositor cannot splice
// elements into a running chain — so a change to any of them is a rebuild
// (remove + add). Compositor-pad properties and params of in-place-updatable
// filters are applied to the running layer.
//
// Browser layers are the exception on the pad side: their width/height set
// the Chrome viewport and the appsrc caps, and their framerate sets the
// feeder's capture cadence and the appsrc caps. All three are baked into
// the running chain — an in-place pad update would only move the compositor
// box while the capture kept the old resolution/fps — so they rebuild too.
// x/y/zorder/alpha stay in-place (the compositor scales the capture to the
// pad box).
func diffLayers(a, b Layer) layerDiff {
	d := layerDiff{props: !maps.Equal(a.Properties, b.Properties)}
	// maps.Equal treats nil and empty maps as equal: an empty (not nil)
	// text-options map is not a chain change.
	if a.Type != b.Type || a.Source != b.Source || a.Text != b.Text ||
		!maps.Equal(a.TextOptions, b.TextOptions) ||
		len(a.Filters) != len(b.Filters) ||
		(a.Type == LayerTypeBrowser && browserChainPropsChanged(a, b)) {
		d.rebuild = true
		return d
	}
	allInPlace := true
	for i, fa := range a.Filters {
		fb := b.Filters[i]
		if fa.Kind != fb.Kind {
			d.rebuild = true
			return d
		}
		if def, ok := LookupFilter(fa.Kind); !ok || !def.InPlace {
			allInPlace = false
		}
		// Param values are JSON scalars (float64/string/bool, per the
		// registry type checks), so they compare with ==.
		if !maps.Equal(fa.Params, fb.Params) {
			d.filters = append(d.filters, i)
		}
	}
	// In place only when every filter of the layer is in-place capable, not
	// just the changed ones: a rollback re-applies every slot (ApplyOp).
	if len(d.filters) > 0 && !allInPlace {
		d.rebuild = true
	}
	return d
}

// browserChainPropsChanged reports whether a browser layer's width, height
// or framerate changed (see diffLayers). A key present on only one side
// counts as a change: in the worst case (value equals the layer package's
// 1280x720@30 default) that costs one needless rebuild, while treating
// absent as default would risk a silent no-op.
func browserChainPropsChanged(a, b Layer) bool {
	for _, key := range []string{"width", "height", "framerate"} {
		av, aOK := a.Properties[key]
		bv, bOK := b.Properties[key]
		if aOK != bOK || av != bv {
			return true
		}
	}
	return false
}

// Applier is implemented by the running compositor. scene defines it so this
// package stays free of the multimedia stack; the compositor is the implementer.
type Applier interface {
	// AddLayer adds a new layer. l must carry a complete properties map.
	AddLayer(ctx context.Context, l Layer) error
	// RemoveLayer removes an existing layer by id.
	RemoveLayer(ctx context.Context, id string) error
	// UpdateLayerProps updates the compositor pad properties of an existing
	// layer in place.
	UpdateLayerProps(ctx context.Context, id string, props map[string]float64) error
	// UpdateLayerFilter re-applies the filter spec to the layer's running
	// filter element in place (in-place-capable filters only — the diff
	// never emits other kinds). No chain teardown, no Chrome restart.
	UpdateLayerFilter(ctx context.Context, id string, index int, spec FilterSpec) error
}

// Batcher is an optional Applier capability: it freezes the whole pipeline
// (READY) before a batch of ops and restores PLAYING afterwards, so a scene
// switch stalls the stream once per batch instead of once per RemoveLayer
// (each per-op freeze/thaw costs ~1s of re-preroll).
type Batcher interface {
	SuspendStream(ctx context.Context) error
	ResumeStream(ctx context.Context) error
}

// Suspend freezes the stream around a batch of ops when that pays off: the
// applier can (Batcher) and the batch attaches or detaches more than one
// chain. In-place updates never need the freeze — a batch of pad and filter
// updates must not stall the stream. The returned function resumes the
// stream; it is a no-op when nothing was suspended.
func Suspend(ctx context.Context, a Applier, ops []Op) (resume func(), err error) {
	structural := 0
	for _, op := range ops {
		if op.Kind == OpAdd || op.Kind == OpRemove {
			structural++
		}
	}
	b, ok := a.(Batcher)
	if !ok || structural < 2 {
		return func() {}, nil
	}
	if err := b.SuspendStream(ctx); err != nil {
		return nil, fmt.Errorf("scene: suspend batch: %w", err)
	}
	return func() { _ = b.ResumeStream(ctx) }, nil
}

// ApplyDiff executes ops against the applier in order (removes, then updates,
// then adds — the order produced by Diff), inside one Suspend.
func ApplyDiff(ctx context.Context, a Applier, ops []Op) error {
	resume, err := Suspend(ctx, a, ops)
	if err != nil {
		return err
	}
	defer resume()
	for _, op := range ops {
		if err := ApplyOp(ctx, a, op); err != nil {
			return err
		}
	}
	return nil
}

// ApplyOp applies a single op against the applier.
func ApplyOp(ctx context.Context, a Applier, op Op) error {
	switch op.Kind {
	case OpRemove:
		if err := a.RemoveLayer(ctx, op.ID); err != nil {
			return fmt.Errorf("scene: remove %s: %w", op.ID, err)
		}
	case OpUpdateProps:
		if err := a.UpdateLayerProps(ctx, op.ID, op.Layer.Properties); err != nil {
			return fmt.Errorf("scene: update %s: %w", op.ID, err)
		}
	case OpUpdateFilter:
		// The op carries the full target layer. Diff flags exactly the
		// changed slots; rollback ops (unflagged) set every filter back to
		// the old values.
		anyFlagged := slices.ContainsFunc(op.Layer.Filters, func(f FilterSpec) bool { return f.inPlaceChanged })
		for i, spec := range op.Layer.Filters {
			if anyFlagged && !spec.inPlaceChanged {
				continue
			}
			if err := a.UpdateLayerFilter(ctx, op.ID, i, spec); err != nil {
				return fmt.Errorf("scene: update filter %d of %s: %w", i, op.ID, err)
			}
		}
	case OpAdd:
		if err := a.AddLayer(ctx, op.Layer); err != nil {
			return fmt.Errorf("scene: add %s: %w", op.ID, err)
		}
	default:
		return fmt.Errorf("scene: unknown op kind %d", op.Kind)
	}
	return nil
}
