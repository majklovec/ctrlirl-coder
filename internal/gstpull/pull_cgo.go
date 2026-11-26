// Cgo bindings to the GStreamer *app* library (libgstapp): the exact calls the
// original ctrlirl_coder/belacoder uses to drain a terminal appsink —
// gst_app_sink_pull_sample + gst_sample_get_buffer + gst_buffer_map, plus the
// caps helpers (gst_pad_get_current_caps / gst_structure_get_fraction) for the
// sink framerate and the buffer PTS.
//
// Every GStreamer entry point is wrapped in a small C function that takes and
// returns opaque `void*`. cgo can only pass `unsafe.Pointer` to/ from C
// pointer types, so routing through `void*` keeps the Go side free of the
// generated `*_Ctype_struct_...` types. A C static helper also stands in for
// the GST_BUFFER_PTS macro (cgo cannot reference macros).
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

// One pulled+mapped sample: the map info must be kept until unmap.
typedef struct {
	GstSample *sample;
	GstBuffer *buffer;
	GstMapInfo map;
} GstpullMapped;

// Pull one sample (blocking until the streaming thread emits one) and map it
// read-only in the same call. Fills *data/*size/*pts; returns an opaque
// handle, 0 when the element left PLAYING (frozen/EOS — the pull returned
// NULL), or -1 on a map failure (the sample is dropped there). The payload
// is valid only until gstpull_release_mapped — the send path copies it
// synchronously (srt_sendmsg2), it is never retained.
gintptr gstpull_pull_mapped(void *sink, const unsigned char **data, int *size, int64_t *pts) {
	if (sink == NULL) {
		return 0;
	}
	GstSample *sample = gst_app_sink_pull_sample((GstAppSink *)sink);
	if (sample == NULL) {
		return 0;
	}
	GstBuffer *buffer = gst_sample_get_buffer(sample);
	GstpullMapped *m = g_new0(GstpullMapped, 1);
	m->sample = sample;
	m->buffer = buffer;
	if (buffer == NULL || gst_buffer_map(buffer, &m->map, GST_MAP_READ) == FALSE) {
		gst_sample_unref(sample);
		g_free(m);
		return -1;
	}
	*data = m->map.data;
	*size = (int)m->map.size;
	*pts = GST_BUFFER_PTS(buffer);
	return (gintptr)m;
}

// Unmap and unref a pulled sample.
void gstpull_release_mapped(gintptr handle) {
	if (handle == 0) {
		return;
	}
	GstpullMapped *m = (GstpullMapped *)handle;
	gst_buffer_unmap(m->buffer, &m->map);
	gst_sample_unref(m->sample);
	g_free(m);
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
	"unsafe"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
)

// frameRef is one pulled, mapped buffer: a C handle plus the payload view.
// data/size are valid only until release().
type frameRef struct {
	handle int64
	data   unsafe.Pointer
	size   int
	pts    uint64
}

// pullMappedFrame pulls one sample and maps it in one cgo call.
// handle 0 = no sample (pipeline left PLAYING), -1 = map failure.
func pullMappedFrame(sink unsafe.Pointer) frameRef {
	var data *C.uchar
	var size C.int
	var pts C.int64_t
	h := C.gstpull_pull_mapped(sink, &data, &size, &pts)
	if h == 0 {
		return frameRef{}
	}
	if h < 0 {
		return frameRef{handle: -1}
	}
	return frameRef{
		handle: int64(h),
		data:   unsafe.Pointer(data),
		size:   int(size),
		pts:    uint64(pts),
	}
}

// release unmaps and unrefs the buffer (one cgo call). Must run after any
// consumer is done with the payload (the SRT send copies it synchronously).
func (f *frameRef) release() {
	if f.handle != 0 {
		C.gstpull_release_mapped(C.gintptr(f.handle))
		f.handle = 0
	}
}

// bytes views the mapped payload as a Go slice without copying (zero-copy).
// The slice is only legal until release().
func (f *frameRef) bytes() []byte {
	if f.handle <= 0 || f.size <= 0 {
		return nil
	}
	return unsafe.Slice((*byte)(f.data), f.size)
}

// framerate reads the "framerate" fraction from the src pad current caps.
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
