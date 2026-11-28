package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// ackRecorder is a websocket peer that forwards every message the session
// writes (frame acks, commands) to a channel.
func ackRecorder(out chan<- []byte) func(context.Context, *websocket.Conn) {
	return func(ctx context.Context, c *websocket.Conn) {
		for {
			_, msg, err := c.Read(ctx)
			if err != nil {
				return
			}
			out <- msg
		}
	}
}

func recvMsg(t *testing.T, ch <-chan []byte) map[string]any {
	t.Helper()
	select {
	case raw := <-ch:
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("session wrote invalid JSON %q: %v", raw, err)
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("session wrote nothing")
		return nil
	}
}

// frameMessage builds a Page.screencastFrame message; chromeOrder selects
// the member order Chrome emits (the shortcut's layout) or another valid one.
func frameMessage(frame []byte, sessionID int, chromeOrder bool) []byte {
	b64 := base64.StdEncoding.EncodeToString(frame)
	meta := `"metadata":{"offsetTop":0,"pageScaleFactor":1,"deviceWidth":1280,"deviceHeight":720,"timestamp":1.5}`
	if chromeOrder {
		return []byte(`{"method":"Page.screencastFrame","params":{"data":"` + b64 + `",` + meta +
			`,"sessionId":` + itoa(sessionID) + `}}`)
	}
	return []byte(`{"params":{"sessionId":` + itoa(sessionID) + `,` + meta + `,"data":"` + b64 +
		`"},"method":"Page.screencastFrame"}`)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// Both decode paths — the shortcut for Chrome's layout and the generic JSON
// decoder for any other — must hand the same frame to the handler and ack
// the same session id.
func TestDispatchScreencastFrameBothPaths(t *testing.T) {
	frame := bytes.Repeat([]byte{0x89, 'P', 'N', 'G', 0, 1, 2, 3, 250, 251}, 1000)
	for _, tc := range []struct {
		name        string
		chromeOrder bool
	}{{"chrome layout (shortcut)", true}, {"other member order (generic)", false}} {
		t.Run(tc.name, func(t *testing.T) {
			msg := frameMessage(frame, 42, tc.chromeOrder)
			if _, _, ok := screencastFrame(msg); ok != tc.chromeOrder {
				t.Fatalf("shortcut taken = %v, want %v", ok, tc.chromeOrder)
			}
			acks := make(chan []byte, 4)
			sess := benchSession(t, ackRecorder(acks))
			var got []byte
			sess.SetStreamHandler(func(f []byte) { got = f })
			sess.dispatch(msg)
			if !bytes.Equal(got, frame) {
				t.Fatalf("handler got %d bytes, want the %d-byte frame", len(got), len(frame))
			}
			ack := recvMsg(t, acks)
			if ack["method"] != "Page.screencastFrameAck" {
				t.Fatalf("ack method = %v", ack["method"])
			}
			if sid := ack["params"].(map[string]any)["sessionId"]; sid != float64(42) {
				t.Fatalf("ack sessionId = %v, want 42", sid)
			}
			if id, _ := ack["id"].(float64); id < 1 {
				t.Fatalf("ack id = %v, want a fresh command id", ack["id"])
			}
		})
	}
}

// The shortcut must step aside for anything but the exact layout it knows;
// the generic decoder then decides.
func TestScreencastShortcutDeclines(t *testing.T) {
	frame := []byte("frame-bytes-frame-bytes")
	good := string(frameMessage(frame, 7, true))
	if f, sid, ok := screencastFrame([]byte(good)); !ok || !bytes.Equal(f, frame) || sid != 7 {
		t.Fatalf("shortcut on Chrome's layout = (%q, %d, %v)", f, sid, ok)
	}
	b64 := base64.StdEncoding.EncodeToString(frame)
	for name, msg := range map[string]string{
		"escape in the string":  strings.Replace(good, b64, strings.Replace(b64, "Z", `\u005a`, 1), 1),
		"invalid base64":        strings.Replace(good, b64, "!!!!"+b64[4:], 1),
		"unterminated string":   good[:len(screencastPrefix)+8],
		"malformed tail":        strings.Replace(good, `"sessionId":7`, `"sessionId":`, 1),
		"trailing garbage":      good + "x",
		"another event":         `{"method":"Page.loadEventFired","params":{"timestamp":1}}`,
		"a response":            `{"id":3,"result":{"data":"` + b64 + `"}}`,
		"whitespace in the key": strings.Replace(good, `"params":{"data"`, `"params": {"data"`, 1),
	} {
		if _, _, ok := screencastFrame([]byte(msg)); ok {
			t.Errorf("%s: shortcut accepted the message", name)
		}
	}
	// An escaped (but valid) frame string still reaches the handler through
	// the generic decoder.
	if !strings.Contains(b64, "Z") {
		t.Fatal("test frame has no 'Z' to escape")
	}
	sess := benchSession(t, discardPeer)
	var got []byte
	sess.SetStreamHandler(func(f []byte) { got = f })
	sess.dispatch([]byte(strings.Replace(good, b64, strings.Replace(b64, "Z", `\u005a`, 1), 1)))
	if !bytes.Equal(got, frame) {
		t.Fatalf("generic path delivered %q, want %q", got, frame)
	}
}

// call results are typed: the screenshot frame and the evaluate value arrive
// decoded, an error response becomes an error.
func TestCallDecodesTypedResults(t *testing.T) {
	frame := []byte{1, 2, 3, 4, 5}
	replies := map[string]string{
		"Page.captureScreenshot": `"result":{"data":"` + base64.StdEncoding.EncodeToString(frame) + `"}`,
		"Runtime.evaluate":       `"result":{"result":{"type":"string","value":"ANGLE (NVIDIA)"}}`,
		"Page.enable":            `"result":{}`,
		"Page.bogus":             `"error":{"code":-32601,"message":"'Page.bogus' wasn't found"}`,
	}
	sess := benchSession(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			_, raw, err := c.Read(ctx)
			if err != nil {
				return
			}
			var cmd struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(raw, &cmd) != nil {
				return
			}
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"id":`+itoa(cmd.ID)+`,`+replies[cmd.Method]+`}`))
		}
	})
	go sess.readLoop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if got, err := sess.capture(ctx); err != nil || !bytes.Equal(got, frame) {
		t.Errorf("capture = (%v, %v), want the frame", got, err)
	}
	if got := sess.glRenderer(ctx); got != "ANGLE (NVIDIA)" {
		t.Errorf("glRenderer = %q", got)
	}
	if _, err := sess.call(ctx, "Page.enable", nil); err != nil {
		t.Errorf("Page.enable: %v", err)
	}
	if _, err := sess.call(ctx, "Page.bogus", nil); err == nil || !strings.Contains(err.Error(), "wasn't found") {
		t.Errorf("Page.bogus error = %v, want the CDP error message", err)
	}
}

func TestDispatchEvents(t *testing.T) {
	sess := benchSession(t, discardPeer)
	// Events the session does not read — including one whose params.data is
	// not a string — are ignored.
	for _, msg := range []string{
		`{"method":"Page.frameNavigated","params":{"frame":{"id":"A"}}}`,
		`{"method":"Some.event","params":{"data":{"nested":true},"sessionId":"x"}}`,
		`{"id":999,"result":{}}`, // response nobody waits for (a frame ack's)
		`not json`,
	} {
		sess.dispatch([]byte(msg))
	}
	select {
	case <-sess.loadCh:
		t.Fatal("loadCh closed before Page.loadEventFired")
	default:
	}
	sess.dispatch([]byte(`{"method":"Page.loadEventFired","params":{"timestamp":12.5}}`))
	select {
	case <-sess.loadCh:
	default:
		t.Fatal("Page.loadEventFired did not close loadCh")
	}
}
