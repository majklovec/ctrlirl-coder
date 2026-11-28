package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/go-gst/go-glib/pkg/gobject/v2"
	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
)

// benchFrame is a 300 KB stand-in for a 1280x720 browser screenshot frame.
var benchFrame = make([]byte, 300*1024)

// BenchmarkCDPFrameDecode benchmarks the frame payload path of a
// Page.captureScreenshot response: websocket-frame bytes → JSON envelope
// (map decode — a json.RawMessage result field benchmarks ~2x slower) →
// base64 decode of the "data" field.
func BenchmarkCDPFrameDecode(b *testing.B) {
	b.ReportAllocs()
	b64 := base64.StdEncoding.EncodeToString(benchFrame)
	env, err := json.Marshal(map[string]any{"id": 1, "result": map[string]any{"data": b64}})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		var r cdpResponse
		if err := json.Unmarshal(env, &r); err != nil {
			b.Fatal(err)
		}
		s, ok := r.Result["data"].(string)
		if !ok {
			b.Fatal("no data in result")
		}
		d, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			b.Fatal(err)
		}
		if len(d) != len(benchFrame) {
			b.Fatal("frame size mismatch")
		}
	}
}

// BenchmarkPushBuffer benchmarks the appsrc push path: GstBuffer allocate +
// memcpy + thread-safe push in one C call.
func BenchmarkPushBuffer(b *testing.B) {
	b.ReportAllocs()
	gstutil.EnsureInit()
	pip, err := gstutil.FromLaunch("appsrc name=src is-live=true max-buffers=10 ! fakesink")
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		pip.SetState(gst.StateNull)
		gobject.UnsafeObjectUnref(pip.As())
	}()
	el := pip.As().GetByName("src")
	if el == nil {
		b.Fatal("no appsrc element")
	}
	if err := pip.SetState(gst.StatePlaying); err != nil {
		b.Fatal(err)
	}
	// Pushing before the streaming thread has created the appsrc (the
	// element's own preroll) aborts inside GStreamer — so wait for the
	// appsrc element itself to reach PLAYING.
	deadline := time.Now().Add(10 * time.Second)
	for el.GetCurrentState() != gst.StatePlaying {
		if time.Now().After(deadline) {
			b.Fatal("appsrc never reached PLAYING")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < b.N; i++ {
		if ret := pushBuffer(el, benchFrame); ret != 1 {
			b.Fatalf("push ret = %d, want 1", ret)
		}
	}
}

// screencastEvent is a Page.screencastFrame message in the field order
// Chrome emits (method, params{data, metadata, sessionId}).
func screencastEvent(tb testing.TB) []byte {
	tb.Helper()
	return []byte(`{"method":"Page.screencastFrame","params":{"data":"` +
		base64.StdEncoding.EncodeToString(benchFrame) +
		`","metadata":{"offsetTop":0,"pageScaleFactor":1,"deviceWidth":1280,"deviceHeight":720,` +
		`"scrollOffsetX":0,"scrollOffsetY":0,"timestamp":1795845152.067},"sessionId":7}}`)
}

// benchSession is a cdpsession wired to an in-process websocket peer that
// runs serve — the transport of a real session without a Chrome process.
func benchSession(tb testing.TB, serve func(ctx context.Context, c *websocket.Conn)) *cdpsession {
	tb.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(cdpWebSocketReadLimit)
		serve(r.Context(), c)
	}))
	tb.Cleanup(srv.Close)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		tb.Fatal(err)
	}
	conn.SetReadLimit(cdpWebSocketReadLimit)
	tb.Cleanup(func() { conn.CloseNow() })
	return &cdpsession{
		ws:     conn,
		done:   make(chan struct{}),
		calls:  make(map[int]chan *cdpResponse, 8),
		loadCh: make(chan struct{}),
	}
}

// discardPeer reads and drops everything the session writes (frame acks).
func discardPeer(ctx context.Context, c *websocket.Conn) {
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
	}
}

// BenchmarkScreencastDispatch benchmarks the per-frame CPU path of the
// screencast stream: one Page.screencastFrame message through the read
// loop's dispatch (envelope decode, base64 decode, handler, ack write).
func BenchmarkScreencastDispatch(b *testing.B) {
	sess := benchSession(b, discardPeer)
	var got int
	sess.SetStreamHandler(func(frame []byte) { got = len(frame) })
	msg := screencastEvent(b)
	b.ReportAllocs()
	b.SetBytes(int64(len(benchFrame)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sess.dispatch(msg)
		if got != len(benchFrame) {
			b.Fatalf("handler got %d bytes, want %d", got, len(benchFrame))
		}
	}
}

// BenchmarkScreencastReadLoop benchmarks the whole receive path the way
// Chrome drives it: the peer sends a frame and waits for its ack before the
// next one; the session's read loop reads the websocket message, decodes it
// and acks. Includes the websocket read buffering BenchmarkScreencastDispatch
// leaves out.
func BenchmarkScreencastReadLoop(b *testing.B) {
	msg := screencastEvent(b)
	start := make(chan int)
	done := make(chan error, 1)
	sess := benchSession(b, func(ctx context.Context, c *websocket.Conn) {
		for n := range start {
			var err error
			for i := 0; i < n && err == nil; i++ {
				if err = c.Write(ctx, websocket.MessageText, msg); err == nil {
					_, _, err = c.Read(ctx) // the frame ack
				}
			}
			done <- err
		}
	})
	defer close(start)
	sess.SetStreamHandler(func(frame []byte) {
		if len(frame) != len(benchFrame) {
			b.Errorf("handler got %d bytes, want %d", len(frame), len(benchFrame))
		}
	})
	go sess.readLoop()
	b.ReportAllocs()
	b.SetBytes(int64(len(benchFrame)))
	b.ResetTimer()
	start <- b.N
	if err := <-done; err != nil {
		b.Fatal(err)
	}
}
