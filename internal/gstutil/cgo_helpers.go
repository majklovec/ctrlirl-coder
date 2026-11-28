// Cgo home for the GStreamer binding workarounds that more than one
// package needs: the g_object_setv property setter (the go-gst binding's
// varargs g_object_set silently drops int/enum values in this build) and
// the raw-pointer extraction used to hand elements to C code.
package gstutil

/*
#cgo pkg-config: gstreamer-1.0
#include <gst/gst.h>
#include <gst/gstcontext.h>

// Deep (recursive) search of a bin for the first element created from the
// named factory. Returns a new (floating) reference, NULL when absent.
static GObject *gstutil_find_deep_factory(void *bin, const char *factory) {
	GstIterator *it = gst_bin_iterate_all_by_element_factory_name(GST_BIN(bin), factory);
	GValue v = G_VALUE_INIT;
	GObject *found = NULL;
	while (gst_iterator_next(it, &v) == GST_ITERATOR_OK) {
		GObject *o = g_value_get_object(&v);
		g_value_reset(&v);
		if (found != NULL) {
			gst_object_unref(o);
		} else {
			found = o;
		}
	}
	gst_iterator_free(it);
	return found;
}

// Set a numeric (int/uint, any width) or enum property via the non-varargs
// g_object_setv. In this GStreamer build the varargs g_object_set silently
// drops int/enum GValues (the value is read back as 0), which makes every
// integer property set a no-op — including the adaptive bitrate controller's
// x264enc "bitrate". setv with a GValue typed to the property works.
// Returns 0 on success, -1 when the object has no such property,
// -2 when the property is not a numeric/enum type.
int gstutil_set_num(void *obj, const char *name, long long value) {
	GObject *o = (GObject *)obj;
	if (o == NULL || name == NULL) {
		return -1;
	}
	GParamSpec *ps = g_object_class_find_property(G_OBJECT_GET_CLASS(o), name);
	if (ps == NULL) {
		return -1;
	}
	GType t = ps->value_type;
	int numeric = (t == G_TYPE_INT || t == G_TYPE_UINT || t == G_TYPE_INT64 ||
	               t == G_TYPE_UINT64 || g_type_is_a(t, G_TYPE_ENUM));
	if (!numeric) {
		return -2;
	}
	GValue val = {0};
	g_value_init(&val, t);
	switch (t) {
	case G_TYPE_INT:
		g_value_set_int(&val, (gint)value);
		break;
	case G_TYPE_UINT:
		g_value_set_uint(&val, (guint)value);
		break;
	case G_TYPE_INT64:
		g_value_set_int64(&val, (gint64)value);
		break;
	case G_TYPE_UINT64:
		g_value_set_uint64(&val, (guint64)value);
		break;
	default:
		g_value_set_enum(&val, (int)value);
		break;
	}
	g_object_setv(o, 1, &name, &val);
	g_value_unset(&val);
	return 0;
}

// Render a caps-typed property as a string (g_object_get_property +
// gst_caps_to_string). The go-gst binding has no GValue marshaler for
// GstCaps, so ObjectProperty returns InvalidValue for such properties.
// Returns a C-allocated string, or NULL when the property is missing or
// not a caps type.
char *gstutil_caps_to_str(void *obj, const char *name) {
	GObject *o = (GObject *)obj;
	if (o == NULL || name == NULL) {
		return NULL;
	}
	GParamSpec *ps = g_object_class_find_property(G_OBJECT_GET_CLASS(o), name);
	if (ps == NULL || !g_type_is_a(ps->value_type, GST_TYPE_CAPS)) {
		return NULL;
	}
	GValue val = {0};
	g_value_init(&val, ps->value_type);
	g_object_get_property(o, name, &val);
	GstCaps *caps = g_value_get_boxed(&val);
	if (caps == NULL) {
		g_value_unset(&val);
		return NULL;
	}
	char *str = gst_caps_to_string(caps);
	g_value_unset(&val);
	return str;
}

// Mark a pipeline as live by attaching the live GST_CONTEXT ("live" token,
// non-writable). In a live pipeline, elements that resync running time (queue,
// audioconvert, ...) treat an upstream timestamp DISCONTINUITY — such as the
// per-segment ~0.4 s audio PTS jump hlsdemux2 emits on a TS CC-reset — as a
// running-time shift, so downstream sees continuous timestamps instead of a
// gap. This is what stops the audiomixer from filling that jump with silence
// (the recurring 10 s audio glitch). go-gst exposes no context builder, so the
// token/context assembly lives here. Returns 0 on success, -1 on a NULL
// pipeline.
int gstutil_make_live(void *pipeline) {
	GstElement *pipe = (GstElement *)pipeline;
	if (pipe == NULL) {
		return -1;
	}
	// A "live" context whose data structure carries the live token under the
	// token name "live". The token value is an array of token-sets, each set
	// an array of strings: [ "live" ]. GStreamer 1.28 exposes the array value
	// type via gst_value_array_init/append_value (the older
	// gst_context_new_empty/add_token and GstValueArray helpers are private).
	GstContext *ctx = gst_context_new("live", FALSE);
	GstStructure *s = gst_context_writable_structure(ctx);
	GValue inner = G_VALUE_INIT;
	gst_value_array_init(&inner, 1);
	GValue tok = G_VALUE_INIT;
	g_value_init(&tok, G_TYPE_STRING);
	g_value_set_string(&tok, "live");
	gst_value_array_append_value(&inner, &tok);
	g_value_unset(&tok);
	GValue outer = G_VALUE_INIT;
	gst_value_array_init(&outer, 1);
	gst_value_array_append_value(&outer, &inner);
	g_value_unset(&inner);
	gst_structure_set_value(s, "live", &outer);
	g_value_unset(&outer);
	// gst_element_set_context takes its own reference; on GStreamer 1.28
	// GstContext is a GstMiniObject, so g_object_unref/gst_object_unref on it
	// trips the G_IS_OBJECT assertion ("g_object_unref: assertion
	// 'G_IS_OBJECT (object)' failed"). The element owns the last reference
	// and frees the context with the pipeline — no explicit unref here.
	gst_element_set_context(pipe, ctx);
	return 0;
}

// Reports whether an element carries the live context token (debug/verify).
int gstutil_is_live(void *el) {
	GstElement *e = (GstElement *)el;
	if (e == NULL) {
		return -1;
	}
	GstContext *ctx = gst_element_get_context(e, "live");
	if (ctx == NULL) {
		return 0;
	}
	int r = gst_context_has_context_type(ctx, "live") ? 1 : 0;
	gst_object_unref(ctx);
	return r;
}

*/
import "C"

import (
	"fmt"
	"unsafe"

	"github.com/go-gst/go-glib/pkg/gobject/v2"
	"github.com/go-gst/go-gst/pkg/gst"
)

// SetNumericProperty sets an integer/enum property on an element through
// the non-varargs g_object_setv path. The go-gst/go-glib binding goes
// through the varargs g_object_set, which silently drops int/enum values in
// this GStreamer build, so callers that need a numeric property to actually
// take (encoder bitrate, queue sizes, ...) must use this.
// Returns an error for unknown or non-numeric properties.
func SetNumericProperty(el gst.Element, name string, value int64) error {
	if el == nil {
		return fmt.Errorf("nil element")
	}
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	switch rc := int(C.gstutil_set_num(Ptr(el), cname, C.longlong(value))); rc {
	case 0:
		return nil
	case -2:
		return fmt.Errorf("property %q is not numeric", name)
	default:
		return fmt.Errorf("element has no property %q", name)
	}
}

// Ptr extracts the raw C GObject pointer from a go-gst element without
// touching its refcount (a "None" ownership borrow).
func Ptr(el gst.Element) unsafe.Pointer {
	return gobject.UnsafeObjectToGlibNone(el)
}

// FindDeepByFactory returns the first descendant of bin (recursive) created
// from the named element factory, taking ownership of its reference; nil
// when absent. The go-gst binding's IterateAllByElementFactoryName is an
// unimplemented stub ("must be handwritten"), so the deep walk happens in C.
func FindDeepByFactory(bin gst.Element, factory string) gst.Element {
	cbin := Ptr(bin)
	if cbin == nil || factory == "" {
		return nil
	}
	cname := C.CString(factory)
	defer C.free(unsafe.Pointer(cname))
	p := C.gstutil_find_deep_factory(cbin, cname)
	if p == nil {
		return nil
	}
	return gst.UnsafeElementFromGlibNone(unsafe.Pointer(p))
}

// MakeLive attaches the live GST_CONTEXT to an element and everything
// downstream of it (contexts propagate down the element tree). Call it after
// the pipeline is built and before it is set to PLAYING. See
// gstutil_make_live for why this is required (keeps the per-segment
// hlsdemux2 audio PTS jump a resync rather than a silence-filling gap).
//
// The caller decides the SCOPE: the compositor pipeline attaches it to the
// shared audio tail head only (see CompositorPipeline.Start) — never the
// pipeline root or the terminal appsink. A root-level live context inherits into
// the upstream VOD source (uridecodebin → hlsdemux2, soup), which becomes
// unpauseable and deadlocks on appsink backpressure (drop=false) at the
// segment boundary / drains a finite playlist to EOS. The same-named-context
// "carve-out" does not help: context fields merge and the parent's "live"
// field wins over any child override (measured on GStreamer 1.28). Returns
// an error only for a nil element.
func MakeLive(el gst.Element) error {
	if el == nil {
		return fmt.Errorf("nil element")
	}
	if rc := int(C.gstutil_make_live(Ptr(el))); rc != 0 {
		return fmt.Errorf("element is not a GstElement")
	}
	return nil
}

// IsLive reports whether the element carries the live context token.
func IsLive(el gst.Element) bool {
	if el == nil {
		return false
	}
	return int(C.gstutil_is_live(Ptr(el))) == 1
}

// CapsString renders a caps-typed property of the element (e.g. a
// capsfilter's "caps") as its gst_caps_to_string form. Returns "" when
// the property is missing or not a caps type.
func CapsString(el gst.Element, name string) string {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	cstr := C.gstutil_caps_to_str(Ptr(el), cname)
	if cstr == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(cstr))
	return C.GoString(cstr)
}

// IsFactoryType reports whether el was created from the named element
// factory, comparing the element's GType against a freshly created probe
// (the go-gst binding exposes no GST_IS_APP_SINK-style type checks and no
// factory klasses metadata, so a GType compare is the exact check).
func IsFactoryType(el gst.Element, factoryName string) bool {
	f := gst.ElementFactoryFind(factoryName)
	if f == nil {
		return false
	}
	probe := f.Create("")
	defer gobject.UnsafeObjectUnref(probe)
	return el.GoValueType() == probe.GoValueType()
}
