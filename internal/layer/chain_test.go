package layer_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/layer"
	"ctrlirl-coder/internal/layer/text"
	"ctrlirl-coder/internal/layer/video"
	"ctrlirl-coder/internal/scene"
)

func TestMain(m *testing.M) {
	gst.Init()
	os.Exit(m.Run())
}

// TestVideoAudioChains verifies which video layer sources expose an audio
// chain (SRT, HTTP, local file) and that the chain selects its dynamic
// source's first AUDIO pad (DynamicPadAudio) — the HLS-source no-sound fix.
func TestVideoAudioChains(t *testing.T) {
	file := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(file, []byte{0}, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cases := []struct {
		name       string
		source     string
		wantNil    bool
		wantLen    int
		wantDynIx  int
		wantShared bool
	}{
		{"testpattern", "", true, 0, 0, false},
		{"v4l2", "/dev/video0", true, 0, 0, false},
		{"srt", "srt://127.0.0.1:9000", false, 3, 0, false},
		// http audio chain: [audioconvert, queue] — the queue spans the
		// hlsdemux2 per-segment audio PTS offset in the live pipeline so the
		// liveadder mix sees bounded jitter, not a running-time break.
		{"http", "https://test-streams.mux.dev/x36xhzz/x36xhzz.m3u8", false, 2, 0, true},
		{"file", file, false, 4, 1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := video.NewVideoSource(scene.Layer{
				ID: "vid", Type: scene.LayerTypeVideo, Source: tc.source,
			})
			if _, err := v.BuildChain(); err != nil {
				t.Fatalf("BuildChain: %v", err)
			}
			chain, err := v.AudioChain()
			if tc.wantNil {
				if err != nil || chain != nil {
					t.Fatalf("AudioChain = (%v, %v), want (nil, nil)", chain, err)
				}
				return
			}
			if err != nil || chain == nil {
				t.Fatalf("AudioChain = (%v, %v), want chain", chain, err)
			}
			if len(chain.Elements) != tc.wantLen {
				t.Fatalf("chain has %d elements, want %d", len(chain.Elements), tc.wantLen)
			}
			if chain.DynamicSourceIndex != tc.wantDynIx {
				t.Fatalf("DynamicSourceIndex = %d, want %d", chain.DynamicSourceIndex, tc.wantDynIx)
			}
			if (chain.SharedSource != nil) != tc.wantShared {
				t.Fatalf("SharedSource = %v, want shared=%v", chain.SharedSource, tc.wantShared)
			}
			if !chain.DynamicPadAudio {
				t.Fatal("DynamicPadAudio not set")
			}
			if err := chain.LinkChain(); err != nil {
				t.Fatalf("LinkChain: %v", err)
			}
			// The static tail (audioconvert → queue) must be linked; the
			// dynamic pad links at runtime when the source adds it.
			conv := chain.Elements[len(chain.Elements)-2]
			if conv.GetStaticPad("src").GetPeer() == nil {
				t.Fatalf("%s src pad not linked", conv.GetName())
			}
		})
	}
}

// TestDynamicPadSelection verifies that a DynamicPadAudio chain hooks the
// source's audio pad and ignores video pads, and vice versa on video
// chains. The fake source adds decodebin-style pads (video_0, audio_0)
// only after LinkChain registered the pad-added handler, like a real
// decodebin does at runtime.
func TestDynamicPadSelection(t *testing.T) {
	run := func(t *testing.T, wantAudio bool) {
		src, addPads := makeFakeDynamicSource(t)
		sink, err := layer.MakeElement("fakesink", "dynsink", nil)
		if err != nil {
			t.Fatal(err)
		}
		chain := &layer.Chain{Elements: []gst.Element{src, sink}, DynamicSourceIndex: 0, DynamicPadAudio: wantAudio}
		if err := chain.LinkChain(); err != nil {
			t.Fatalf("LinkChain: %v", err)
		}
		vp, ap := addPads()
		want, dont := ap, vp
		if !wantAudio {
			want, dont = vp, ap
		}
		if want.GetPeer() == nil {
			t.Fatalf("%s pad not linked", want.GetName())
		}
		if dont.GetPeer() != nil {
			t.Fatalf("%s pad must stay unlinked", dont.GetName())
		}
	}
	t.Run("audio chain takes audio pad", func(t *testing.T) { run(t, true) })
	t.Run("video chain takes video pad", func(t *testing.T) { run(t, false) })
}

// TestSharedSourceChain verifies the shared-source audio chain: the static
// tail links, the shared runtime source's audio pad hooks onto the tail
// head, and the video pad is left alone — in the compositor's real order
// (video chain Linked before the audio chain, both before the source adds
// its pads at preroll).
func TestSharedSourceChain(t *testing.T) {
	src, addPads := makeFakeDynamicSource(t)
	vconv, err := layer.MakeElement("videoconvert", "shared-vconv", nil)
	if err != nil {
		t.Fatal(err)
	}
	vq, err := layer.MakeElement("queue", "shared-vq", nil)
	if err != nil {
		t.Fatal(err)
	}
	vchain := &layer.Chain{Elements: []gst.Element{vconv, vq}, SharedSource: src}
	if err := vchain.LinkChain(); err != nil {
		t.Fatalf("video LinkChain: %v", err)
	}
	aconv, err := layer.MakeElement("audioconvert", "shared-aconv", nil)
	if err != nil {
		t.Fatal(err)
	}
	aq, err := layer.MakeElement("queue", "shared-aq", nil)
	if err != nil {
		t.Fatal(err)
	}
	achain := &layer.Chain{
		Elements: []gst.Element{aconv, aq}, SharedSource: src, DynamicPadAudio: true,
		DynamicSourceIndex: -1,
	}
	if err := achain.LinkChain(); err != nil {
		t.Fatalf("audio LinkChain: %v", err)
	}
	// Static tails are linked immediately.
	if vconv.GetStaticPad("src").GetPeer() == nil || aconv.GetStaticPad("src").GetPeer() == nil {
		t.Fatal("static tail not linked")
	}
	// Runtime pads appear after both chains linked, like a decodebin at preroll.
	vp, ap := addPads()
	if vp.GetPeer() == nil {
		t.Fatal("shared source video_0 pad not linked")
	}
	if ap.GetPeer() == nil {
		t.Fatal("shared source audio_0 pad not linked")
	}
	// Each hook lands on its own tail head.
	if got := vp.GetPeer(); got == nil || aconv.GetStaticPad("sink").GetPeer().GetName() != "audio_0" {
		t.Fatalf("audio pad linked into the wrong tail (peer=%v)", got)
	}
}

// makeFakeDynamicSource builds a bin (no pad template constraints) and
// returns it with a closure that adds two src pads named like a
// decodebin's (video_0, audio_0) on demand.
func makeFakeDynamicSource(t *testing.T) (gst.Element, func() (gst.Pad, gst.Pad)) {
	t.Helper()
	f, err := layer.MakeElement("bin", "fakedyn", nil)
	if err != nil {
		t.Fatalf("bin: %v", err)
	}
	add := func() (gst.Pad, gst.Pad) {
		vp := gst.NewPad("video_0", gst.PadSrc)
		ap := gst.NewPad("audio_0", gst.PadSrc)
		if !f.AddPad(vp) || !f.AddPad(ap) {
			t.Fatal("adding fake dynamic pads failed")
		}
		return vp, ap
	}
	return f, add
}

// TestStaticChainsLinkAllPairs is a regression test for the
// Chain.DynamicSourceIndex zero-value trap: int's zero value is 0, which
// LinkChain interprets as "dynamic source at index 0" and silently skips
// the first link of the chain. Static chains must carry DynamicSourceIndex
// = -1 so every consecutive pair gets linked.
func TestStaticChainsLinkAllPairs(t *testing.T) {
	cases := []struct {
		name string
		l    layer.Layer
	}{
		{"text", text.NewTextOverlay(scene.Layer{ID: "title", Type: scene.LayerTypeText, Text: "hi"})},
		{"video-static", video.NewVideoSource(scene.Layer{ID: "vid", Type: scene.LayerTypeVideo})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chain, err := tc.l.BuildChain()
			if err != nil {
				t.Fatalf("BuildChain: %v", err)
			}
			if len(chain.Elements) < 3 {
				t.Fatalf("expected a multi-element chain, got %d", len(chain.Elements))
			}
			if err := chain.LinkChain(); err != nil {
				t.Fatalf("LinkChain: %v", err)
			}
			for i := 0; i+1 < len(chain.Elements); i++ {
				srcPad := chain.Elements[i].GetStaticPad("src")
				if srcPad == nil {
					t.Fatalf("element %d (%s) has no src pad", i, chain.Elements[i].GetName())
				}
				if peer := srcPad.GetPeer(); peer == nil {
					t.Fatalf("element %d (%s) src pad is not linked to element %d (%s)",
						i, chain.Elements[i].GetName(), i+1, chain.Elements[i+1].GetName())
				}
			}
		})
	}
}
