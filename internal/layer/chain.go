package layer

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/go-gst/go-gst/pkg/gst"
)

// Chain is a layer's element chain in link order.
type Chain struct {
	// Elements are in link order. LinkChain links Elements[i].src into
	// Elements[i+1]'s compatible sink pad for every i < len-1; the last
	// element's "src" pad is the chain's output.
	Elements []gst.Element

	// DynamicSourceIndex is the index of the runtime-pad source (decodebin,
	// uridecodebin ...) in Elements, or -1 when the chain is fully static.
	// LinkChain connects the source chain to it, then connects its first
	// video pad to the tail's sink pad (first audio pad if
	// DynamicPadAudio is set).
	DynamicSourceIndex int

	// DynamicPadAudio selects the first audio pad of the dynamic source
	// instead of the first video pad (layer audio chains).
	DynamicPadAudio bool

	// SharedSource, when set, is a runtime-pad source the compositor has
	// ALREADY added to the pipeline — it belongs to a sibling chain (a
	// layer's video chain feeds its audio chain from the same source
	// element, so the stream is fetched once, not twice). It is not in
	// Elements; LinkChain hooks the shared source's matching pad onto
	// Elements[0]'s sink pad and links Elements statically.
	SharedSource gst.Element
}

// linkRange links Elements[start].src → Elements[i+1] for every
// start <= i < exclusiveEnd-1.
func (c *Chain) linkRange(start, exclusiveEnd int) error {
	for i := start; i+1 < exclusiveEnd; i++ {
		if !c.Elements[i].Link(c.Elements[i+1]) {
			return fmt.Errorf("layer: link %s -> %s failed", c.Elements[i].GetName(), c.Elements[i+1].GetName())
		}
	}
	return nil
}

// SrcPad returns the chain's output pad (the last element's "src").
func (c *Chain) SrcPad() gst.Pad {
	return c.Elements[len(c.Elements)-1].GetStaticPad("src")
}

// LinkChain links the elements of the chain in order. It must be called
// AFTER every element has been added to a bin: GStreamer 1.28's
// gst_bin_add breaks pad links made before insertion, so pre-linking a
// chain (before Add) silently unlinks the first hop.
func (c *Chain) LinkChain() error {
	if c.SharedSource != nil {
		// The shared source already sits in the bin (owned by the sibling
		// video chain); only the static tail is ours to link.
		if len(c.Elements) < 2 {
			return fmt.Errorf("layer: shared-source chain %s needs a source and a tail",
				c.Elements[0].GetName())
		}
		if err := c.linkRange(0, len(c.Elements)); err != nil {
			return err
		}
		var linked sync.Once
		sink := c.Elements[0].GetStaticPad("sink")
		if sink == nil {
			return fmt.Errorf("layer: shared-source tail %s has no sink pad", c.Elements[0].GetName())
		}
		linkPad := func(p gst.Pad) {
			linked.Do(func() {
				if r := p.Link(sink); r != gst.PadLinkOK {
					slog.Error("layer: shared source pad link failed",
						"source", c.SharedSource.GetName(), "pad", p.GetName(), "result", r.String())
				}
			})
		}
		// Runtime sources (decodebin and friends) add their pads during
		// preroll — after the video and audio chains of a layer have both
		// linked (the AddLayer command runs LinkChain on each before the
		// state change), so the pad-added handler always sees the pad.
		c.SharedSource.ConnectPadAdded(func(_ gst.Element, p gst.Pad) {
			if padIsKind(p, mediaTypeFor(c.DynamicPadAudio)) {
				linkPad(p)
			}
		})
		return nil
	}
	if c.DynamicSourceIndex < 0 {
		return c.linkRange(0, len(c.Elements))
	}
	tailStart := c.DynamicSourceIndex + 1
	if tailStart >= len(c.Elements) {
		return fmt.Errorf("layer: dynamic source %s has no tail elements", c.Elements[c.DynamicSourceIndex].GetName())
	}
	if err := c.linkRange(0, tailStart); err != nil {
		return err
	}
	if err := c.linkRange(tailStart, len(c.Elements)); err != nil {
		return err
	}

	dyn := c.Elements[c.DynamicSourceIndex]
	var linked sync.Once
	dyn.ConnectPadAdded(func(_ gst.Element, srcPad gst.Pad) {
		if !padIsKind(srcPad, mediaTypeFor(c.DynamicPadAudio)) {
			return
		}
		linked.Do(func() {
			sink := c.Elements[tailStart].GetStaticPad("sink")
			if sink == nil {
				slog.Error("layer: dynamic source tail has no sink pad", "source", dyn.GetName())
				return
			}
			if r := srcPad.Link(sink); r != gst.PadLinkOK {
				slog.Error("layer: dynamic pad link failed",
					"source", dyn.GetName(),
					"pad", srcPad.GetName(),
					"result", r.String())
			}
		})
	})
	return nil
}

// mediaTypeFor maps the DynamicPadAudio flag to the GStreamer media type the
// chain hooks.
func mediaTypeFor(wantAudio bool) string {
	if wantAudio {
		return "audio"
	}
	return "video"
}

// padIsKind reports whether the dynamic source pad carries a media stream of
// the given GStreamer media type ("video" or "audio"), by pad name prefix
// (decodebin names its dynamic pads video_0 / audio_0) or negotiated caps.
func padIsKind(pad gst.Pad, mediaType string) bool {
	if strings.HasPrefix(pad.GetName(), mediaType+"_") {
		return true
	}
	caps := pad.GetCurrentCaps()
	if caps == nil {
		caps = pad.GetAllowedCaps()
	}
	if caps == nil {
		return false
	}
	for i := 0; i < int(caps.GetSize()); i++ {
		if strings.HasPrefix(caps.GetStructure(uint(i)).GetName(), mediaType+"/") {
			return true
		}
	}
	return false
}
