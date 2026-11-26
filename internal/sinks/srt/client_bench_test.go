package srt

import (
	"io"
	"log/slog"
	"testing"
)

// benchClient returns an unconnected client (Write takes the state-read +
// early-return path — the per-bucket bookkeeping cost on a down link).
func benchClient(b *testing.B) *Client {
	b.Helper()
	c, err := NewClient(ClientConfig{URI: "srt://127.0.0.1:9000", LatencyMs: 120},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		b.Fatal(err)
	}
	return c
}

// BenchmarkWriteNotConnected benchmarks Write's per-buffer state read when
// the link is down: the mutex pair + early return.
func BenchmarkWriteNotConnected(b *testing.B) {
	b.ReportAllocs()
	c := benchClient(b)
	buf := make([]byte, 1316)
	for i := 0; i < b.N; i++ {
		_, _ = c.Write(buf)
	}
}

// BenchmarkSendRateTracker benchmarks the write-path rate accounting:
// record per chunk + mbps read per Stats poll.
func BenchmarkSendRateTracker(b *testing.B) {
	b.ReportAllocs()
	var t sendRateTracker
	_ = t.mbps() // prime the baseline
	for i := 0; i < b.N; i++ {
		t.record(1316)
		_ = t.mbps()
	}
}
