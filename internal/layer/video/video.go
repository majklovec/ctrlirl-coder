package video

import (
	"fmt"
	"net/url"
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

	// audioSpecs is the dynamic audio source of the layer, or nil for
	// video-only sources (test pattern, /dev/ capture).
	audioSpecs []string
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

// BuildChain selects the source element for the layer's source and links the
// full chain source → [decodebin] → videoconvert → capsfilter(BGRA) → queue.
func (v *VideoSource) BuildChain() (*layer.Chain, error) {
	var (
		dynamicIndex = -1
		audioSpecs   []string
		err          error
		srcs         []gst.Element
		source       = strings.TrimSpace(v.L.Source)
	)
	switch {
	case strings.HasPrefix(source, "testsrc:"):
		pattern, frequency, parseErr := parseTestSource(source)
		if parseErr != nil {
			return nil, fmt.Errorf("layer %s: %w", v.L.ID, parseErr)
		}
		v.testToneHz = frequency
		srcs, err = v.sourceChain([]string{"videotestsrc:videotestsrc"}, map[string]any{
			"is-live": true,
			"pattern": int32(pattern),
		})
	case source == "":
		// No source pin: test pattern (useful for smoke tests).
		srcs, err = v.sourceChain([]string{"videotestsrc:videotestsrc"}, map[string]any{"is-live": true})
	case strings.HasPrefix(source, "srt://") || strings.HasPrefix(source, "urisrt://"):
		srcs, err = v.sourceChain([]string{"srtsrc:srtsrc"}, map[string]any{"uri": source})
		// "aud" name infix: pipeline element names must be unique and the
		// video chain already claims <id>-srtsrc.
		audioSpecs = []string{"srtsrc:audsrtsrc"}
	case strings.HasPrefix(source, "/dev/"):
		srcs, err = v.sourceChain([]string{"v4l2src:v4l2src"}, map[string]any{"device": source})
	case isHTTPSource(source):
		srcs, err = v.sourceChain([]string{"uridecodebin3:uridecodebin3"}, map[string]any{"uri": source})
		if err != nil {
			srcs, err = v.sourceChain([]string{"uridecodebin:uridecodebin"}, map[string]any{"uri": source})
		}
		if err == nil {
			dynamicIndex = 0
			// One uridecodebin feeds BOTH chains: its video_0 pad drives the
			// video tail, its audio_0 pad the layer audio chain (see
			// AudioChain). A second fetcher for the audio would drift the
			// A/V offset live — the source element is shared, not duplicated.
			v.sharedSource = srcs[0]
			// Pre-buffer after the decodebin: absorbs decode/network hitches
			// (queue leaks downstream, never drops). The periodic 10 s
			// content hole is fixed elsewhere — the hlsdemux2 download
			// watermark clamp (compositor/hls_watermark.go); a queue cannot
			// invent content the demux skips (verified by A/B capture).
			prebuf, pErr := layer.MakeElement("queue", v.L.ID+"-prebuf", map[string]any{"max-size-buffers": 45})
			if pErr != nil {
				return nil, pErr
			}
			srcs = append(srcs, prebuf)
		}
	default:
		srcs, err = v.sourceChain([]string{"filesrc:filesrc", "decodebin:decodebin"}, map[string]any{"location": source})
		if err == nil {
			dynamicIndex = len(srcs) - 1
		}
		audioSpecs = []string{"filesrc:audfilesrc", "decodebin:auddecodebin"}
	}
	if err != nil {
		return nil, err
	}
	v.audioSpecs = audioSpecs

	// Elements are returned unlinked: the compositor adds them to the
	// pipeline and then links the chain (GStreamer 1.28's gst_bin_add breaks
	// links made before insertion). Filters land between the source and the
	// videoconvert tail.
	// The pre-filter videoconvert (present when the layer has video
	// filters) lives in VideoFilters: decoders emit I420/Y420, and
	// `rotate` refuses those — without the convert the link fails, the
	// layer pad never produces a buffer, and the compositor (which needs
	// every pad) goes fully black.
	chain, err := layer.SurfaceChain(v.L, srcs...)
	if err != nil {
		return nil, err
	}
	chain.DynamicSourceIndex = dynamicIndex
	return chain, nil
}

// AudioChain returns the layer's raw-PCM audio chain for media sources that
// carry an audio track (SRT, HTTP/HTTPS, local file): the source (audio pad
// of the decodebin) → audioconvert → queue. The compositor mixes all layer
// chains into one shared encoder + mpegtsmux pad. The first audio pad is
// used (the source may not negotiate its audio track until runtime).
// Returns (nil, nil) for audio-less layers (test pattern, /dev/ capture).
func (v *VideoSource) AudioChain() (*layer.Chain, error) {
	if v.testToneHz > 0 {
		src, err := layer.MakeElement("audiotestsrc", v.L.ID+"-tone", map[string]any{
			"is-live": true,
			"wave":    int32(0),
			"freq":    v.testToneHz,
		})
		if err != nil {
			return nil, err
		}
		conv, err := layer.MakeElement("audioconvert", v.L.ID+"-tone-conv", nil)
		if err != nil {
			return nil, err
		}
		q, err := layer.MakeElement("queue", v.L.ID+"-tone-queue", map[string]any{"max-size-buffers": 60})
		if err != nil {
			return nil, err
		}
		return &layer.Chain{Elements: []gst.Element{src, conv, q}, DynamicSourceIndex: -1}, nil
	}
	if v.sharedSource == nil && len(v.audioSpecs) == 0 {
		return nil, nil
	}
	fail := func(format string, args ...any) (*layer.Chain, error) {
		return nil, fmt.Errorf(format, args...)
	}
	conv, err := layer.MakeElement("audioconvert", v.L.ID+"-audconv", nil)
	if err != nil {
		return fail("layer: %v", err)
	}
	q, err := layer.MakeElement("queue", v.L.ID+"-audqueue", map[string]any{"max-size-buffers": 60})
	if err != nil {
		return fail("layer: %v", err)
	}
	if v.sharedSource != nil {
		// The video chain already added the decodebin to the pipeline; hook
		// its audio_0 pad onto this head (Chain.SharedSource). No second
		// fetcher: one network stream, one running time, A/V can't drift.
		//
		// hlsdemux2 splices each 10 s TS segment with a ~0.3 s per-segment audio
		// PTS offset, bridging it with digital silence in a real-time (live)
		// pipeline. The shared mixer (liveadder, see compositor) passes the
		// samples through, and this queue spans the small offset so the resync
		// is a bounded jitter, not a running-time break. (A full VOD pre-buffer
		// of this source decodes with no such offset — see IMPLEMENTATION.md —
		// so no downstream resampler can recover the bridged samples.)
		return &layer.Chain{
			Elements:        []gst.Element{conv, q},
			SharedSource:    v.sharedSource,
			DynamicPadAudio: true,
		}, nil
	}
	srcs, err := v.sourceChain(v.audioSpecs, audioSourceProps(v.L.Source))
	if err != nil {
		return fail("layer: %v", err)
	}
	return &layer.Chain{
		Elements:           append(srcs, conv, q),
		DynamicSourceIndex: len(srcs) - 1,
		DynamicPadAudio:    true,
	}, nil
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

// audioSourceProps selects the per-source element properties of an audio
// chain: the source element is the first one in the chain, so filesrc gets
// its location and srtsrc/uridecodebin get their URI.
func audioSourceProps(source string) map[string]any {
	switch {
	case strings.HasPrefix(source, "srt://") || strings.HasPrefix(source, "urisrt://"):
		return map[string]any{"uri": source}
	case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
		return map[string]any{"uri": source}
	default:
		return map[string]any{"location": source}
	}
}

func isHTTPSource(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

// sourceChain builds the given source elements (factory:name pairs) and
// applies the factory-keyed properties to the first element.
func (v *VideoSource) sourceChain(specs []string, props map[string]any) ([]gst.Element, error) {
	els := make([]gst.Element, 0, len(specs))
	for i, spec := range specs {
		parts := strings.SplitN(spec, ":", 2)
		factory, name := parts[0], v.L.ID+"-"+parts[1]
		el, err := layer.MakeElement(factory, name, nil)
		if err != nil {
			return nil, err
		}
		if i == 0 && len(props) > 0 {
			el.SetObjectProperties(props)
		}
		els = append(els, el)
	}
	return els, nil
}
