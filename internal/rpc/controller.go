package rpc

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"ctrlirl-coder/internal/scene"
)

// Controller is the scene state machine behind the JSON-RPC methods. It owns
// the authoritative copy of the running scene, so scene file reloads (SIGHUP)
// and RPC requests converge on one place: every edit mutates a copy of the
// scene descriptor and pushes the diff through the scene.Applier (the
// compositor, which serializes the pipeline mutations). Pure Go: the applier
// is an interface, tests use a fake.
type Controller struct {
	mu     sync.Mutex
	ap     scene.Applier
	logger *slog.Logger

	cur    *scene.Scene
	scenes map[string]*scene.Scene // named scenes for switching
}

// New builds a controller. initial is the scene currently on screen (nil →
// empty scene). ap receives the diff operations.
func New(ap scene.Applier, initial *scene.Scene, logger *slog.Logger) *Controller {
	if initial == nil {
		initial = &scene.Scene{Layers: []scene.Layer{}}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Controller{
		ap:     ap,
		logger: logger,
		cur:    initial,
		scenes: map[string]*scene.Scene{},
	}
}

// Current returns a deep copy of the running scene.
func (c *Controller) Current() *scene.Scene {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneScene(c.cur)
}

// SceneNames lists the named (switchable) scenes, sorted.
func (c *Controller) SceneNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(maps.Keys(c.scenes))
}

// Scene returns a named scene (deep copy). ok is false when unknown.
func (c *Controller) Scene(name string) (*scene.Scene, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.scenes[name]
	if !ok {
		return nil, false
	}
	return cloneScene(s), true
}

// Register stores a parsed scene under name for later scene.switch, without
// touching the running scene. name defaults to the scene's own name field.
func (c *Controller) Register(name string, s *scene.Scene) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scenes[c.resolveName(name, s)] = cloneScene(s)
}

// resolveName picks the name a scene is stored under: the argument, falling
// back to the scene's own name field, then a generated one. Callers hold c.mu.
func (c *Controller) resolveName(name string, s *scene.Scene) string {
	if s.Name != "" && name == "" {
		name = s.Name
	}
	if name == "" {
		name = fmt.Sprintf("unnamed-%d", len(c.scenes))
	}
	return name
}

// Replace moves the running scene from the current state to the new state
// (already parsed/validated): diff, apply, swap on success. On a mid-apply
// failure a best-effort rollback (diff back to the old scene) is attempted
// and the original error is returned.
func (c *Controller) Replace(ctx context.Context, nx *scene.Scene) ([]scene.Op, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.replaceLocked(ctx, nx)
}

// Load stores a parsed scene under name and switches to it (same diff/
// rollback path as Replace). name defaults to the scene's own name field.
func (c *Controller) Load(ctx context.Context, name string, s *scene.Scene) ([]scene.Op, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name = c.resolveName(name, s)
	c.scenes[name] = cloneScene(s)
	ops, err := c.replaceLocked(ctx, s)
	if err != nil {
		delete(c.scenes, name)
		return ops, err
	}
	c.logger.Info("rpc: scene loaded and switched", "scene", name, "layers", len(s.Layers))
	return ops, nil
}

// Switch moves to a previously loaded named scene.
func (c *Controller) Switch(ctx context.Context, name string) (*scene.Scene, []scene.Op, error) {
	c.mu.Lock()
	s, ok := c.scenes[name]
	c.mu.Unlock()
	if !ok {
		return nil, nil, fmt.Errorf("rpc: no scene named %q (load it first)", name)
	}
	s = cloneScene(s)
	ops, err := c.Replace(ctx, s)
	if err != nil {
		return nil, ops, fmt.Errorf("rpc: switch to %q: %w", name, err)
	}
	c.logger.Info("rpc: scene switched", "scene", name, "layers", len(s.Layers))
	return s, ops, nil
}

// modify runs tx against a clone of the running scene, re-validates the
// result through the full scene parser (zorder normalization, id/type/filter
// checks), and pushes the diff to the applier with rollback-on-failure. It
// returns the applied ops and the new scene.
func (c *Controller) modify(ctx context.Context, describe string, tx func(*scene.Scene) error) ([]scene.Op, *scene.Scene, error) {
	// One critical section from clone to commit: a concurrent edit that
	// read the scene before the previous edit committed would otherwise
	// diff against the stale state and its ops revert the new edit's
	// changes (live-repro: two back-to-back layer.set calls re-mutated the
	// first layer's alpha, hiding the just-shown scene).
	c.mu.Lock()
	defer c.mu.Unlock()
	nx := cloneScene(c.cur)
	if err := tx(nx); err != nil {
		return nil, nil, err
	}
	data, err := json.Marshal(nx)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := scene.Parse(data)
	if err != nil {
		return nil, nil, err
	}
	ops, err := c.replaceLocked(ctx, parsed)
	if err != nil {
		c.logger.Error("rpc: apply failed, rolled back", "op", describe, "err", err)
		return ops, nil, fmt.Errorf("rpc: %s: %w", describe, err)
	}
	c.logger.Info("rpc: applied", "op", describe, "ops", len(ops))
	return ops, parsed, nil
}

// replaceLocked is Replace without the lock (callers hold it). Ops are
// applied one by one so a mid-sequence failure can be inverted precisely
// (only the ops that took effect are rolled back).
func (c *Controller) replaceLocked(ctx context.Context, nx *scene.Scene) ([]scene.Op, error) {
	ops := scene.Diff(c.cur, nx)
	if len(ops) == 0 {
		c.cur = nx
		return ops, nil
	}
	// One freeze/thaw around the whole batch, not per RemoveLayer — per-op
	// state cycles make a multi-layer switch stall the stream seconds
	// (each READY→PLAYING re-prerolls compositor + encoder + mux).
	resume, err := scene.Suspend(ctx, c.ap, ops)
	if err != nil {
		return nil, err // nothing applied yet
	}
	defer resume()
	old := c.cur
	for i, op := range ops {
		if err := scene.ApplyOp(ctx, c.ap, op); err != nil {
			c.rollbackLocked(ctx, old, ops[:i])
			return ops, err
		}
	}
	c.cur = nx
	return ops, nil
}

// rollbackLocked inverts exactly the ops that took effect (done), in reverse
// order with z-order safety (re-adds after removes), and restores cur to the
// pre-transition scene. If the inversion itself fails the pipeline may
// diverge from the scene; the caller must reload the scene. Callers hold
// the lock.
func (c *Controller) rollbackLocked(ctx context.Context, old *scene.Scene, done []scene.Op) {
	var rb []scene.Op
	for i := len(done) - 1; i >= 0; i-- {
		op := done[i]
		switch op.Kind {
		case scene.OpAdd:
			rb = append(rb, scene.Op{Kind: scene.OpRemove, ID: op.ID})
		case scene.OpRemove:
			if l := findLayer(old, op.ID); l != nil {
				rb = append(rb, scene.Op{Kind: scene.OpAdd, ID: op.ID, Layer: *l})
			}
		case scene.OpUpdateProps:
			if l := findLayer(old, op.ID); l != nil {
				rb = append(rb, scene.Op{Kind: scene.OpUpdateProps, ID: op.ID, Layer: *l})
			}
		case scene.OpUpdateFilter:
			// Unflagged: ApplyOp reverts every in-place filter to the old
			// values, which is exactly the inverse of the flagged op.
			if l := findLayer(old, op.ID); l != nil {
				rb = append(rb, scene.Op{Kind: scene.OpUpdateFilter, ID: op.ID, Layer: *l})
			}
		}
	}
	// z-order safety, stable within a kind: removes first, then updates,
	// then adds.
	slices.SortStableFunc(rb, func(a, b scene.Op) int { return cmp.Compare(rollbackRank(a), rollbackRank(b)) })
	c.cur = old
	if len(rb) > 0 {
		if err := scene.ApplyDiff(ctx, c.ap, rb); err != nil {
			c.logger.Error("rpc: ROLLBACK FAILED — pipeline may not match the scene; reload it", "err", err)
		}
	}
}

// Layer returns a pointer to the layer with id in s.
func findLayer(s *scene.Scene, id string) *scene.Layer {
	for i := range s.Layers {
		if s.Layers[i].ID == id {
			return &s.Layers[i]
		}
	}
	return nil
}

// rollbackRank orders rollback ops: removes (0), updates (1), adds (2).
func rollbackRank(op scene.Op) int {
	switch op.Kind {
	case scene.OpRemove:
		return 0
	case scene.OpUpdateProps, scene.OpUpdateFilter:
		return 1
	default:
		return 2
	}
}

// cloneScene deep-copies the scene (no JSON round-trip; scene.Clone
// preserves nil-vs-empty maps).
func cloneScene(s *scene.Scene) *scene.Scene {
	return s.Clone()
}
