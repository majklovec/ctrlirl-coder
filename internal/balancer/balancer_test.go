package balancer

import "testing"

func TestFactory(t *testing.T) {
	p := Params{
		MinBitrate:   500_000,
		MaxBitrate:   6_000_000,
		SRTLatencyMs: 2000,
		SRTPktSize:   1316,
	}

	for name, want := range map[string]string{
		"adaptive": "adaptive",
		"fixed":    "fixed",
		"aimd":     "aimd",
		"":         "adaptive",
		"bogus":    "adaptive",
	} {
		if got := New(name, p).Name(); got != want {
			t.Errorf("New(%q).Name() = %q, want %q", name, got, want)
		}
	}
}
