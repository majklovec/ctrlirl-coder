// Cgo binding for pushing bytes into an appsrc. The go-gst v1.4.1 binding
// exposes neither an appsrc type nor buffer creation, so the call is wrapped
// by hand — once, here, for every Go-fed layer (browser, testcode).
package gstutil

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0
#include <gst/gst.h>
#include <gst/app/gstappsrc.h>

// Allocate a buffer holding a copy of len bytes of data and push it to the
// appsrc, in one C call. data may point at Go memory: it is read only within
// this single synchronous call (cgo pointer rules).
//
// gst_app_src_push_buffer is transfer-full on EVERY flow result (it unrefs
// the buffer itself on FLUSHING/EOS), so the buffer is never touched after
// the push — unref'ing it here double-freed on every flushing push (scene
// switch) and corrupted the appsrc queue.
static GstFlowReturn gstutil_push_bytes(void *src, const unsigned char *data, gsize len) {
	if (src == NULL || data == NULL || len == 0) {
		return GST_FLOW_ERROR;
	}
	GstBuffer *b = gst_buffer_new_allocate(NULL, len, NULL);
	if (b == NULL) {
		return GST_FLOW_ERROR;
	}
	if (gst_buffer_fill(b, 0, data, len) != len) {
		gst_buffer_unref(b);
		return GST_FLOW_ERROR;
	}
	return gst_app_src_push_buffer(GST_APP_SRC(src), b);
}
*/
import "C"

import (
	"unsafe"

	"github.com/go-gst/go-gst/pkg/gst"
)

// PushBytes copies data into a new GStreamer buffer and pushes it to the
// appsrc, in one cgo call. data stays in Go memory and may be reused as soon
// as the call returns.
//
// It is safe to call from any goroutine (gst_app_src_push_buffer locks the
// appsrc and hands the buffer to the streaming thread). The result is the
// appsrc's flow return: FlowOK when queued, FlowFlushing while the pipeline
// is not running (stopped, or frozen at READY for a scene switch — not an
// error), FlowError for a nil appsrc or empty data.
func PushBytes(appsrc gst.Element, data []byte) gst.FlowReturn {
	if appsrc == nil || len(data) == 0 {
		return gst.FlowError
	}
	return gst.FlowReturn(C.gstutil_push_bytes(Ptr(appsrc), (*C.uchar)(unsafe.Pointer(&data[0])), C.gsize(len(data))))
}
