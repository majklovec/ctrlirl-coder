package video

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/layer"
	"ctrlirl-coder/internal/scene"
)

// VideoSource is a live video layer: an SRT/device/file/test source chain
// feeding the compositor. Media sources that can carry an audio track
// (SRT, HTTP/HTTPS, local file) additionally expose a raw-PCM audio chain
// (AudioChain) that the compositor mixes into the shared mpegtsmux tail.
// The testsrc://red and testsrc://blue sources produce a matching sine tone.
type VideoSource struct {
	layer.BaseLayer

	// audioSource is the layer's own audio source chain (ending in a
	// runtime-pad element), or nil for sources without one of their own
	// (test pattern, /dev/ capture, HTTP — see sharedSource).
	audioSource []layer.Spec
	// sharedSource is the decodebin the video chain already owns (HTTP/HTTPS
	// sources). The audio chain hooks its audio pad instead of fetching the
	// stream a second time — two independent live fetchers each buffer and
	// resync on their own, so the A/V offset between them drifts without
	// bound (audible audio glitches and video artifacts at the same moments,
	// audio running ahead of video). One source, one running time.
	sharedSource gst.Element
	testToneHz   float64
}

// NewVideoSource builds a video layer.
func NewVideoSource(l scene.Layer) *VideoSource {
	return &VideoSource{BaseLayer: layer.NewBaseLayer(l, scene.LayerTypeVideo)}
}

// spec names an element of this layer: "<layer id>-<suffix>".
func (v *VideoSource) spec(factory, suffix string, props map[string]any) layer.Spec {
	return layer.Spec{Factory: factory, Name: v.L.ID + "-" + suffix, Props: props}
}

// BuildChain selects the source for the layer's `source` and returns the
// chain source → [decodebin] → [filters] → videoconvert → videoscale →
// capsfilter(BGRA) → queue.
//
// Elements are returned unlinked: the compositor adds them to the pipeline
// and then links the chain (GStreamer 1.28's gst_bin_add breaks links made
// before insertion).
func (v *VideoSource) BuildChain() (*layer.Chain, error) {
	var (
		head    []gst.Element
		err     error
		dynamic = -1 // index of the runtime-pad source in head
		source  = strings.TrimSpace(v.L.Source)
	)
	switch {
	case strings.HasPrefix(source, "testsrc:"):
		pattern, frequency, perr := parseTestSource(source)
		if perr != nil {
			return nil, fmt.Errorf("layer %s: %w", v.L.ID, perr)
		}
		v.testToneHz = frequency
		head, err = layer.Build(v.spec("videotestsrc", "videotestsrc", map[string]any{
			"is-live": true,
			"pattern": int32(pattern),
		}))

	case source == "":
		// No source: test pattern (useful for smoke tests).
		head, err = layer.Build(v.spec("videotestsrc", "videotestsrc", map[string]any{"is-live": true}))

	case isSRTSource(source):
		head, err = layer.Build(v.spec("srtsrc", "srtsrc", map[string]any{"uri": source}))
		// "aud" name infix: pipeline element names must be unique and the
		// video chain already claims <id>-srtsrc.
		v.audioSource = []layer.Spec{v.spec("srtsrc", "audsrtsrc", map[string]any{"uri": source})}

	case strings.HasPrefix(source, "/dev/"):
		head, err = layer.Build(v.spec("v4l2src", "v4l2src", map[string]any{"device": source}))

	case isHTTPSource(source):
		// Pre-buffer after the decodebin: absorbs decode/network hitches
		// (queue leaks downstream, never drops). The periodic 10 s content
		// hole is fixed elsewhere — the hlsdemux2 download watermark clamp
		// (compositor/hls_watermark.go); a queue cannot invent content the
		// demux skips (verified by A/B capture).
		prebuf := v.spec("queue", "prebuf", map[string]any{"max-size-buffers": 45})
		uri := map[string]any{"uri": source}
		head, err = layer.Build(v.spec("uridecodebin3", "uridecodebin3", uri), prebuf)
		if err != nil {
			head, err = layer.Build(v.spec("uridecodebin", "uridecodebin", uri), prebuf)
		}
		if err == nil {
			dynamic = 0
			// One uridecodebin feeds BOTH chains: its video_0 pad drives the
			// video tail, its audio_0 pad the layer audio chain (see
			// AudioChain). A second fetcher for the audio would drift the
			// A/V offset live — the source element is shared, not duplicated.
			v.sharedSource = head[0]
		}

	default: // a local file
		head, err = layer.Build(
			v.spec("filesrc", "filesrc", map[string]any{"location": source}),
			v.spec("decodebin", "decodebin", nil),
		)
		dynamic = 1
		v.audioSource = []layer.Spec{
			v.spec("filesrc", "audfilesrc", map[string]any{"location": source}),
			v.spec("decodebin", "auddecodebin", nil),
		}
	}
	if err != nil {
		return nil, err
	}

	chain, err := layer.SurfaceChain(v.L, head...)
	if err != nil {
		return nil, err
	}
	chain.DynamicSourceIndex = dynamic
	return chain, nil
}

// audioTail is the [audioconvert, queue] every audio chain of this layer
// ends in; prefix keeps the names of the tone and the media variant apart.
func (v *VideoSource) audioTail(prefix string) []layer.Spec {
	return []layer.Spec{
		v.spec("audioconvert", prefix+"conv", nil),
		v.spec("queue", prefix+"queue", map[string]any{"max-size-buffers": 60}),
	}
}

// AudioChain returns the layer's raw-PCM audio chain for media sources that
// carry an audio track (SRT, HTTP/HTTPS, local file): the source (audio pad
// of the decodebin) → audioconvert → queue. The compositor mixes all layer
// chains into one shared encoder + mpegtsmux pad. The first audio pad is
// used (the source may not negotiate its audio track until runtime).
// Returns (nil, nil) for audio-less layers (test pattern, /dev/ capture).
func (v *VideoSource) AudioChain() (*layer.Chain, error) {
	switch {
	case v.testToneHz > 0:
		tone := v.spec("audiotestsrc", "tone", map[string]any{
			"is-live": true,
			"wave":    int32(0),
			"freq":    v.testToneHz,
		})
		els, err := layer.Build(slices.Concat([]layer.Spec{tone}, v.audioTail("tone-"))...)
		if err != nil {
			return nil, err
		}
		return layer.StaticChain(els...), nil

	case v.sharedSource != nil:
		// The video chain already added the decodebin to the pipeline; hook
		// its audio_0 pad onto this tail (Chain.SharedSource). No second
		// fetcher: one network stream, one running time, A/V can't drift.
		//
		// hlsdemux2 splices each 10 s TS segment with a ~0.3 s per-segment audio
		// PTS offset, bridging it with digital silence in a real-time (live)
		// pipeline. The shared mixer (liveadder, see compositor) passes the
		// samples through, and this queue spans the small offset so the resync
		// is a bounded jitter, not a running-time break. (A full VOD pre-buffer
		// of this source decodes with no such offset — see IMPLEMENTATION.md —
		// so no downstream resampler can recover the bridged samples.)
		tail, err := layer.Build(v.audioTail("aud")...)
		if err != nil {
			return nil, err
		}
		return &layer.Chain{Elements: tail, SharedSource: v.sharedSource, DynamicPadAudio: true}, nil

	case len(v.audioSource) > 0:
		els, err := layer.Build(slices.Concat(v.audioSource, v.audioTail("aud"))...)
		if err != nil {
			return nil, err
		}
		return &layer.Chain{
			Elements:           els,
			DynamicSourceIndex: len(v.audioSource) - 1,
			DynamicPadAudio:    true,
		}, nil
	}
	return nil, nil
}

func parseTestSource(source string) (int, float64, error) {
	u, err := url.Parse(source)
	if err != nil || u.Scheme != "testsrc" || u.Path != "" || u.Fragment != "" {
		return 0, 0, fmt.Errorf("invalid test source URI %q", source)
	}
	patterns := map[string]int{"red": 4, "blue": 6}
	pattern, ok := patterns[u.Hostname()]
	if !ok {
		return 0, 0, fmt.Errorf("test source color must be red or blue")
	}
	if u.User != nil || strings.Contains(u.Host, ":") {
		return 0, 0, fmt.Errorf("test source URI must not include user info or port")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid test source query: %w", err)
	}
	for key := range query {
		if key != "freq" {
			return 0, 0, fmt.Errorf("unsupported test source parameter %q", key)
		}
	}
	frequency := 440.0
	if values, exists := query["freq"]; exists {
		if len(values) != 1 || values[0] == "" {
			return 0, 0, fmt.Errorf("test source frequency must be specified once")
		}
		raw := values[0]
		frequency, err = strconv.ParseFloat(raw, 64)
		if err != nil || !(frequency >= 20 && frequency <= 20000) {
			return 0, 0, fmt.Errorf("test source frequency must be between 20 and 20000 Hz")
		}
	}
	return pattern, frequency, nil
}

func isSRTSource(source string) bool {
	return strings.HasPrefix(source, "srt://") || strings.HasPrefix(source, "urisrt://")
}

func isHTTPSource(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}
