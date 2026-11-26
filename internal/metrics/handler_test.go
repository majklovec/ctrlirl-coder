package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsHandler(t *testing.T) {
	h := Handler(func() []Point {
		return []Point{
			{Name: "test_gauge", Value: 42},
			{Name: "test_ratio", Value: 1.5e-3},
			{Name: "test_count", Value: 7, Type: TypeCounter},
		}
	})

	// /metrics renders Prometheus text exposition: # TYPE line + name-value.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	got := string(body)
	for _, want := range []string{
		"# TYPE test_gauge gauge",
		"test_gauge 42",
		"test_ratio 0.0015",
		"# TYPE test_count counter",
		"test_count 7",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("body missing %q:\n%s", want, got)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}

	// POST is rejected.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", strings.NewReader("x")))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rec.Code)
	}
}

func TestMetricsHandlerPanicsWithoutGather(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for nil gather")
		}
	}()
	Handler(nil)
}
