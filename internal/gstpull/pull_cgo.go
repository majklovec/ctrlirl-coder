// Cgo bindings to the GStreamer *app* library (libgstapp): draining the
// terminal appsink with gst_app_sink_pull_sample (as the original
// ctrlirl_coder/belacoder does), plus the caps helper
// (gst_pad_get_current_caps / gst_structure_get_fraction) for the sink
// framerate.
//
// Every GStreamer entry point is wrapped in a small C function that takes
// opaque `void*`. cgo can only pass `unsafe.Pointer` to/from C pointer types,
// so routing through `void*` keeps the Go side free of the generated
// `*_Ctype_struct_...` types.
//
// All of these run on the Go drain goroutine (the pull model), never on
// GStreamer's streaming thread, so blocking here is safe. The appsink element
// stays owned by go-gst; we only borrow its raw pointer during each C call.
package gstpull

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0
#include <stdlib.h>
#include <gst/gst.h>
#include <gst/app/gstappsink.h>

// Drain state kept between calls: a sample that was pulled but did not fit
// the caller's buffer waits here for the next call.
typedef struct {
	GstSample *pending;
} GstpullState;

// One drained batch.
typedef struct {
	gsize bytes;      // payload bytes copied into dst
	guint buffers;    // buffers those bytes came from
	guint64 pts;      // PTS of the last copied buffer that carried one
	gboolean has_pts; // pts is set
	gsize need;       // > 0: the next buffer needs a dst this large (nothing copied)
	gboolean eos;     // the pull returned NULL with nothing copied: EOS or NULL state
} GstpullBatch;

GstpullState *gstpull_state_new(void) {
	return g_new0(GstpullState, 1);
}

void gstpull_state_free(GstpullState *st) {
	if (st == NULL) {
		return;
	}
	if (st->pending != NULL) {
		gst_sample_unref(st->pending);
	}
	g_free(st);
}

// Drain the appsink into dst in ONE call: block for the first buffer, then
// keep taking buffers until dst is full, the appsink has been empty for
// linger_us, or hold_us has passed since the first buffer. Payloads are
// copied straight into dst (which may be Go memory: it is only touched
// during this call), so the caller crosses into C once per batch instead of
// twice per buffer.
//
// A buffer that does not fit the remaining room is parked in st and opens
// the next batch; if it does not fit an EMPTY dst, need reports its size so
// the caller can grow dst and call again.
//
// The blocking pull returns NULL only at EOS or once the element is in NULL
// (a READY freeze keeps it blocked), so eos means the drain is over. A NULL
// from the timed pull just ends the batch; the next call's blocking pull
// tells idle from EOS.
GstpullBatch gstpull_drain(void *sink, GstpullState *st, unsigned char *dst, gsize cap,
		gint64 linger_us, gint64 hold_us) {
	GstpullBatch out = {0};
	GstAppSink *appsink = (GstAppSink *)sink;
	gint64 deadline = 0;

	for (;;) {
		GstSample *sample = st->pending;
		st->pending = NULL;
		if (sample == NULL && out.buffers == 0) {
			sample = gst_app_sink_pull_sample(appsink);
			if (sample == NULL) {
				out.eos = TRUE;
				return out;
			}
		} else if (sample == NULL) {
			gint64 left = deadline - g_get_monotonic_time();
			if (left <= 0) {
				return out;
			}
			sample = gst_app_sink_try_pull_sample(appsink, (GstClockTime)MIN(left, linger_us) * GST_USECOND);
			if (sample == NULL) {
				return out;
			}
		}

		GstBuffer *buffer = gst_sample_get_buffer(sample);
		gsize size = buffer != NULL ? gst_buffer_get_size(buffer) : 0;
		if (size == 0) {
			gst_sample_unref(sample); // empty buffer: nothing to send
			continue;
		}
		if (size > cap - out.bytes) {
			st->pending = sample;
			if (out.buffers == 0) {
				out.need = size;
			}
			return out;
		}
		if (out.buffers == 0) {
			deadline = g_get_monotonic_time() + hold_us;
		}
		gst_buffer_extract(buffer, 0, dst + out.bytes, size);
		out.bytes += size;
		out.buffers++;
		if (GST_BUFFER_PTS_IS_VALID(buffer)) {
			out.pts = GST_BUFFER_PTS(buffer);
			out.has_pts = TRUE;
		}
		gst_sample_unref(sample);
		if (out.bytes == cap) {
			return out;
		}
	}
}

// Read the "framerate" fraction from the element's sink pad current caps —
// the appsink's sink pad is a static pad carrying the negotiated video caps
// after the pipeline starts (mirrors belacoder.c get_sink_framerate).
// Returns 1 and fills num/den on success, 0 otherwise.
int gstpull_framerate(void *el, int *num, int *den) {
	if (el == NULL || num == NULL || den == NULL) {
		return 0;
	}
	GstElement *element = (GstElement *)el;
	GstPad *pad = gst_element_get_static_pad(element, "sink");
	if (pad == NULL) {
		return 0;
	}
	GstCaps *caps = gst_pad_get_current_caps(pad); // ref'd by the call
	gst_object_unref(pad);
	if (caps == NULL) {
		return 0;
	}
	int ret = 0;
	if (gst_caps_is_fixed(caps)) {
		const GstStructure *str = gst_caps_get_structure(caps, 0);
		if (gst_structure_get_fraction(str, "framerate", num, den)) {
			ret = 1;
		}
	}
	gst_caps_unref(caps);
	return ret;
}
*/
import "C"

import (
	"time"
	"unsafe"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
)

// drainer drains one appsink in batches. It owns the C-side state (a buffer
// parked between batches) and belongs to the goroutine that created it.
type drainer struct {
	sink  unsafe.Pointer
	state *C.GstpullState
}

func newDrainer(sink unsafe.Pointer) *drainer {
	return &drainer{sink: sink, state: C.gstpull_state_new()}
}

// close releases the C-side state (and a parked buffer, if any).
func (d *drainer) close() {
	C.gstpull_state_free(d.state)
	d.state = nil
}

// batch is the outcome of one drain call.
type batch struct {
	bytes   int    // payload bytes now in dst[:bytes]
	buffers int    // buffers those bytes came from
	pts     uint64 // PTS (ns) of the last buffer that carried one
	hasPTS  bool
	need    int  // > 0: nothing was copied; the next buffer needs len(dst) >= need
	eos     bool // the appsink is at EOS or stopped: the drain is over
}

// drain blocks for the next buffer and fills dst with it and whatever
// follows within linger (bounded by hold and len(dst)), in one cgo call.
// dst must not be empty.
func (d *drainer) drain(dst []byte, linger, hold time.Duration) batch {
	r := C.gstpull_drain(d.sink, d.state, (*C.uchar)(unsafe.Pointer(&dst[0])), C.gsize(len(dst)),
		C.gint64(linger.Microseconds()), C.gint64(hold.Microseconds()))
	return batch{
		bytes:   int(r.bytes),
		buffers: int(r.buffers),
		pts:     uint64(r.pts),
		hasPTS:  r.has_pts != 0,
		need:    int(r.need),
		eos:     r.eos != 0,
	}
}

// framerate reads the "framerate" fraction from the sink pad current caps.
// Returns (num, den); (0, 0) when unavailable.
func framerate(el gst.Element) (int, int) {
	if el == nil {
		return 0, 0
	}
	var num, den C.int
	if C.gstpull_framerate(gstutil.Ptr(el), &num, &den) == 0 {
		return 0, 0
	}
	return int(num), int(den)
}
