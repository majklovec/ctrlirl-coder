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
//
// A chain fed by a runtime-pad source is linked in two static runs — up to
// the source, and from the element behind it — and the gap is closed when
// the source adds its pad.
func (c *Chain) LinkChain() error {
	var (
		source    gst.Element // runtime-pad source feeding Elements[tailStart]
		tailStart int
	)
	switch {
	case c.SharedSource != nil:
		// The shared source already sits in the bin (owned by the sibling
		// video chain); only the static tail is ours to link.
		if len(c.Elements) < 2 {
			return fmt.Errorf("layer: shared-source chain %s needs a source and a tail",
				c.Elements[0].GetName())
		}
		source = c.SharedSource
	case c.DynamicSourceIndex >= 0:
		source, tailStart = c.Elements[c.DynamicSourceIndex], c.DynamicSourceIndex+1
		if tailStart >= len(c.Elements) {
			return fmt.Errorf("layer: dynamic source %s has no tail elements", source.GetName())
		}
		if err := c.linkRange(0, tailStart); err != nil {
			return err
		}
	default:
		return c.linkRange(0, len(c.Elements))
	}
	if err := c.linkRange(tailStart, len(c.Elements)); err != nil {
		return err
	}
	return c.linkDynamicPad(source, c.Elements[tailStart])
}

// linkDynamicPad links the first pad of the chain's media kind that source
// adds at runtime to tail's sink pad. Runtime sources (decodebin and
// friends) add their pads during preroll — after the video and audio chains
// of a layer have both been linked (AddLayer runs LinkChain on each before
// the state change), so the handler always sees the pad.
func (c *Chain) linkDynamicPad(source, tail gst.Element) error {
	sink := tail.GetStaticPad("sink")
	if sink == nil {
		return fmt.Errorf("layer: %s has no sink pad for the dynamic source %s", tail.GetName(), source.GetName())
	}
	kind := mediaTypeFor(c.DynamicPadAudio)
	var linked sync.Once
	source.ConnectPadAdded(func(_ gst.Element, pad gst.Pad) {
		if !padIsKind(pad, kind) {
			return
		}
		linked.Do(func() {
			if r := pad.Link(sink); r != gst.PadLinkOK {
				slog.Error("layer: dynamic pad link failed",
					"source", source.GetName(), "pad", pad.GetName(), "result", r.String())
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
