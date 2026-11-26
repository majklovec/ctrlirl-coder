package video

import (
	"testing"

	"ctrlirl-coder/internal/gstutil"
	"ctrlirl-coder/internal/scene"
)

func TestIsHTTPSource(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		{"http://example.com/stream.m3u8", true},
		{"https://example.com/stream.m3u8", true},
		{"srt://localhost:7000", false},
		{"", false},
		{"video.mp4", false},
	}

	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			if got := isHTTPSource(tt.source); got != tt.want {
				t.Fatalf("isHTTPSource(%q) = %v, want %v", tt.source, got, tt.want)
			}
		})
	}
}

func TestParseTestSource(t *testing.T) {
	tests := []struct {
		name        string
		source      string
		wantPattern int
		wantTone    float64
		wantErr     bool
	}{
		{name: "red", source: "testsrc://red?freq=440", wantPattern: 4, wantTone: 440},
		{name: "blue", source: "testsrc://blue?freq=660", wantPattern: 6, wantTone: 660},
		{name: "default tone", source: "testsrc://red", wantPattern: 4, wantTone: 440},
		{name: "unsupported color", source: "testsrc://green", wantErr: true},
		{name: "invalid frequency", source: "testsrc://red?freq=0", wantErr: true},
		{name: "unknown parameter", source: "testsrc://red?other=1", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pattern, tone, err := parseTestSource(tt.source)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseTestSource(%q) error = %v, wantErr %v", tt.source, err, tt.wantErr)
			}
			if err == nil && (pattern != tt.wantPattern || tone != tt.wantTone) {
				t.Fatalf("parseTestSource(%q) = (%d, %g), want (%d, %g)",
					tt.source, pattern, tone, tt.wantPattern, tt.wantTone)
			}
		})
	}
}

func TestTestSourceBuildsVideoAndToneChains(t *testing.T) {
	gstutil.EnsureInit()
	for _, source := range []string{
		"testsrc://red?freq=440",
		"testsrc://blue?freq=660",
	} {
		t.Run(source, func(t *testing.T) {
			v := NewVideoSource(scene.Layer{
				ID:     "test-video",
				Type:   scene.LayerTypeVideo,
				Source: source,
				Properties: map[string]float64{
					"width": 1280, "height": 720,
				},
			})
			video, err := v.BuildChain()
			if err != nil {
				t.Fatalf("BuildChain: %v", err)
			}
			if video == nil || len(video.Elements) == 0 {
				t.Fatal("BuildChain returned empty video chain")
			}
			audio, err := v.AudioChain()
			if err != nil {
				t.Fatalf("AudioChain: %v", err)
			}
			if audio == nil || len(audio.Elements) != 3 {
				t.Fatalf("AudioChain returned %+v, want source/convert/queue", audio)
			}
		})
	}
}
