// The Chrome CDP session for a browser layer: a persistent headless Chrome
// (one per layer) driven over the DevTools websocket, providing viewport
// control and frame capture. The primary path is Page.startScreencast (PNG,
// alpha-preserving — a transparent page background gives composited alerts
// their alpha channel): it streams fresh frames at vsync cadence, decoupled
// from the capture round trip. Page.captureScreenshot (50ms floor at 1080p:
// SwiftShader raster + PNG encode) is the fallback for Chrome builds where
// the screencast stream delivers no frames.
//
// A page reached over a raw CDP connection reports a 0x0 layout viewport
// (innerWidth/innerHeight), and captureScreenshot hangs on that with "Cannot
// take screenshot with 0 width" — so newCDPSession always issues
// Emulation.setDeviceMetricsOverride before the first capture.
//
// Chrome's media stack does not honor the host's proxy environment, and the
// page's traffic is not routed through them, so proxy env vars are stripped.
package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const cdpWebSocketReadLimit int64 = 16 << 20

type cdpsession struct {
	ws      *websocket.Conn
	cmd     *exec.Cmd
	userDir string
	port    int
	done    chan struct{} // closed when Chrome exits

	mu     sync.Mutex
	nextID int
	calls  map[int]chan *cdpMessage

	ackBuf []byte // frame-ack scratch; read-loop goroutine only

	// screencast frame sink, installed by the feeder before startScreencast.
	// Called from readLoop with the decoded PNG frame; the ack is sent by
	// the session, so the handler must not write to the socket.
	strMu    sync.Mutex
	streamFn func(frame []byte)

	// loadCh closes on the main frame's Page.loadEventFired. A
	// Page.startScreencast issued before the page has loaded streams no
	// frames at all (verified: 0 fps until the page paints; 60 fps after),
	// so New waits for it before returning.
	loadOnce sync.Once
	loadCh   chan struct{}
}

// cdpMessage is one DevTools message: a command response (ID set) or a page
// event (Method set). It is decoded in ONE pass into typed fields: the frame
// payloads are megabyte-scale base64 strings arriving up to 60 times a
// second, and a []byte field lets encoding/json base64-decode them straight
// out of the message — no map[string]any, no intermediate string, no second
// parse to tell an event from a response.
type cdpMessage struct {
	ID     int        `json:"id"`
	Method string     `json:"method"`
	Params cdpParams  `json:"params"`
	Result cdpResult  `json:"result"`
	Error  *cdpCError `json:"error"`
}

// cdpParams holds the event parameters the session reads.
type cdpParams struct {
	Data      []byte `json:"data"`      // Page.screencastFrame: the PNG
	SessionID int64  `json:"sessionId"` // Page.screencastFrame: echoed in the ack
	FrameID   string `json:"frameId"`
}

// cdpResult holds the command results the session reads.
type cdpResult struct {
	Data   []byte `json:"data"` // Page.captureScreenshot: the PNG
	Result struct {
		Value any `json:"value"` // Runtime.evaluate (returnByValue)
	} `json:"result"`
}

type cdpCError struct {
	Message string `json:"message"`
}

// findChrome locates the browser binary: $CTRLIRL_CHROME_BIN, then the usual
// names on PATH.
func findChrome() (string, error) {
	if p := strings.TrimSpace(os.Getenv("CTRLIRL_CHROME_BIN")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("layer: CTRLIRL_CHROME_BIN=%q not found", p)
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("layer: no Chrome/Chromium found (set CTRLIRL_CHROME_BIN)")
}

// renderBackendFlags picks Chrome's rendering backend.
//
// Hardware rendering keeps the page's requestAnimationFrame locked to the
// display refresh (via GPU compositing), which is what makes motion in the
// captured stream look smooth: the line-bounce probe shows a 0/16px
// stall-then-teleport beat under SwiftShader (a 30fps raster stretched across
// 60 delivered frames) that disappears on a real GPU. Software raster
// (SwiftShader) is the portable fallback for machines with no usable GPU.
//
// Selection order: CTRLIRL_CHROME_GPU=1 forces GPU; =0 forces SwiftShader;
// otherwise auto-detect (GL context present → GPU, else SwiftShader).
func renderBackendFlags() []string {
	// Operator override: CTRLIRL_CHROME_GPU_ARGS="a;b;c" replaces the whole
	// backend flag set (used to tune the render backend per host).
	if extra := os.Getenv("CTRLIRL_CHROME_GPU_ARGS"); extra != "" {
		return strings.FieldsFunc(extra, func(r rune) bool { return r == ';' })
	}
	onceRenderFlags.Do(func() {
		switch os.Getenv("CTRLIRL_CHROME_GPU") {
		case "1":
			renderFlags = gpuFlags
		case "0":
			renderFlags = swiftshaderFlags
		default:
			if hasGPU() {
				renderFlags = gpuFlags
			} else {
				renderFlags = swiftshaderFlags
			}
		}
	})
	return renderFlags
}

var (
	gpuFlags         = []string{"--enable-gpu-rasterization", "--enable-zero-copy"}
	swiftshaderFlags = []string{"--disable-gpu", "--enable-unsafe-swiftshader"}
	renderFlags      []string
	onceRenderFlags  sync.Once
)

// hasGPU reports whether a usable OpenGL renderer is present (i.e. the box has
// a working GPU/driver, not just software Mesa). A GLX context on the default
// display is enough for headless Chrome's GPU compositing.
func hasGPU() bool {
	// Cheap: ask glxinfo for the renderer and ignore anything that is a
	// known software fallback (llvmpipe/swrast) — those count as no-GPU.
	out, err := exec.Command("glxinfo", "-B").Output()
	if err != nil {
		return false
	}
	var renderer string
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "OpenGL renderer"); i >= 0 {
			renderer = strings.TrimSpace(line[i+len("OpenGL renderer"):])
		}
	}
	switch {
	case renderer == "":
		return false
	case strings.Contains(renderer, "llvmpipe"), strings.Contains(renderer, "swrast"), strings.Contains(renderer, "Mesa"):
		return false
	default:
		return true
	}
}

// newCDPSession launches a persistent headless Chrome in an isolated profile,
// connects its page websocket, navigates the page to the target, and pins the
// layout viewport. The target URL may be http(s) or file://; an empty target
// loads about:blank. Chrome is started with no URL argument and driven over
// CDP, running under a throwaway HOME so it never collides with the user's own
// Chrome session.
func newCDPSession(ctx context.Context, url, audioSink string, w, h int) (*cdpsession, error) {
	bin, err := findChrome()
	if err != nil {
		return nil, err
	}
	port, err := freeDebugPort()
	if err != nil {
		return nil, err
	}
	// Isolated HOME for this Chrome instance. Chrome's ProcessSingleton keys
	// off the real profile dir; without this, launching collides with any
	// other Chrome the user has open (SingletonLock: File exists) and the
	// headless process aborts. Pointing HOME/XDG at a throwaway dir keeps the
	// overlay's browser fully independent of the user's session.
	userDir, err := os.MkdirTemp("", "ctrlirl-chrome-*")
	if err != nil {
		return nil, err
	}
	profileDir := filepath.Join(userDir, "ud")
	for _, d := range []string{profileDir, filepath.Join(userDir, ".config"), filepath.Join(userDir, ".cache")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			os.RemoveAll(userDir)
			return nil, err
		}
	}
	s := &cdpsession{
		userDir: userDir,
		port:    port,
		done:    make(chan struct{}),
		calls:   make(map[int]chan *cdpMessage, 8),
		loadCh:  make(chan struct{}),
	}

	// The target URL is loaded via CDP after we connect; Chrome is started with
	// no URL argument (headless=new rejects a URL + multiple targets).
	target := url
	if target == "" {
		target = "about:blank"
	}

	// Note: --user-data-dir MUST use the equals form. The space form is
	// silently treated as a default dir and DevTools refuses to start.
	// Rendering backend (see renderBackendFlags). Hardware rendering keeps
	// the page's requestAnimationFrame locked to the display refresh, which
	// is what makes motion in the captured stream look smooth; SwiftShader
	// is the fallback for machines without a usable GPU. The screencast
	// capture path is unchanged (CDP Page.startScreencast reads compositor
	// output regardless of the backend).
	args := []string{
		"--headless=new",
		"--no-sandbox",
	}
	args = append(args, renderBackendFlags()...)
	args = append(args,
		"--hide-scrollbars",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-timer-throttling",
		"--disable-renderer-backgrounding",
		"--disable-backgrounding-occlusion",
		"--autoplay-policy=no-user-gesture-required",
		// Equals form is mandatory: the space form ("--window-size 1280,720")
		// leaves "1280,720" as a positional URL argument, which headless=new
		// rejects with "Multiple targets are not supported".
		"--window-size="+fmt.Sprintf("%d,%d", w, h),
		"--remote-debugging-port="+strconv.Itoa(port),
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir="+profileDir,
	)
	if audioSink != "" {
		args = append(args, "--audio-sink", audioSink)
	}

	s.cmd = exec.Command(bin, args...)
	baseEnv := filterProxyEnv(os.Environ())
	s.cmd.Env = append(baseEnv,
		"HOME="+userDir,
		"XDG_CONFIG_HOME="+filepath.Join(userDir, ".config"),
		"XDG_CACHE_HOME="+filepath.Join(userDir, ".cache"),
	)
	// PULSE_SINK pins every stream this Chrome instance creates onto the
	// layer's null sink at the process level (PulseAudio and PipeWire-Pulse
	// both honor it), so audio arriving minutes later (a donation alert) is
	// routed correctly without any pactl round trip. routeAudio remains as a
	// fallback for the case where the env pin does not take.
	if audioSink != "" {
		s.cmd.Env = append(s.cmd.Env, "PULSE_SINK="+audioSink)
	}
	if err := s.cmd.Start(); err != nil {
		os.RemoveAll(userDir)
		return nil, fmt.Errorf("layer: launch %s: %w", bin, err)
	}
	go func() {
		_ = s.cmd.Wait()
		close(s.done)
	}()

	wsURL, err := s.waitPageWS(ctx)
	if err != nil {
		s.stop()
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		s.stop()
		return nil, fmt.Errorf("layer: chrome websocket: %w", err)
	}
	conn.SetReadLimit(cdpWebSocketReadLimit)
	s.ws = conn
	go s.readLoop()

	if _, err := s.call(ctx, "Page.enable", nil); err != nil {
		s.stop()
		return nil, fmt.Errorf("layer: CDP Page.enable: %w", err)
	}
	// Transparent page background: captureScreenshot then emits PNG with an
	// alpha channel instead of a white backdrop, so the layer composites
	// over the stream instead of covering it.
	if _, err := s.call(ctx, "Emulation.setDefaultBackgroundColorOverride", map[string]any{
		"color": map[string]any{"r": 0, "g": 0, "b": 0, "a": 0},
	}); err != nil {
		s.stop()
		return nil, fmt.Errorf("layer: CDP transparent background: %w", err)
	}
	// Chrome was launched with no URL; point the page target at the real one.
	if _, err := s.call(ctx, "Page.navigate", map[string]any{"url": target}); err != nil {
		s.stop()
		return nil, fmt.Errorf("layer: CDP navigate: %w", err)
	}
	if err := s.setViewport(w, h); err != nil {
		s.stop()
		return nil, fmt.Errorf("layer: CDP set viewport: %w", err)
	}
	// The page must have loaded before the first capture: a
	// Page.startScreencast issued before the page paints streams zero
	// frames, and an unpainted capture is blank. Bounded: a page that never
	// fires load (a dead remote host) still gets the layer — the fallback
	// polling captures whatever the page shows.
	select {
	case <-s.loadCh:
	case <-time.After(10 * time.Second):
	}
	return s, nil
}

// stop kills Chrome and removes the profile directory.
func (s *cdpsession) stop() {
	if s.ws != nil {
		_ = s.ws.Close(websocket.StatusNormalClosure, "")
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	<-s.done
	os.RemoveAll(s.userDir)
}

// Stopped reports Chrome exit.
func (s *cdpsession) Stopped() <-chan struct{} { return s.done }

// setViewport pins the layout size (the captureScreenshot precondition).
func (s *cdpsession) setViewport(w, h int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             w,
		"height":            h,
		"deviceScaleFactor": 1,
		"mobile":            false,
	})
	return err
}

// SetStreamHandler installs the sink for Page.screencastFrame events.
// The handler runs on the CDP read-loop goroutine and receives the decoded
// PNG frame, which it may keep; the session acks the frame itself after the
// handler returns, so the handler must not issue CDP calls.
func (s *cdpsession) SetStreamHandler(fn func(frame []byte)) {
	s.strMu.Lock()
	s.streamFn = fn
	s.strMu.Unlock()
}

// startScreencast requests the DevTools frame stream: PNG (alpha-preserving),
// full-size, every frame. Frames arrive as Page.screencastFrame events
// dispatched to the installed stream handler.
func (s *cdpsession) startScreencast(ctx context.Context) error {
	_, err := s.call(ctx, "Page.startScreencast", map[string]any{
		"format":        "png",
		"maxWidth":      1920,
		"maxHeight":     1080,
		"everyNthFrame": 1,
	})
	return err
}

// stopScreencast ends the frame stream. Errors are non-fatal: the stream
// dies with the Chrome process.
func (s *cdpsession) stopScreencast(ctx context.Context) {
	s.call(ctx, "Page.stopScreencast", nil)
}

// capture grabs one PNG frame from the page. The page background is
// transparent (setDefaultBackgroundColorOverride), so the PNG carries an
// alpha channel for compositing.
func (s *cdpsession) capture(ctx context.Context) ([]byte, error) {
	res, err := s.call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return nil, err
	}
	if len(res.Data) == 0 {
		return nil, fmt.Errorf("layer: captureScreenshot returned no data")
	}
	return res.Data, nil
}

// glRenderer reports the active WebGL rasterizer via a Runtime.evaluate probe
// that reads the (unmasked) WebGL renderer string. It distinguishes hardware
// rendering (contains "NVIDIA"/"AMD"/"Intel" + model) from the software
// fallback (contains "SwiftShader" or is a pure-Mesa "llvmpipe"), which is
// exactly what separates smooth display-locked JS animation from the 30 fps
// software-raster 0/16 stall-then-teleport beat. Runtime.evaluate is used
// instead of SystemInfo.queryWebGL because the latter is absent from headless
// Chrome. Best-effort: on any failure it returns "unknown: <reason>" so a
// broken probe can never block startup.
func (s *cdpsession) glRenderer(ctx context.Context) string {
	const js = `(function(){
		try {
			var c = document.createElement('canvas');
			var gl = c.getContext('webgl') || c.getContext('experimental-webgl');
			if (!gl) return 'no-webgl';
			var ext = gl.getExtension('WEBGL_debug_renderer_info');
			return gl.getParameter(ext ? ext.UNMASKED_RENDERER_WEBGL : gl.RENDERER);
		} catch (e) { return 'err:' + e; }
	})()`
	res, err := s.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    js,
		"returnByValue": true,
	})
	if err != nil {
		return "unknown: " + err.Error()
	}
	r, _ := res.Result.Value.(string)
	if r == "" {
		return "unknown"
	}
	return r
}

// call sends one CDP command and waits for its response.
func (s *cdpsession) call(ctx context.Context, method string, params map[string]any) (cdpResult, error) {
	id, ch := s.nextCall()
	defer s.dropCall(id)

	payload, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return cdpResult{}, err
	}
	if err := s.ws.Write(ctx, websocket.MessageText, payload); err != nil {
		return cdpResult{}, fmt.Errorf("layer: CDP %s: %w", method, err)
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return cdpResult{}, fmt.Errorf("layer: CDP %s: %s", method, r.Error.Message)
		}
		return r.Result, nil
	case <-ctx.Done():
		return cdpResult{}, ctx.Err()
	case <-s.done:
		return cdpResult{}, fmt.Errorf("layer: chrome exited")
	}
}

func (s *cdpsession) nextCall() (int, chan *cdpMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	ch := make(chan *cdpMessage, 1)
	s.calls[s.nextID] = ch
	return s.nextID, ch
}

func (s *cdpsession) dropCall(id int) {
	s.mu.Lock()
	delete(s.calls, id)
	s.mu.Unlock()
}

// readLoop reads DevTools messages until the socket closes, then fails every
// pending call. One buffer is reused for all messages: dispatch consumes a
// message completely (decoding copies what it keeps) before the next read.
func (s *cdpsession) readLoop() {
	var msg bytes.Buffer
	for {
		_, r, err := s.ws.Reader(context.Background())
		if err == nil {
			msg.Reset()
			_, err = msg.ReadFrom(r)
		}
		if err != nil {
			s.failPendingCalls(err)
			return
		}
		s.dispatch(msg.Bytes())
	}
}

// failPendingCalls answers every waiting call with err (the socket is gone).
func (s *cdpsession) failPendingCalls(err error) {
	s.mu.Lock()
	pending := s.calls
	s.calls = make(map[int]chan *cdpMessage, 8)
	s.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- &cdpMessage{Error: &cdpCError{Message: err.Error()}}:
		default:
		}
	}
}

// dispatch routes one DevTools message: a response goes to its waiter, a
// page event to handleEvent. Runs on the read-loop goroutine and must not
// keep a reference to data.
func (s *cdpsession) dispatch(data []byte) {
	// Screencast frames are nearly all of the traffic and nearly all of
	// its bytes; they take a shortcut around the JSON decoder.
	if frame, sessionID, ok := screencastFrame(data); ok {
		s.handleFrame(frame, sessionID)
		return
	}
	var m cdpMessage
	if err := json.Unmarshal(data, &m); err != nil {
		if m.ID == 0 {
			return // an event in a shape the session does not read
		}
		m.Error = &cdpCError{Message: "malformed response: " + err.Error()}
	}
	if m.ID == 0 {
		s.handleEvent(&m)
		return
	}
	s.mu.Lock()
	if ch, ok := s.calls[m.ID]; ok {
		ch <- &m
	}
	s.mu.Unlock()
}

// screencastPrefix is how Chrome starts every Page.screencastFrame message:
// method first, then params with the frame as its first member.
const screencastPrefix = `{"method":"Page.screencastFrame","params":{"data":"`

// screencastFrame decodes a Page.screencastFrame message without running the
// JSON decoder over the frame. encoding/json validates and scans every byte
// of a message before decoding it, which costs several times the base64
// decode itself on a megabyte string.
//
// It recognises exactly the layout Chrome emits (screencastPrefix), takes the
// base64 run up to its closing quote, and JSON-decodes only the small
// remainder of params. Anything else — another member order, an escape
// inside the string, invalid base64, a malformed tail — reports !ok and the
// caller falls back to the generic decoder, so this is an optimisation only:
// it never decides what a message means.
func screencastFrame(msg []byte) (frame []byte, sessionID int64, ok bool) {
	if !bytes.HasPrefix(msg, []byte(screencastPrefix)) {
		return nil, 0, false
	}
	b64 := msg[len(screencastPrefix):]
	end := bytes.IndexByte(b64, '"')
	if end < 0 {
		return nil, 0, false
	}
	// tail is what follows the frame: `,"metadata":{…},"sessionId":7}}`.
	// Without the message's final brace and behind a stand-in for the
	// frame, it is the params object on its own.
	b64, tail := b64[:end], b64[end+1:]
	if len(tail) < 2 || tail[len(tail)-1] != '}' {
		return nil, 0, false
	}
	var params struct {
		SessionID int64 `json:"sessionId"`
	}
	rest := make([]byte, 0, len(tail)+len(`{"data":""`))
	rest = append(append(rest, `{"data":""`...), tail[:len(tail)-1]...)
	if json.Unmarshal(rest, &params) != nil {
		return nil, 0, false
	}
	frame = make([]byte, base64.StdEncoding.DecodedLen(len(b64)))
	n, err := base64.StdEncoding.Decode(frame, b64)
	if err != nil {
		return nil, 0, false
	}
	return frame[:n], params.SessionID, true
}

// handleEvent handles a page event: a screencast frame (generic decode
// path), or the main frame's Page.loadEventFired, which closes loadCh.
// Everything else is ignored.
func (s *cdpsession) handleEvent(m *cdpMessage) {
	switch m.Method {
	case "Page.screencastFrame":
		s.handleFrame(m.Params.Data, m.Params.SessionID)
	case "Page.loadEventFired":
		if m.Params.FrameID == "" { // main frame only
			s.loadOnce.Do(func() { close(s.loadCh) })
		}
	}
}

// handleFrame passes a screencast frame to the installed handler and acks
// it so Chrome keeps streaming (it stalls delivery once unacked frames pile
// up).
func (s *cdpsession) handleFrame(frame []byte, sessionID int64) {
	s.strMu.Lock()
	fn := s.streamFn
	s.strMu.Unlock()
	if fn != nil {
		if len(frame) == 0 {
			slog.Debug("layer: empty screencast frame")
		} else {
			fn(frame)
		}
	}
	s.ackScreencast(sessionID)
}

// ackScreencast writes a screencast frame ack without waiting for the
// response (an id with no pending call, dropped by dispatch). It must not
// block on a reply: it runs on the read-loop goroutine, the only one that
// could read it — a blocking s.call here deadlocks.
func (s *cdpsession) ackScreencast(sessionID int64) {
	s.ackBuf = fmt.Appendf(s.ackBuf[:0],
		`{"id":%d,"method":"Page.screencastFrameAck","params":{"sessionId":%d}}`,
		s.nextIDOnly(), sessionID)
	_ = s.ws.Write(context.Background(), websocket.MessageText, s.ackBuf)
}

// nextIDOnly reserves a CDP command id without registering a waiter.
func (s *cdpsession) nextIDOnly() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return s.nextID
}

// freeDebugPort picks a free localhost TCP port for the DevTools endpoint.
func freeDebugPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	_, portStr, _ := net.SplitHostPort(l.Addr().String())
	return strconv.Atoi(portStr)
}

// waitPageWS polls the DevTools /json endpoint until a page target appears.
func (s *cdpsession) waitPageWS(ctx context.Context) (string, error) {
	deadline := time.Now().Add(25 * time.Second)
	cli := &http.Client{Timeout: 2 * time.Second}
	for {
		if ws := s.pollPageWS(cli); ws != "" {
			return ws, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("layer: chrome devtools /json never reported a page target")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-s.done:
			return "", fmt.Errorf("layer: chrome exited during startup")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (s *cdpsession) pollPageWS(cli *http.Client) string {
	resp, err := cli.Get(fmt.Sprintf("http://127.0.0.1:%d/json", s.port))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var targets []map[string]any
	if json.Unmarshal(data, &targets) != nil {
		return ""
	}
	for _, t := range targets {
		if t["type"] == "page" {
			if ws, ok := t["webSocketDebuggerUrl"].(string); ok {
				return ws
			}
		}
	}
	return ""
}

// filterProxyEnv drops HTTP(S)/FTP/SOCKS proxy variables so the page's
// traffic goes direct.
func filterProxyEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if strings.EqualFold(key, "http_proxy") || strings.EqualFold(key, "https_proxy") ||
			strings.EqualFold(key, "ftp_proxy") || strings.EqualFold(key, "socks5_proxy") ||
			strings.EqualFold(key, "all_proxy") || strings.EqualFold(key, "no_proxy") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
