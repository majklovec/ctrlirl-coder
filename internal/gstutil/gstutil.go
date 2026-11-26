// Package gstutil wraps the go-gst v1.4.1 generated API with the small,
// typed helpers the rest of ctrlirl-coder needs. It is the only layer that
// is expected to change if we ever move to a different GStreamer binding.
package gstutil

import (
	"fmt"
	"strings"
	"sync"

	"github.com/go-gst/go-gst/pkg/gst"
)

// initOnce ensures gst.Init() (plugin registry load) runs exactly once,
// before any element factory lookup.
var initOnce sync.Once

// EnsureInit initializes the GStreamer library (loads the plugin registry).
// Safe to call multiple times.
func EnsureInit() {
	initOnce.Do(func() {
		gst.Init()
		// Hardware decoders can be installed without an active device in
		// headless builds. Demote them so decodebin prefers software H264
		// fallback instead of trying CUDA/VA/Vulkan memory sinks it cannot use.
		registry := gst.RegistryGet()
		for _, name := range []string{
			"nvh264dec",
			"vah264dec",
			"vaapih264dec",
			"vulkanh264dec",
			"vulkanh264device1dec",
			"qsvh264dec",
			"msdkh264dec",
			"v4l2h264dec",
			"v4l2slh264dec",
		} {
			if feature := registry.LookupFeature(name); feature != nil {
				feature.SetRank(uint(gst.RankNone))
			}
		}
	})
}

// Pipeline is a typed handle over an already-parsed GStreamer pipeline.
type Pipeline struct {
	el gst.Pipeline
}

// FromLaunch parses a gst-launch description into a pipeline held in the
// NULL state. The resulting pipeline is owned by the caller.
//
// Unlike the gst-launch-1.0 tool, the C API gst_parse_launch does not accept
// '#' comment lines, so they are stripped here (the templates use them).
func FromLaunch(launch string) (*Pipeline, error) {
	EnsureInit()
	el, err := gst.ParseLaunch(stripComments(launch))
	if err != nil {
		return nil, fmt.Errorf("parse launch: %w", err)
	}
	pip, ok := el.(gst.Pipeline)
	if !ok {
		return nil, fmt.Errorf("parsed element %q is not a Pipeline", el.GetName())
	}
	return &Pipeline{el: pip}, nil
}

// As exposes the underlying gst.Pipeline for callers that need direct access.
func (p *Pipeline) As() gst.Pipeline { return p.el }

// SetState puts the pipeline into the given state, blocking until the
// transition completes, returning an error on failure.
func (p *Pipeline) SetState(s gst.State) error {
	return SetStateSafe(p.el, s)
}

// stripComments removes whole-line '#' comments from a launch description
// (matching gst-launch-1.0's file syntax).
func stripComments(launch string) string {
	var b strings.Builder
	for _, line := range strings.Split(launch, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// SetStateSafe sets an element to the given state, blocking until the
// transition completes. Any failure is returned as an error.
//
// The blocking set-state return value is authoritative: a live pipeline
// (one that contains a live source, e.g. an appsrc or pulsesrc) returns
// StateChangeNoPreroll because it reaches PLAYING without waiting for a
// buffer, while a non-live pipeline returns StateChangeSuccess once
// preroll completes. Both are success. A spurious re-read via GetState
// cannot be used here: for a live pipeline mid-transition (PAUSED with
// pending PLAYING) GetState reports StateChangeFailure even though the
// transition is healthy, which would wrongly fail startup.
func SetStateSafe(el gst.Element, s gst.State) error {
	if el == nil {
		return fmt.Errorf("cannot set state on nil element")
	}
	ret := el.SetState(s)
	if ret == gst.StateChangeFailure {
		return fmt.Errorf("state change to %v failed (ret=%v)", s, ret)
	}
	return nil
}
