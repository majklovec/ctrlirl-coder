package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func testHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"echo": string(b)})
	})
}

func TestHTTPPing(t *testing.T) {
	srv, err := New(Config{Addr: "127.0.0.1:0"}, map[string]http.Handler{
		"/rpc": testHandler("pong"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.Start(context.Background())
	t.Cleanup(func() { _ = srv.Stop() })
	base := "http://" + srv.Addr()
	hc := http.DefaultClient

	// Mounted endpoint over plaintext HTTP.
	resp, err := hc.Post(base+"/rpc", "application/json", strings.NewReader(`{"id":1}`))
	if err != nil {
		t.Fatalf("POST /rpc: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /rpc = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["echo"] != `{"id":1}` {
		t.Fatalf("echo = %v", out["echo"])
	}

	// Unmounted path → 404 (both methods).
	if resp, err = hc.Get(base + "/nope"); err != nil {
		t.Fatalf("GET /nope: %v", err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET /nope = %d, want 404", resp.StatusCode)
		}
	}
	// Non-POST on /rpc → 405.
	if resp, err = hc.Get(base + "/rpc"); err != nil {
		t.Fatalf("GET /rpc: %v", err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET /rpc = %d, want 405", resp.StatusCode)
		}
	}
}

func TestAddrRequired(t *testing.T) {
	if _, err := New(Config{Addr: ""}, map[string]http.Handler{}); err == nil {
		t.Fatal("New with empty addr succeeded, want error")
	}
}
