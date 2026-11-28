package srt

import "testing"

// SRTO_MAXBW takes bytes per second; the config is in kbit/s. Passing the
// number through unconverted capped a "6000" stream at 48 kbit/s.
func TestSocketOptionsConvertMaxBandwidthToBytesPerSecond(t *testing.T) {
	opts := socketOptions(ClientConfig{LatencyMs: 120, MaxBandwidthKbps: 6000})
	if got, want := opts["maxbw"], "750000"; got != want {
		t.Errorf("maxbw = %q, want %q (6000 kbit/s in bytes/s)", got, want)
	}
	if _, set := socketOptions(ClientConfig{LatencyMs: 120})["maxbw"]; set {
		t.Error("maxbw set without a configured cap; 0 must leave SRT uncapped")
	}
}

func TestSocketOptions(t *testing.T) {
	opts := socketOptions(ClientConfig{LatencyMs: 250, StreamID: "live/a", Passphrase: "0123456789"})
	want := map[string]string{
		"transtype":  "live",
		"latency":    "250",
		"streamid":   "live/a",
		"passphrase": "0123456789",
	}
	if len(opts) != len(want) {
		t.Fatalf("options = %v, want %v", opts, want)
	}
	for k, v := range want {
		if opts[k] != v {
			t.Errorf("option %s = %q, want %q", k, opts[k], v)
		}
	}
}

func TestParseURI(t *testing.T) {
	for _, tc := range []struct {
		uri  string
		host string
		port uint16
		ok   bool
	}{
		{"srt://example.org:9000", "example.org", 9000, true},
		{" 127.0.0.1:7000 ", "127.0.0.1", 7000, true},
		{"srt://[::1]:7000", "::1", 7000, true},
		{"srt://example.org", "", 0, false},
		{"srt://:7000", "", 0, false},
		{"srt://example.org:99999", "", 0, false},
		{"", "", 0, false},
	} {
		host, port, err := parseURI(tc.uri)
		if (err == nil) != tc.ok {
			t.Errorf("parseURI(%q) error = %v, want ok=%v", tc.uri, err, tc.ok)
			continue
		}
		if tc.ok && (host != tc.host || port != tc.port) {
			t.Errorf("parseURI(%q) = %s:%d, want %s:%d", tc.uri, host, port, tc.host, tc.port)
		}
	}
}
