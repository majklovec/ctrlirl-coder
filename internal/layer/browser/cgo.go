// Cgo bindings for the browser layer's frame push: allocation plus
// gst_app_src_push_buffer in a single C function. The go-gst v1.4.1 binding
// exposes neither an appsrc type nor buffer creation, so the call is wrapped
// by hand. The pattern mirrors internal/gstpull: opaque void* in and out.
//
// These run on the layer's own frame-feeder goroutine (the push model), never
// on GStreamer's streaming thread, so blocking here is safe.
// gst_app_src_push_buffer is thread-safe (it locks the appsrc and posts the
// buffer to the streaming thread). The appsrc element stays owned by go-gst;
// we only borrow its raw pointer per call.
package browser

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0
#include <stdlib.h>
#include <string.h>
#include <gst/gst.h>
#include <gst/app/gstappsrc.h>

// Allocate a buffer holding a copy of len bytes of data and push it to the
// appsrc, in one C call. data may point at Go memory: it is read only within
// this single synchronous C call (cgo pointer rules), so no C.CBytes hop is
// needed. gst_app_src_push_buffer is transfer-full on EVERY flow result (it
// unrefs the buffer itself on FLUSHING/EOS), so the buffer is never touched
// after the push — unref'ing it here double-freed on every flushing push
// (scene switch) and corrupted the appsrc queue. Returns 1 on OK, 0 on
// FLUSHING (pipeline stopping; not an error), -1 on any other flow error.
int browser_push_data(void *src, const unsigned char *data, int len) {
	if (src == NULL || data == NULL || len <= 0) {
		return -1;
	}
	GstBuffer *b = gst_buffer_new_allocate(NULL, (gsize)len, NULL);
	GstMemory *m = gst_buffer_peek_memory(b, 0);
	GstMapInfo info;
	gst_memory_map(m, &info, GST_MAP_WRITE);
	memcpy(info.data, data, (size_t)len);
	gst_memory_unmap(m, &info);
	GstFlowReturn r = gst_app_src_push_buffer((GstAppSrc *)src, b);
	if (r == GST_FLOW_OK) {
		return 1;
	}
	if (r == GST_FLOW_FLUSHING) {
		return 0;
	}
	return -1;
}

// The numeric/enum property setter lives in internal/gstutil
// (gstutil.SetNumericProperty) — one home for the binding workarounds.
*/
import "C"

import (
	"unsafe"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
)

// pushBuffer allocates a GStreamer buffer copy of data and pushes it to the
// appsrc in a single C call. data stays in Go memory; the pointer crosses
// the cgo boundary for the duration of one synchronous call only (the same
// pattern gstpull uses), so there is no C.CBytes allocation.
// Return: 1 = pushed, 0 = flushing (the feeder should stop), -1 = failure.
func pushBuffer(appSrc gst.Element, data []byte) int {
	if len(data) == 0 {
		return -1
	}
	return int(C.browser_push_data(gstutil.Ptr(appSrc), (*C.uchar)(unsafe.Pointer(&data[0])), C.int(len(data))))
}
