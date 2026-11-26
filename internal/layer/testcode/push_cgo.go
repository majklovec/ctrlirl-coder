// Cgo glue to feed rendered frames into the layer's appsrc (go-gst v1.4.1
// exposes no appsrc push wrapper). Allocation, copy and push happen in one
// C call so no go-gst Buffer wrapper (and its finalizer) ever owns the
// GstBuffer — mirrors internal/layer/browser/cgo.go.
package testcode

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0
#include <string.h>
#include <gst/gst.h>
#include <gst/app/gstappsrc.h>

// Copy len bytes of data into a fresh buffer and push it. data may point at
// Go memory: it is only read during this synchronous call.
// gst_app_src_push_buffer is transfer-full on EVERY flow result (it unrefs
// the buffer itself on FLUSHING/EOS), so the buffer is never touched after
// the push. Unref'ing it here would double-free and corrupt the appsrc
// queue (gstappsrc.c "code should not be reached").
// Returns the GstFlowReturn, or GST_FLOW_ERROR on allocation/map failure.
int testcode_push_data(void *src, const unsigned char *data, int len) {
	if (src == NULL || data == NULL || len <= 0) {
		return GST_FLOW_ERROR;
	}
	GstBuffer *b = gst_buffer_new_allocate(NULL, (gsize)len, NULL);
	if (b == NULL) {
		return GST_FLOW_ERROR;
	}
	if (gst_buffer_fill(b, 0, data, (gsize)len) != (gsize)len) {
		gst_buffer_unref(b);
		return GST_FLOW_ERROR;
	}
	return gst_app_src_push_buffer(GST_APP_SRC(src), b);
}
*/
import "C"

import (
	"errors"
	"unsafe"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
)

// pushAppSrcFeed pushes one BGRA frame (w*h*4 bytes) into the appsrc and
// returns the GStreamer flow result. Ownership of the buffer passes to the
// appsrc inside the C call.
func pushAppSrcFeed(appsrc gst.Element, data []byte) (gst.FlowReturn, error) {
	if appsrc == nil {
		return 0, errors.New("testcode: nil appsrc")
	}
	if len(data) == 0 {
		return 0, errors.New("testcode: empty frame")
	}
	r := C.testcode_push_data(gstutil.Ptr(appsrc), (*C.uchar)(unsafe.Pointer(&data[0])), C.int(len(data)))
	return gst.FlowReturn(int(r)), nil
}
