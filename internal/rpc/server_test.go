package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ctrlirl-coder/internal/scene"
	"ctrlirl-coder/internal/server"
)

// recApplier records every operation the controller pushes (scene.Applier).
type recApplier struct {
	ops       []string
	failNext  bool // make the next op fail (rollback test)
	failed    string
	blockNext bool          // make the next UpdateLayerProps block until releaseCh closes
	releaseCh chan struct{} // closed by the test to unblock a hooked op
	mu        sync.Mutex
}

// count returns how many recorded ops equal op.
func (f *recApplier) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.ops {
		if o == op {
			n++
		}
	}
	return n
}

func (f *recApplier) AddLayer(ctx context.Context, l scene.Layer) error {
	if f.failNext {
		f.failNext, f.failed = false, "add:"+l.ID
		return errors.New("injected add failure")
	}
	f.ops = append(f.ops, "add:"+l.ID)
	return nil
}

func (f *recApplier) RemoveLayer(ctx context.Context, id string) error {
	if f.failNext {
		f.failNext, f.failed = false, "remove:"+id
		return errors.New("injected remove failure")
	}
	f.ops = append(f.ops, "remove:"+id)
	return nil
}

func (f *recApplier) UpdateLayerProps(ctx context.Context, id string, props map[string]float64) error {
	if f.failNext {
		f.failNext, f.failed = false, "update:"+id
		return errors.New("injected update failure")
	}
	// The concurrency test polls count() from another goroutine while this
	// op is in flight, so the record and the hook state are read under mu.
	f.mu.Lock()
	f.ops = append(f.ops, "update:"+id)
	block, ch := f.blockNext, f.releaseCh
	f.blockNext = false
	f.mu.Unlock()
	if block {
		<-ch
	}
	return nil
}
func (f *recApplier) UpdateLayerFilter(ctx context.Context, id string, index int, spec scene.FilterSpec) error {
	if f.failNext {
		f.failNext, f.failed = false, "update-filter:"+id
		return errors.New("injected update-filter failure")
	}
	f.ops = append(f.ops, fmt.Sprintf("update-filter:%s:%d", id, index))
	return nil
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// client is one HTTP connection to the /rpc endpoint.
type client struct {
	http *http.Client
	base string
}

// call posts one JSON-RPC request and returns the decoded response.
func (c *client) call(t *testing.T, id int, method string, params string) map[string]any {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)
	resp, err := c.http.Post(c.base+"/rpc", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("bad response %q: %v", data, err)
	}
	return out
}

func respErr(t *testing.T, resp map[string]any) (code int, msg string) {
	t.Helper()
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error response, got %v", resp)
	}
	code = int(e["code"].(float64))
	msg, _ = e["message"].(string)
	return code, msg
}

func respApplied(t *testing.T, resp map[string]any) []string {
	t.Helper()
	if resp["error"] != nil {
		t.Fatalf("expected success, got %v", resp["error"])
	}
	r, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected object result, got %v", resp["result"])
	}
	applied, _ := r["applied"].([]any)
	out := make([]string, 0, len(applied))
	for _, a := range applied {
		out = append(out, a.(string))
	}
	return out
}

// startServer starts the HTTP API server with the given controller and
// returns an HTTP client bound to it.
func startServer(t *testing.T, ctl *Controller) *client {
	t.Helper()
	srv, err := server.New(server.Config{Addr: "127.0.0.1:0"}, map[string]http.Handler{
		"/rpc": NewServer(ctl),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Start(context.Background())
	t.Cleanup(func() { _ = srv.Stop() })
	base := "http://" + srv.Addr()
	hc := &http.Client{}
	t.Cleanup(hc.CloseIdleConnections)
	return &client{http: hc, base: base}
}

// callRaw posts an arbitrary body (used to exercise parse-error paths) and
// returns the decoded response.
func (c *client) callRaw(t *testing.T, body string) map[string]any {
	t.Helper()
	resp, err := c.http.Post(c.base+"/rpc", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("bad response %q: %v", data, err)
	}
	return out
}

func TestRPCFullFlow(t *testing.T) {
	ap := &recApplier{}
	ctl := New(ap, nil, nil)
	rw := startServer(t, ctl)
	id := 0
	next := func() int { id++; return id }

	// ping + status on an empty scene.
	if r := rw.call(t, next(), "system.ping", "null"); r["result"] != "pong" {
		t.Fatalf("ping: %v", r)
	}
	if r := rw.call(t, next(), "system.status", "null"); r["result"] == nil {
		t.Fatal("status: no result")
	}

	// Load a scene with two layers.
	sceneJSON := `{"name":"s1","canvas":{"width":1280,"height":720},"layers":[
	  {"id":"a","type":"video","source":"","properties":{"zorder":1}},
	  {"id":"b","type":"text","text":"hi","properties":{"zorder":2}}
	]}`
	r := rw.call(t, next(), "scene.load", mustJSON(t, map[string]any{"name": "s1", "json": json.RawMessage(sceneJSON)}))
	if got := respApplied(t, r); !reflect.DeepEqual(got, []string{"add:a", "add:b"}) {
		t.Fatalf("scene.load applied = %v", got)
	}
	if !reflect.DeepEqual(ap.ops, []string{"add:a", "add:b"}) {
		t.Fatalf("applier ops = %v", ap.ops)
	}

	// scene.get reflects the load.
	r = rw.call(t, next(), "scene.get", "null")
	cur, _ := r["result"].(map[string]any)
	if cur["name"] != "s1" {
		t.Fatalf("scene.get name = %v", cur["name"])
	}

	// scene.load with switch:false registers only — no applier ops, no switch.
	ap.ops = nil
	r = rw.call(t, next(), "scene.load", mustJSON(t, map[string]any{"name": "s3", "json": json.RawMessage(sceneJSON), "switch": false}))
	if r == nil || r["result"] != "registered" {
		t.Fatalf("register-only load result = %v", r)
	}
	if len(ap.ops) != 0 {
		t.Fatalf("register-only load pushed ops: %v", ap.ops)
	}

	// layer.set with properties only → update op.
	ap.ops = nil
	r = rw.call(t, next(), "layer.set", `{"id":"a","properties":{"zorder":1,"xpos":64,"width":1024}}`)
	if got := respApplied(t, r); !reflect.DeepEqual(got, []string{"update:a"}) {
		t.Fatalf("layer.set applied = %v", got)
	}

	// layer.set moving an unsized (canvas-wide) layer off origin would widen
	// the compositor canvas past the trunk capsfilter → zero frames; the
	// canvas invariant rejects it and the pipeline stays untouched.
	ap.ops = nil
	r = rw.call(t, next(), "layer.set", `{"id":"b","properties":{"zorder":2,"xpos":64}}`)
	if _, msg := respErr(t, r); !strings.Contains(msg, "exceeds canvas") {
		t.Fatalf("geometry rejection message = %q, want canvas violation", msg)
	}
	if len(ap.ops) != 0 {
		t.Fatalf("rejected layer.set must not push ops: %v", ap.ops)
	}

	// layer.set changing source → rebuild (remove+add).
	ap.ops = nil
	r = rw.call(t, next(), "layer.set", `{"id":"a","source":"/dev/video0"}`)
	if got := respApplied(t, r); !reflect.DeepEqual(got, []string{"remove:a", "add:a"}) {
		t.Fatalf("layer.set source applied = %v", got)
	}

	// filter.add on layer b (a browser-free layer: video filter only).
	r = rw.call(t, next(), "filter.add", `{"id":"a","filter":{"kind":"videocrop","params":{"top":10}}}`)
	if got := respApplied(t, r); len(got) != 2 {
		t.Fatalf("filter.add should rebuild, got %v", got)
	}

	// filter.set on the freshly added filter.
	ap.ops = nil
	r = rw.call(t, next(), "filter.set", `{"id":"a","index":0,"params":{"top":20}}`)
	if got := respApplied(t, r); len(got) != 2 {
		t.Fatalf("filter.set should rebuild, got %v", got)
	}

	// filter.remove.
	ap.ops = nil
	r = rw.call(t, next(), "filter.remove", `{"id":"a","index":0}`)
	if got := respApplied(t, r); len(got) != 2 {
		t.Fatalf("filter.remove should rebuild, got %v", got)
	}

	// layer.add + layer.remove round-trip.
	r = rw.call(t, next(), "layer.add", `{"layer":{"id":"c","type":"image","source":"/tmp/x.png","properties":{"zorder":3}}}`)
	if got := respApplied(t, r); !reflect.DeepEqual(got, []string{"add:c"}) {
		t.Fatalf("layer.add applied = %v", got)
	}
	r = rw.call(t, next(), "layer.remove", `{"id":"c"}`)
	if got := respApplied(t, r); !reflect.DeepEqual(got, []string{"remove:c"}) {
		t.Fatalf("layer.remove applied = %v", got)
	}

	// scene.switch: load a second scene, switch, switch back.
	scene2 := `{"name":"s2","canvas":{"width":640,"height":360},"layers":[{"id":"solo","type":"video","source":"","properties":{"zorder":1}}]}`
	r = rw.call(t, next(), "scene.load", mustJSON(t, map[string]any{"name": "s2", "json": json.RawMessage(scene2)}))
	respApplied(t, r)
	r = rw.call(t, next(), "scene.switch", `{"name":"s1"}`)
	respApplied(t, r)
	r = rw.call(t, next(), "scene.names", "null")
	names, _ := r["result"].([]any)
	if len(names) != 3 { // s1, s2, s3 (register-only)
		t.Fatalf("scene.names = %v", names)
	}
}

func TestRPCErrors(t *testing.T) {
	ap := &recApplier{}
	ctl := New(ap, nil, nil)
	rw := startServer(t, ctl)
	id := 0
	next := func() int { id++; return id }

	// Unknown method.
	if code, _ := respErr(t, rw.call(t, next(), "nope.nope", "null")); code != CodeMethodNotFound {
		t.Fatalf("unknown method code = %d", code)
	}
	// Malformed JSON.
	if code, _ := respErr(t, rw.callRaw(t, "{not json")); code != CodeParseError {
		t.Fatalf("parse error code = %d", code)
	}
	// Missing params.
	if code, _ := respErr(t, rw.call(t, next(), "layer.remove", "null")); code != CodeInvalidParams {
		t.Fatalf("missing params code = %d", code)
	}
	// Validation failure (duplicate layer id) → app error, state unchanged.
	sceneJSON := `{"name":"s1","layers":[
	  {"id":"a","type":"video","source":"","properties":{"zorder":1}},
	  {"id":"a","type":"video","source":"","properties":{"zorder":2}}
	]}`
	r := rw.call(t, next(), "scene.load", mustJSON(t, map[string]any{"json": json.RawMessage(sceneJSON)}))
	respErr(t, r)
	if len(ap.ops) != 0 {
		t.Fatalf("failed load must not reach the applier, got %v", ap.ops)
	}
	// Unknown layer.
	if _, msg := respErr(t, rw.call(t, next(), "layer.set", `{"id":"ghost","properties":{"zorder":1}}`)); msg == "" {
		t.Fatal("expected an error for unknown layer")
	}
	// Invalid scene JSON (audio filter on a video layer).
	bad := `{"name":"x","layers":[{"id":"a","type":"video","source":"","properties":{"zorder":1},"filters":[{"kind":"volume"}]}]}`
	if code, _ := respErr(t, rw.call(t, next(), "scene.load", mustJSON(t, map[string]any{"json": json.RawMessage(bad)}))); code != CodeInvalidParams {
		t.Fatalf("invalid scene code = %d", code)
	}
	// Unknown switch target.
	if _, msg := respErr(t, rw.call(t, next(), "scene.switch", `{"name":"nope"}`)); msg == "" {
		t.Fatal("expected unknown scene error")
	}
}

func TestRPCRollback(t *testing.T) {
	ap := &recApplier{}
	ctl := New(ap, nil, nil)
	rw := startServer(t, ctl)
	id := 0
	next := func() int { id++; return id }

	// Establish a scene, then make the applier fail on the next add.
	sceneJSON := `{"name":"s1","layers":[{"id":"a","type":"video","source":"","properties":{"zorder":1}}]}`
	r := rw.call(t, next(), "scene.load", mustJSON(t, map[string]any{"json": json.RawMessage(sceneJSON)}))
	respApplied(t, r)

	ap.failNext = true
	r = rw.call(t, next(), "layer.add", `{"layer":{"id":"b","type":"video","source":"","properties":{"zorder":2}}}`)
	if code, _ := respErr(t, r); code != CodeAppError {
		t.Fatalf("expected app error, got %v", r)
	}
	// State must be unchanged: scene.get has only layer a.
	r = rw.call(t, next(), "scene.get", "null")
	cur, _ := r["result"].(map[string]any)
	layers, _ := cur["layers"].([]any)
	if len(layers) != 1 {
		t.Fatalf("state after failed add = %v layers, want 1", len(layers))
	}
}

func TestRPCSave(t *testing.T) {
	ap := &recApplier{}
	ctl := New(ap, nil, nil)
	rw := startServer(t, ctl)
	id := 0
	next := func() int { id++; return id }

	sceneJSON := `{"name":"saved","layers":[{"id":"a","type":"video","source":"","properties":{"zorder":1}}]}`
	r := rw.call(t, next(), "scene.load", mustJSON(t, map[string]any{"name": "saved", "json": json.RawMessage(sceneJSON)}))
	respApplied(t, r)

	path := filepath.Join(t.TempDir(), "out.json")
	r = rw.call(t, next(), "scene.save", `{"path":"`+path+`"}`)
	if r["error"] != nil {
		t.Fatalf("scene.save: %v", r["error"])
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var sc struct {
		Name   string `json:"name"`
		Layers []any  `json:"layers"`
	}
	if err := json.Unmarshal(data, &sc); err != nil || sc.Name != "saved" {
		t.Fatalf("saved scene = %s (%v)", data, err)
	}

	// scene.save with an unknown name is an error, not a nil scene.
	r = rw.call(t, next(), "scene.save", `{"name":"nope","path":"`+path+`"}`)
	if code, msg := respErr(t, r); code != CodeAppError {
		t.Fatalf("scene.save unknown name: code %d msg %q", code, msg)
	}
}

func TestRPCSceneNamesSorted(t *testing.T) {
	ap := &recApplier{}
	ctl := New(ap, nil, nil)
	rw := startServer(t, ctl)
	id := 0
	next := func() int { id++; return id }

	for _, n := range []string{"zeta", "alpha", "mid"} {
		r := rw.call(t, next(), "scene.load", mustJSON(t, map[string]any{
			"name":   n,
			"switch": false,
			"json":   json.RawMessage(`{"layers":[{"id":"` + n + `","type":"video","source":"","properties":{"zorder":1}}]}`),
		}))
		if r["error"] != nil {
			t.Fatalf("scene.load %s: %v", n, r["error"])
		}
	}
	r := rw.call(t, next(), "scene.names", `{}`)
	names, _ := r["result"].([]any)
	got := make([]string, 0, len(names))
	for _, n := range names {
		got = append(got, n.(string))
	}
	if want := []string{"alpha", "mid", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scene.names = %v, want %v", got, want)
	}
}

// TestRPCLoadFailureUnlocks: a failing scene.load (mid-apply op failure)
// must release the controller lock — the next request completes.
// TestRPCNotification204: a request without an id (notification) gets a
// 204 with no body, single or batched.
func TestRPCNotification204(t *testing.T) {
	ap := &recApplier{}
	ctl := New(ap, nil, nil)
	c := startServer(t, ctl)

	// Single notification → 204, empty body.
	resp, err := c.http.Post(c.base+"/rpc", "application/json", strings.NewReader(`{"jsonrpc":"2.0","method":"system.ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("single notification = %d, want 204", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("notification body = %q, want empty", body)
	}

	// All-notification batch → 204.
	resp, err = c.http.Post(c.base+"/rpc", "application/json", strings.NewReader(`[{"jsonrpc":"2.0","method":"system.ping"}]`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("batch notification = %d, want 204", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("batch notification body = %q, want empty", body)
	}
}

func TestRPCLoadFailureUnlocks(t *testing.T) {
	ap := &recApplier{}
	ctl := New(ap, nil, nil)
	rw := startServer(t, ctl)
	id := 0
	next := func() int { id++; return id }

	ap.failNext = true
	r := rw.call(t, next(), "scene.load", mustJSON(t, map[string]any{
		"name": "broken",
		"json": json.RawMessage(`{"layers":[{"id":"x","type":"video","source":"","properties":{"zorder":1}}]}`),
	}))
	if r["error"] == nil {
		t.Fatalf("expected the failing load to error, got %v", r)
	}
	// If the lock were stuck, this call would block forever (test timeout).
	if r = rw.call(t, next(), "scene.get", `{}`); r["result"] == nil {
		t.Fatalf("controller locked after a failed load: %v", r)
	}
	// The failed load must not have registered the scene.
	r = rw.call(t, next(), "scene.names", `{}`)
	names, _ := r["result"].([]any)
	if len(names) != 0 {
		t.Fatalf("failed load left a registered scene: %v", names)
	}
}

// TestModifyConcurrentNoLostEdit is a regression test for the switch
// blackout bug: two fast back-to-back scene edits (the test script fires
// fire-and-forget layer.set calls) used to clone the scene in modify() and
// commit it LATER, out of the lock. The second edit then committed a scene
// cloned before the first edit's changes, silently rolling back the first
// edit's alpha in the tracked state — the pipeline and the scene state
// diverged and both layers could end up hidden/muted (black + silent).
// With blocking applied, the interleaving is deterministic.
func TestModifyConcurrentNoLostEdit(t *testing.T) {
	ap := &recApplier{releaseCh: make(chan struct{})}
	ap.mu.Lock()
	ap.blockNext = true
	ap.mu.Unlock()
	ctl := New(ap, nil, nil)

	sc := &scene.Scene{
		Name:   "s",
		Canvas: scene.Canvas{Width: 1280, Height: 720},
		Layers: []scene.Layer{
			{ID: "a", Type: "video", Properties: map[string]float64{"zorder": 1, "alpha": 1}},
			{ID: "b", Type: "text", Text: "hi", Properties: map[string]float64{"zorder": 2, "alpha": 0}},
		},
	}
	if _, err := ctl.Load(context.Background(), "s", sc); err != nil {
		t.Fatalf("seed load: %v", err)
	}

	// Edit A: hide a. Its applier hook blocks mid-apply, before the commit.
	modifyA := func(s *scene.Scene) error {
		return withLayer(s, "a", func(l *scene.Layer) error {
			l.Properties["alpha"] = 0
			return nil
		})
	}
	errA := make(chan error, 1)
	go func() {
		_, _, e := ctl.modify(context.Background(), "A", modifyA)
		errA <- e
	}()
	// Wait until A has reached the blocked apply (its op recorded).
	deadline := time.Now().Add(5 * time.Second)
	for ap.count("update:a") < 1 {
		if time.Now().After(deadline) {
			t.Fatal("edit A never reached its apply op")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Edit B: show b. Under the old (pre-fix) code B clones the PRE-A
	// scene (a.alpha == 1) while A is mid-apply and its commit then loses
	// whichever edit commits last. With the fix the whole edit is one
	// critical section: B waits for A's commit and diffs the fresh state.
	errB := make(chan error, 1)
	go func() {
		_, _, e := ctl.modify(context.Background(), "B", func(s *scene.Scene) error {
			return withLayer(s, "b", func(l *scene.Layer) error {
				l.Properties["alpha"] = 1
				return nil
			})
		})
		errB <- e
	}()
	// Let B reach its (blocking) clone/entry point before releasing A.
	time.Sleep(100 * time.Millisecond)
	close(ap.releaseCh)

	select {
	case e := <-errA:
		if e != nil {
			t.Fatalf("edit A: %v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("edit A did not finish")
	}
	select {
	case e := <-errB:
		if e != nil {
			t.Fatalf("edit B: %v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("edit B did not finish")
	}

	cur := ctl.Current()
	var aAlpha, bAlpha float64
	for _, l := range cur.Layers {
		switch l.ID {
		case "a":
			aAlpha = l.Properties["alpha"]
		case "b":
			bAlpha = l.Properties["alpha"]
		}
	}
	if aAlpha != 0 {
		t.Fatalf("edit A lost to concurrent edit B: a.alpha = %v, want 0", aAlpha)
	}
	if bAlpha != 1 {
		t.Fatalf("edit B lost: b.alpha = %v, want 1", bAlpha)
	}
}

// The scene list is part of the wire format: an empty list is [], never
// null, and names come sorted.
func TestSceneNamesEmptyIsListAndSorted(t *testing.T) {
	ctl := New(&recApplier{}, nil, nil)
	if got := mustJSON(t, ctl.SceneNames()); got != "[]" {
		t.Fatalf("no scenes: SceneNames encodes as %s, want []", got)
	}
	for _, name := range []string{"b", "c", "a"} {
		ctl.Register(name, &scene.Scene{Name: name})
	}
	if got := mustJSON(t, ctl.SceneNames()); got != `["a","b","c"]` {
		t.Fatalf("SceneNames = %s, want sorted", got)
	}
}
