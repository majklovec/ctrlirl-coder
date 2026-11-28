// Package rpc is the runtime remote control: a JSON-RPC 2.0 API (wire format
// in RPC.md) that edits the running scene — layers, filters, named scenes.
// It is one HTTP handler mounted at POST /rpc on the shared plaintext HTTP
// listener (internal/server). Bodies are single JSON-RPC requests or 2.0
// batches (arrays); the response mirrors the request shape, or 204 when
// every request was a notification.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"ctrlirl-coder/internal/scene"
)

// JSON-RPC 2.0 error codes (spec §3.1) plus the application range.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeServerError    = -32603
	CodeAppError       = -32000 // scene/pipeline apply failure (message carries detail)
)

// request is one JSON-RPC 2.0 call. params is an object for every method.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type errorObj struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *errorObj       `json:"error,omitempty"`
}

// Server is the JSON-RPC 2.0 endpoint: one http.Handler driven by the
// shared HTTP listener (POST /rpc). Drive it from curl, any HTTP client, or
// JSON-RPC tooling.
type Server struct {
	ctl     *Controller
	methods map[string]handler
}

// NewServer returns the handler for a control controller.
func NewServer(ctl *Controller) *Server {
	s := &Server{ctl: ctl}
	s.methods = s.methodTable()
	return s
}

// handler is one JSON-RPC method: decodes its params, returns the result or
// an RPC error.
type handler func(p json.RawMessage) (any, *errorObj)

// maxRequestLine bounds one request body (4 MiB of JSON dwarfs any scene
// edit; the bound stops a pathological peer from filling memory).
const maxRequestLine = 4 * 1024 * 1024

// methodTable maps the JSON-RPC method names to their handlers.
func (s *Server) methodTable() map[string]handler {
	return map[string]handler{
		"system.ping":   func(json.RawMessage) (any, *errorObj) { return "pong", nil },
		"system.status": func(json.RawMessage) (any, *errorObj) { return s.status(), nil },
		"scene.get": func(json.RawMessage) (any, *errorObj) {
			return s.ctl.Current(), nil
		},
		"scene.names": func(json.RawMessage) (any, *errorObj) {
			return s.ctl.SceneNames(), nil
		},
		"scene.load":    s.sceneLoad,
		"scene.switch":  s.sceneSwitch,
		"scene.save":    s.sceneSave,
		"layer.add":     s.layerAdd,
		"layer.remove":  s.layerRemove,
		"layer.set":     s.layerSet,
		"filter.add":    s.filterAdd,
		"filter.remove": s.filterRemove,
		"filter.set":    s.filterSet,
	}
}

// ServeHTTP implements the JSON-RPC 2.0 endpoint over HTTP. Only POST is
// accepted; the body is a single request object or a 2.0 batch (array).
// A 204 with no body answers all-notification batches.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if cl := r.ContentLength; cl > maxRequestLine {
		http.Error(w, "request body exceeds 4 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestLine+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxRequestLine {
		http.Error(w, "request body exceeds 4 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	var first byte
	if len(body) > 0 {
		first = body[0]
	}
	var resp any
	switch first {
	case '{':
		var req request
		if err := json.Unmarshal(body, &req); err != nil {
			resp = &response{JSONRPC: "2.0", Error: &errorObj{Code: CodeParseError, Message: err.Error()}}
			break
		}
		if r := s.handleOne(&req); r == nil {
			w.WriteHeader(http.StatusNoContent) // notification: no response
			return
		} else {
			resp = r
		}
	case '[':
		var reqs []request
		if err := json.Unmarshal(body, &reqs); err != nil {
			resp = &response{JSONRPC: "2.0", Error: &errorObj{Code: CodeParseError, Message: err.Error()}}
			break
		}
		out := make([]*response, 0, len(reqs))
		for i := range reqs {
			if r := s.handleOne(&reqs[i]); r != nil {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		resp = out
	default:
		resp = &response{JSONRPC: "2.0", Error: &errorObj{Code: CodeParseError, Message: "body must be a JSON object or array"}}
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(resp); err != nil {
		return // client went away mid-write; nothing left to say
	}
}

// handleOne decodes one request object and dispatches the method. It
// returns nil for a well-formed notification (no id) that needs no
// response.
func (s *Server) handleOne(req *request) *response {
	if req.JSONRPC != "2.0" {
		return &response{JSONRPC: "2.0", ID: req.ID, Error: &errorObj{Code: CodeInvalidRequest, Message: "jsonrpc must be \"2.0\""}}
	}
	var result any
	var rpcErr *errorObj
	if h, ok := s.methods[req.Method]; ok {
		result, rpcErr = h(req.Params)
	} else {
		rpcErr = &errorObj{Code: CodeMethodNotFound, Message: "unknown method " + req.Method}
	}
	if req.ID == nil { // notification: no response
		return nil
	}
	resp := &response{JSONRPC: "2.0", ID: req.ID, Result: result}
	if rpcErr != nil {
		resp.Error = rpcErr
	}
	return resp
}

// status is the live state snapshot (system.status).
func (s *Server) status() map[string]any {
	cur := s.ctl.Current()
	layers := make([]map[string]any, 0, len(cur.Layers))
	for _, l := range cur.Layers {
		kinds := make([]string, 0, len(l.Filters))
		for _, f := range l.Filters {
			kinds = append(kinds, f.Kind)
		}
		layers = append(layers, map[string]any{
			"id":      l.ID,
			"type":    l.Type,
			"zorder":  l.Properties["zorder"],
			"filters": kinds,
		})
	}
	return map[string]any{
		"scene":  cur.Name,
		"layers": layers,
		"scenes": s.ctl.SceneNames(),
	}
}

func params(params json.RawMessage, v any) error {
	if len(params) == 0 {
		return errors.New("params required")
	}
	if err := json.Unmarshal(params, v); err != nil {
		return err
	}
	return nil
}

// decode unmarshals a method's params object, mapping decode failures to
// invalid-params errors.
func decode[T any](p json.RawMessage) (T, *errorObj) {
	var in T
	if err := params(p, &in); err != nil {
		return in, invalidParams(err.Error())
	}
	return in, nil
}

func invalidParams(msg string) *errorObj {
	return &errorObj{Code: CodeInvalidParams, Message: msg}
}

// edit applies one scene mutation: clone + tx + full revalidation + diff
// apply, wrapped in the uniform opResult/app-error shape.
func (s *Server) edit(describe string, tx func(*scene.Scene) error) (any, *errorObj) {
	ops, _, err := s.ctl.modify(context.Background(), describe, tx)
	if err != nil {
		return nil, appErrf("%v", err)
	}
	return opResult(ops), nil
}

// withLayer locates the layer with id in sc, or returns an error.
func withLayer(sc *scene.Scene, id string, fn func(*scene.Layer) error) error {
	l := findLayer(sc, id)
	if l == nil {
		return errors.New("no layer " + id)
	}
	return fn(l)
}

// removeLayer drops the layer with id from sc, or returns an error.
func removeLayer(sc *scene.Scene, id string) error {
	for i, l := range sc.Layers {
		if l.ID == id {
			sc.Layers = append(sc.Layers[:i], sc.Layers[i+1:]...)
			return nil
		}
	}
	return errors.New("no layer " + id)
}

// opResult is the uniform result of a scene-editing method.
func opResult(ops []scene.Op) map[string]any {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.Kind.String()+":"+op.ID)
	}
	return map[string]any{"applied": out}
}

func appErrf(format string, a ...any) *errorObj {
	return &errorObj{Code: CodeAppError, Message: fmt.Sprintf(format, a...)}
}

// --- scene methods ---------------------------------------------------------

func (s *Server) sceneLoad(p json.RawMessage) (any, *errorObj) {
	in, rpcErr := decode[struct {
		Name   string          `json:"name"`
		JSON   json.RawMessage `json:"json"`
		Switch *bool           `json:"switch"` // nil/true → switch (default); false → register only
	}](p)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if len(in.JSON) == 0 {
		return nil, invalidParams("scene.load: params.json (scene JSON object) required")
	}
	sc, err := scene.Parse(in.JSON)
	if err != nil {
		return nil, invalidParams(err.Error())
	}
	ctx := context.Background()
	if in.Switch != nil && !*in.Switch {
		s.ctl.Register(in.Name, sc)
		return "registered", nil
	}
	ops, err := s.ctl.Load(ctx, in.Name, sc)
	if err != nil {
		return nil, appErrf("%v", err)
	}
	return opResult(ops), nil
}

func (s *Server) sceneSwitch(p json.RawMessage) (any, *errorObj) {
	in, rpcErr := decode[struct {
		Name string `json:"name"`
	}](p)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if in.Name == "" {
		return nil, invalidParams("scene.switch: name required")
	}
	_, ops, err := s.ctl.Switch(context.Background(), in.Name)
	if err != nil {
		return nil, appErrf("%v", err)
	}
	return opResult(ops), nil
}

// sceneSave writes a scene to a file (scene.save: {name?|current, path?}).
// path is required for name (named scene file); for current the original
// scene path is not known to the server, so path is required as well.
func (s *Server) sceneSave(p json.RawMessage) (any, *errorObj) {
	in, rpcErr := decode[struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}](p)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if in.Path == "" {
		return nil, invalidParams("scene.save: path required")
	}
	sc := s.sceneForSave(in.Name)
	if sc == nil {
		return nil, appErrf("no scene named %q (load it first)", in.Name)
	}
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return nil, appErrf("encode scene: %v", err)
	}
	if err := os.WriteFile(in.Path, append(data, '\n'), 0o644); err != nil {
		return nil, appErrf("write %s: %v", in.Path, err)
	}
	return map[string]any{"path": in.Path}, nil
}

func (s *Server) sceneForSave(name string) *scene.Scene {
	if name == "" {
		return s.ctl.Current()
	}
	sc, ok := s.ctl.Scene(name)
	if !ok {
		return nil
	}
	return sc
}

// --- layer methods ---------------------------------------------------------

func (s *Server) layerAdd(p json.RawMessage) (any, *errorObj) {
	in, rpcErr := decode[struct {
		Layer scene.Layer `json:"layer"`
	}](p)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if in.Layer.ID == "" {
		return nil, invalidParams("layer.add: layer.id required")
	}
	return s.edit("layer.add "+in.Layer.ID, func(sc *scene.Scene) error {
		if findLayer(sc, in.Layer.ID) != nil {
			return errors.New("layer " + in.Layer.ID + " already exists (use layer.set)")
		}
		sc.Layers = append(sc.Layers, in.Layer)
		return nil
	})
}

func (s *Server) layerRemove(p json.RawMessage) (any, *errorObj) {
	in, rpcErr := decode[struct {
		ID string `json:"id"`
	}](p)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if in.ID == "" {
		return nil, invalidParams("layer.remove: id required")
	}
	return s.edit("layer.remove "+in.ID, func(sc *scene.Scene) error {
		return removeLayer(sc, in.ID)
	})
}

// layerSet updates a layer in place. Omitted fields are left as-is; a
// present field replaces the whole sub-object (properties map, text-options
// map, filters array). Changing text, source, type or filters rebuilds the
// chain (the diff engine emits remove+add), other property changes update
// the pad in place.
func (s *Server) layerSet(p json.RawMessage) (any, *errorObj) {
	in, rpcErr := decode[struct {
		ID          string             `json:"id"`
		Source      *string            `json:"source"`
		Type        *string            `json:"type"`
		Text        *string            `json:"text"`
		Properties  map[string]float64 `json:"properties"`
		TextOptions map[string]string  `json:"text-options"`
		Filters     []scene.FilterSpec `json:"filters"`
	}](p)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if in.ID == "" {
		return nil, invalidParams("layer.set: id required")
	}
	return s.edit("layer.set "+in.ID, func(sc *scene.Scene) error {
		return withLayer(sc, in.ID, func(l *scene.Layer) error {
			if in.Source != nil {
				l.Source = *in.Source
			}
			if in.Type != nil {
				l.Type = scene.LayerType(*in.Type)
			}
			if in.Text != nil {
				l.Text = *in.Text
			}
			if in.Properties != nil {
				l.Properties = in.Properties
			}
			if in.TextOptions != nil {
				l.TextOptions = in.TextOptions
			}
			if in.Filters != nil {
				l.Filters = in.Filters
			}
			return nil
		})
	})
}

// --- filter methods --------------------------------------------------------

func (s *Server) filterAdd(p json.RawMessage) (any, *errorObj) {
	in, rpcErr := decode[struct {
		ID     string           `json:"id"`
		Filter scene.FilterSpec `json:"filter"`
		Index  *int             `json:"index"` // optional position (default: append)
	}](p)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if in.ID == "" || in.Filter.Kind == "" {
		return nil, invalidParams("filter.add: id and filter.kind required")
	}
	return s.edit("filter.add "+in.ID, func(sc *scene.Scene) error {
		return withLayer(sc, in.ID, func(l *scene.Layer) error {
			idx := len(l.Filters)
			if in.Index != nil {
				if *in.Index < 0 || *in.Index > len(l.Filters) {
					return errors.New("filter index out of range")
				}
				idx = *in.Index
			}
			filters := make([]scene.FilterSpec, 0, len(l.Filters)+1)
			filters = append(filters, l.Filters[:idx]...)
			filters = append(filters, in.Filter)
			filters = append(filters, l.Filters[idx:]...)
			l.Filters = filters
			return nil
		})
	})
}

func (s *Server) filterRemove(p json.RawMessage) (any, *errorObj) {
	in, rpcErr := decode[struct {
		ID    string `json:"id"`
		Index int    `json:"index"`
	}](p)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if in.ID == "" {
		return nil, invalidParams("filter.remove: id and index required")
	}
	return s.edit("filter.remove "+in.ID, func(sc *scene.Scene) error {
		return withLayer(sc, in.ID, func(l *scene.Layer) error {
			if in.Index < 0 || in.Index >= len(l.Filters) {
				return errors.New("filter index out of range")
			}
			l.Filters = append(l.Filters[:in.Index], l.Filters[in.Index+1:]...)
			return nil
		})
	})
}

func (s *Server) filterSet(p json.RawMessage) (any, *errorObj) {
	in, rpcErr := decode[struct {
		ID     string         `json:"id"`
		Index  int            `json:"index"`
		Kind   string         `json:"kind"`
		Params map[string]any `json:"params"`
	}](p)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if in.ID == "" {
		return nil, invalidParams("filter.set: id, index and kind/params required")
	}
	return s.edit("filter.set "+in.ID, func(sc *scene.Scene) error {
		return withLayer(sc, in.ID, func(l *scene.Layer) error {
			if in.Index < 0 || in.Index >= len(l.Filters) {
				return errors.New("filter index out of range")
			}
			if in.Kind != "" {
				l.Filters[in.Index].Kind = in.Kind
			}
			if in.Params != nil {
				l.Filters[in.Index].Params = in.Params
			}
			return nil
		})
	})
}
