// Package server is the single HTTP surface of ctrlirl-coder: one listener
// serving the JSON-RPC 2.0 control API (POST /rpc) and the /metrics endpoint
// (GET) side by side. It replaces the former Unix-socket RPC listener and
// the separate loopback metrics server; clients talk to it with plain HTTP
// tooling (curl, browser, HTTP clients).
package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Config selects the listen address.
type Config struct {
	Addr string
}

// Server is one HTTP listener hosting the control API and metrics under
// one mux.
type Server struct {
	srv  *http.Server
	ln   net.Listener
	addr string
}

// New builds the HTTP server, binds the listener, and mounts the given
// endpoints under their paths. Each handler may be nil to leave the path
// absent. The server is NOT serving until Start.
func New(cfg Config, handlers map[string]http.Handler) (*Server, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("http: addr required")
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("http: listen %s: %w", cfg.Addr, err)
	}
	mux := http.NewServeMux()
	for path, h := range handlers {
		if h == nil {
			continue
		}
		mux.Handle(path, h)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return &Server{
		srv:  &http.Server{Handler: mux},
		ln:   ln,
		addr: ln.Addr().String(),
	}, nil
}

// Start serves until Stop is called. It returns immediately.
func (s *Server) Start(ctx context.Context) {
	go func() {
		// Serve returns once the listener is closed by Stop; ignore.
		_ = s.srv.Serve(s.ln)
	}()
}

// Stop shuts the server down and closes the listener. Safe to call more
// than once.
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	_ = s.ln.Close()
	return err
}

// Addr returns the bound listener address (useful when bound to :0).
func (s *Server) Addr() string { return s.addr }
