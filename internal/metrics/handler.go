// Package metrics exposes a plain-text /metrics endpoint as an http.Handler.
// It is deliberately dependency-free: one handler, one gather callback, no
// third-party metrics library — the text is the Prometheus exposition
// format (`# TYPE` + `name value` lines, version 0.0.4), scrapeable by
// Prometheus and an operator eyeball/debug surface.
//
// It no longer owns a listener: the handler is mounted at /metrics on the
// shared HTTP server (internal/server), which also carries the JSON-RPC
// control API.
package metrics

import (
	"fmt"
	"net/http"
	"strings"
)

// Metric types in the Prometheus exposition format. The zero/empty Type
// renders as "gauge" (the common case); "counter" is for cumulative
// counters.
const (
	TypeGauge   = "gauge"
	TypeCounter = "counter"
)

// Point is one metric sample: a name, a value, and the Prometheus type.
// Names are part of the operator-facing contract; keep them stable and
// unit-suffixed where the unit is not self-evident.
type Point struct {
	Name  string
	Value float64
	Type  string
}

// Gather is the callback the endpoint calls on every scrape. It runs on the
// HTTP handler goroutine, so it must only read stats (no blocking work).
type Gather func() []Point

// Bool renders a boolean as a Prometheus 0/1 gauge value.
func Bool(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Handler returns the /metrics http.Handler. It renders the gather output in
// the Prometheus text exposition format: one `# TYPE <name> <type>` line per
// metric followed by the `name value` sample. GET only; other methods are
// rejected.
func Handler(g Gather) http.Handler {
	if g == nil {
		panic("metrics: gather callback required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		var b strings.Builder
		for _, p := range g() {
			t := p.Type
			if t == "" {
				t = TypeGauge
			}
			fmt.Fprintf(&b, "# TYPE %s %s\n%s %g\n", p.Name, t, p.Name, p.Value)
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
}
