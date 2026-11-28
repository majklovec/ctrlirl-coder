package browser

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/gstutil"
	"ctrlirl-coder/internal/layer"
	"ctrlirl-coder/internal/scene"
)

// Browser overlay defaults.
const (
	browserDefaultFPS    = 30
	browserDefaultWidth  = 1280
	browserDefaultHeight = 720

	// browserFlushBackoff paces the feeder's re-push loop while the appsrc
	// is FLUSHING (pipeline frozen at READY for a scene switch / layer swap).
	browserFlushBackoff = 200 * time.Millisecond

	// browserStreamWatchdog: the startup window in which the CDP frame
	// stream must deliver its first frame. If it has not by then, this
	// Chrome build's Page.startScreencast is dead and the feeder falls back
	// to captureScreenshot polling for the layer's lifetime. Once the first
	// frame arrives the watchdog is disarmed: a silent stream from then on
	// is a static page (the stream only sends on repaint), not a failure —
	// starting polling alongside a working stream tears the feed apart
	// (measured: 60 → ~45 fps with frame skips when both ran).
	browserStreamWatchdog = 1500 * time.Millisecond
)

// Browser overlay is a live browser layer: a persistent headless Chrome
// (one per layer) whose page is captured over the Chrome DevTools Protocol
// and fed to the compositor as in-pipeline PNG frames (appsrc ! pngdec) so
// the layer keeps an alpha channel for compositing. Frames come from the
// CDP Page.startScreencast stream (PNG, vsync cadence) when that Chrome
// build delivers it; builds where the stream is silent fall back to
// Page.captureScreenshot polling (50ms floor at 1080p). A transparent page
// background keeps the alpha channel in both paths. The page's audio is
// routed to a per-layer Pulse/PipeWire null sink and captured on that
// sink's monitor, exposed as a separate audio chain (see AudioChain) that
// the compositor plugs into the mpegtsmux. Both streams come from the same
// Chrome process.
//
// Video chain (BuildChain): appsrc(image/png) → pngdec → videoconvert →
// videoscale → capsfilter(BGRA) → queue. The PNG bytes are pushed from a
// dedicated feeder goroutine owned by this layer.
//
// Chrome is launched asynchronously (BuildChain spawns it) so the pipeline
// command goroutine is not blocked on the ~1-2 s browser start.
type BrowserOverlay struct {
	layer.BaseLayer

	appSrc   gst.Element // the appsrc the feeder pushes PNG frames into
	sink     string      // Pulse null-sink name ("" = no user audio session)
	sinkUp   func()      // unloads the null sink (nil if none)
	hasAudio bool

	url    string
	width  int
	height int
	fps    int

	// Async lifecycle (set in BuildChain).
	ctx     context.Context // cancelled by Close
	cancel  context.CancelFunc
	done    chan struct{} // closed when the browser goroutine exits
	closed  sync.Once
	started bool
}

// NewBrowserOverlay builds a browser layer.
func NewBrowserOverlay(l scene.Layer) *BrowserOverlay {
	return &BrowserOverlay{BaseLayer: layer.NewBaseLayer(l, scene.LayerTypeBrowser)}
}

// BuildChain creates the appsrc video chain and launches the browser
// asynchronously. The Pulse null sink (when a user audio session is present)
// is created synchronously so AudioChain can expose a device immediately.
func (b *BrowserOverlay) BuildChain() (*layer.Chain, error) {
	b.parseConfig()

	head, err := layer.Build(
		layer.Spec{
			Factory: "appsrc", Name: b.L.ID + "-src",
			Props: map[string]any{
				"is-live":      true,
				"do-timestamp": true,
				// Single frame of latency: at most one buffer is queued and a
				// full queue drops the OLDEST (leaky-type=2 = downstream), so
				// a slow downstream tick can never build backlog — the
				// freshest frame always wins. block=false alone does not drop
				// (it just makes push fail on overflow), so the leaky type is
				// what bounds the queue.
				"block":       false,
				"max-buffers": 1,
				"leaky-type":  2, // GST_APP_LEAKY_TYPE_DOWNSTREAM
			},
			Caps: fmt.Sprintf("image/png,width=%d,height=%d,framerate=%d/1", b.width, b.height, b.fps),
		},
		// pngdec is a static-pad decoder: the chain is static.
		layer.Spec{Factory: "pngdec", Name: b.L.ID + "-pngdec"},
	)
	if err != nil {
		return nil, err
	}
	chain, err := layer.SurfaceChain(b.L, head...)
	if err != nil {
		return nil, err
	}
	b.appSrc = head[0]

	// Audio: a per-layer Pulse/PipeWire null sink gives the page's audio a
	// dedicated monitor to record from. Without a user audio session the
	// layer degrades to video-only.
	sinkName := "ctrlirl_" + sanitizeSinkID(b.L.ID)
	if up, err := pulseVirtualSink(sinkName); err != nil {
		slog.Warn("layer: browser overlay video-only (no audio session)", "id", b.L.ID, "err", err)
	} else {
		b.sink = sinkName
		b.sinkUp = up
		b.hasAudio = true
	}

	b.ctx, b.cancel = context.WithCancel(context.Background())
	b.done = make(chan struct{})
	b.started = true
	go b.runBrowser()

	slog.Info("layer: browser overlay",
		"id", b.L.ID, "url", b.url, "width", b.width, "height", b.height,
		"fps", b.fps, "audio", b.hasAudio)
	return chain, nil
}

// runBrowser is the layer's feeder goroutine: it launches Chrome, connects
// CDP, and runs the two-phase feed until Close (or Chrome's exit). A frame
// source (the CDP screencast stream when the Chrome build delivers it, else
// Page.captureScreenshot polling) keeps the latest frame in a box, and the
// push loop puts frames onto the appsrc, always the freshest one.
//
// Why the stream: headless Chrome's captureScreenshot has a ~50ms floor at
// 1080p (SwiftShader raster + PNG encode, verified: unchanged by GPU
// flags, optimizeForSpeed, fromSurface), which caps content updates at
// ~20fps — 3:1 frame repeats on a 60fps pipeline, visible as jank. The
// Page.startScreencast stream delivers PNG (alpha-preserving) frames at
// vsync cadence, decoupled from the capture round trip, so the content
// updates at the page's own refresh rate. The screencast stream only sends
// frames when the page repaints, so stream silence is normal; the polling
// fallback starts only if the stream never delivers a frame at all (a
// Chrome build where screencast is dead), and the two frame sources never
// run side by side.
func (b *BrowserOverlay) runBrowser() {
	defer close(b.done)
	sess, err := newCDPSession(b.ctx, b.url, b.sink, b.width, b.height)
	if err != nil {
		slog.Error("layer: failed to launch Chrome", "id", b.L.ID, "err", err)
		return
	}
	defer sess.stop()

	// One context for the whole feeder: done when the layer is closed or
	// Chrome exits. Every loop below watches only this.
	ctx, cancel := context.WithCancel(b.ctx)
	pollDone := make(chan struct{})
	defer func() {
		cancel()
		<-pollDone // the poller may be inside a capture call on sess
	}()
	go func() {
		select {
		case <-sess.Stopped():
			slog.Info("layer: Chrome exited", "id", b.L.ID)
			cancel()
		case <-ctx.Done():
		}
	}()

	// Surface the render-backend decision so the flags driving the page's
	// raster are observable. renderflags is the decision (GPU vs
	// SwiftShader). webgl is only supplementary: in headless Chrome the
	// WebGL device string reports ANGLE/SwiftShader-Vulkan even when GPU
	// raster is active (WebGL routing is separate from the compositor
	// rasterizer), so it must not be read as the raster backend.
	slog.Info("layer: browser render backend",
		"id", b.L.ID, "renderflags", strings.Join(renderBackendFlags(), " "),
		"webgl", sess.glRenderer(ctx))

	frames := newLatestFrame()
	b.startFrameSource(ctx, sess, frames, pollDone)
	if b.hasAudio {
		go b.routeAudio(ctx)
	}

	// First frame: wait for one so the appsrc starts with real content —
	// pngdec needs a frame to negotiate caps.
	if !frames.waitFirst(ctx) {
		return
	}
	b.pushLoop(ctx, frames)
}

// interval is one frame period at the layer's framerate.
func (b *BrowserOverlay) interval() time.Duration {
	return time.Second / time.Duration(b.fps)
}

// startFrameSource starts whatever fills frames: the screencast stream, or
// the captureScreenshot poller (pollLoop, which closes pollDone when it
// exits) when the stream cannot be started or never delivers.
func (b *BrowserOverlay) startFrameSource(ctx context.Context, sess *cdpsession, frames *latestFrame, pollDone chan<- struct{}) {
	// The stream's first frame disarms the startup watchdog. Silence after
	// that is a static page, not a broken stream.
	streamStarted := make(chan struct{})
	var streamStartedOnce sync.Once

	// Screencast frames arrive decoded on the CDP read-loop goroutine; the
	// box keeps only the latest.
	sess.SetStreamHandler(func(frame []byte) {
		frames.Store(frame)
		streamStartedOnce.Do(func() { close(streamStarted) })
	})

	startCtx, startCancel := context.WithTimeout(ctx, 5*time.Second)
	err := sess.startScreencast(startCtx)
	startCancel()
	streamed := err == nil
	if err != nil && ctx.Err() == nil {
		slog.Warn("layer: CDP screencast unavailable, polling captureScreenshot",
			"id", b.L.ID, "err", err)
	}
	slog.Info("layer: browser feeder ready (CDP connected)",
		"id", b.L.ID, "stream", streamed)

	// The poller sleeps until woken; one pending wake-up is enough.
	wake := make(chan struct{}, 1)
	go b.pollLoop(ctx, sess, frames, wake, pollDone)
	if !streamed {
		wake <- struct{}{}
		return
	}

	// Startup-only fallback: wait for the stream's first frame within the
	// watchdog window. No frame by then means this Chrome build's
	// Page.startScreencast is dead — fall back to captureScreenshot for
	// the layer's lifetime. Once the first frame lands the watchdog is
	// done for good: a static page silences the stream (it only sends on
	// repaint) and that is NOT a failure. Starting the polling fallback
	// alongside a working stream tears the feed apart (measured: two
	// competing frame sources took the 1080p feed from 60 to ~45 fps with
	// frame skips), so it must never run on a live stream.
	go func() {
		select {
		case <-streamStarted:
		case <-ctx.Done():
		case <-time.After(browserStreamWatchdog):
			slog.Warn("layer: CDP stream never started, falling back to captureScreenshot",
				"id", b.L.ID)
			sess.stopScreencast(context.Background())
			wake <- struct{}{}
		}
	}()
}

// pollLoop is the fallback frame source: once woken, it captures
// Page.captureScreenshot frames at up to one per frame interval until ctx
// is done.
func (b *BrowserOverlay) pollLoop(ctx context.Context, sess *cdpsession, frames *latestFrame, wake <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	select {
	case <-wake:
	case <-ctx.Done():
		return
	}
	interval := b.interval()
	next := time.Now()
	for ctx.Err() == nil {
		data, err := sess.capture(ctx)
		if err != nil {
			slog.Debug("layer: capture failed", "id", b.L.ID, "err", err)
			if !sleepCtx(ctx, interval) {
				return
			}
			next = time.Now()
			continue
		}
		frames.Store(data)
		// At most one capture per interval: sleep until the next slot if
		// ahead, resync if behind (Chrome slower than the target rate —
		// the normal case for this path).
		next = next.Add(interval)
		if wait := time.Until(next); wait <= 0 {
			next = time.Now()
		} else if !sleepCtx(ctx, wait) {
			return
		}
	}
}

// sleepCtx waits d; it reports false when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pushLoop is the push phase: content-driven cadence, keepalive ticker.
//
// Every new frame is pushed as it lands (the fresh signal), which keeps the
// output in phase with the page's own vsync — the screencast stream is
// produced on the page's raster cadence. A pure fixed-ticker push instead
// runs its own ~60 Hz clock; the two clocks beat against each other and the
// ticker sometimes re-pushes the previous frame between two raster frames,
// so one content step spans two ticks and the next spans two frames: a
// 0-px-then-2x stall-then-teleport beat, visible on an 8 px/frame line test
// (measured 0.9% beat pairs pre-fix). Ticker pushes only ever re-push the
// SAME content (keepalive on a static page, where they are required: a live
// stream dies if a single source layer goes silent) and are
// content-invisible.
//
// The ticker also bounds the cadence: it skips when a fresh push has
// supplied a frame within the interval, so the push rate stays ~1/fps in
// total. The appsrc is single-frame (leaky downstream), so a push costs at
// most one decode, never backlog.
func (b *BrowserOverlay) pushLoop(ctx context.Context, frames *latestFrame) {
	interval := b.interval()
	lastPush := time.Now()
	pushLatest := func() bool {
		if !b.pushFrame(ctx, frames.Load()) {
			return false
		}
		lastPush = time.Now()
		return true
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-frames.fresh:
			if !pushLatest() {
				return
			}
		case <-tick.C:
			if time.Since(lastPush) >= interval && !pushLatest() {
				return
			}
		}
	}
}

// latestFrame is a one-slot box holding the most recent captured frame.
// Store is called by the frame source (screencast read loop or the poller);
// Load and waitFirst by the push goroutine.
type latestFrame struct {
	mu   sync.Mutex
	data []byte

	first     chan struct{} // closed on the first Store
	firstOnce sync.Once

	// fresh is a coalescing "frame arrived" signal for the push goroutine:
	// cap 1, non-blocking send. A signal lost to coalescing costs nothing —
	// the pending one makes the push loop load the latest frame.
	fresh chan struct{}
}

func newLatestFrame() *latestFrame {
	return &latestFrame{first: make(chan struct{}), fresh: make(chan struct{}, 1)}
}

// Store publishes a new frame and wakes the push goroutine (coalesced).
func (l *latestFrame) Store(data []byte) {
	l.mu.Lock()
	l.data = data
	l.mu.Unlock()
	l.firstOnce.Do(func() { close(l.first) })
	select {
	case l.fresh <- struct{}{}:
	default:
	}
}

// Load returns the latest frame (nil before the first Store).
func (l *latestFrame) Load() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.data
}

// waitFirst blocks until the first frame has landed; it reports false when
// ctx ends first.
func (l *latestFrame) waitFirst(ctx context.Context) bool {
	select {
	case <-l.first:
		return true
	case <-ctx.Done():
		return false
	}
}

// pushFrame pushes one captured frame into the appsrc, retrying through
// FLUSHING. A scene switch or a chain-affecting layer op holds the whole
// pipeline at READY for the duration of the op batch, and the appsrc
// answers FLUSHING for that whole window. A browser layer can survive the
// switch (same id in both scenes → its chain is kept, not rebuilt), so the
// feeder must outlive the freeze: back off and re-push until the appsrc
// accepts again. The same frame is re-pushed, so a freeze only costs
// freshness (max one stale frame after resume), not the stream.
// Returns false when the feeder must exit (ctx done); true once the frame
// was pushed, or dropped on any other flow error.
func (b *BrowserOverlay) pushFrame(ctx context.Context, frame []byte) bool {
	for {
		if gstutil.PushBytes(b.appSrc, frame) != gst.FlowFlushing {
			return true
		}
		if !sleepCtx(ctx, browserFlushBackoff) {
			return false
		}
	}
}

// routeAudio re-routes Chrome's audio stream onto the layer's null sink as a
// fallback for the PULSE_SINK env pin. Pages that stay silent for a long
// time (donation alerts) create no sink-input until the first sound, so this
// keeps trying for the layer's whole lifetime instead of giving up after
// 15 seconds (best effort: audio stays video-only when this fails).
func (b *BrowserOverlay) routeAudio(ctx context.Context) {
	for {
		if id := findSinkInputID("chrome", b.sink); id != "" {
			if err := routeSinkInput(id, b.sink); err != nil {
				slog.Warn("layer: audio reroute failed", "id", b.L.ID, "err", err)
				return
			}
			slog.Info("layer: page audio routed to null sink", "id", b.L.ID, "sink", b.sink)
			return
		}
		if !sleepCtx(ctx, 2*time.Second) {
			return
		}
	}
}

// AudioChain returns the element chain that captures the page's audio from
// the layer's Pulse null-sink monitor as raw PCM. The compositor mixes all
// layers' chains into one shared encoder + mpegtsmux pad (one audio track:
// MPEG-TS demuxers such as OBS only play the stream's first program, so
// per-layer AAC programs would drop every layer after the first).
// Returns (nil, nil) when the layer has no audio session.
func (b *BrowserOverlay) AudioChain() (*layer.Chain, error) {
	if !b.hasAudio {
		return nil, nil
	}
	head, err := layer.Build(
		layer.Spec{Factory: "pulsesrc", Name: b.L.ID + "-audsrc", Props: map[string]any{
			"device": b.sink + ".monitor",
		}},
		layer.Spec{Factory: "audioconvert", Name: b.L.ID + "-audconv"},
	)
	if err != nil {
		return nil, err
	}
	// Audio filters (volume, equalizer, audiodynamic, pitch) sit between the
	// converter and the tail; they operate on raw PCM, which the
	// compositor's shared audiomixer then combines with the other layers.
	// The mixer takes the chains' native (float) format — no capsfilter:
	// audiomixer sink pads do not accept S16LE and reject a pinned link.
	filters, err := layer.AudioFilters(b.L)
	if err != nil {
		return nil, err
	}
	return layer.StaticChain(append(head, filters...)...), nil
}

// DefaultProperties returns the compositor-pad properties for this layer.
// Browser-specific properties (framerate, quality) are kept out of the pad
// property map: the compositor writes every entry onto the pad, and GStreamer
// rejects unknown pad properties (state-change failure).
func (b *BrowserOverlay) DefaultProperties() map[string]float64 {
	props := layer.DefaultProps(b.L)
	delete(props, "framerate")
	delete(props, "quality")
	return props
}

// Close stops the feeder, kills Chrome, and unloads the Pulse null sink. It is
// idempotent. Call it after the compositor has removed the chain elements
// (so the audio source's monitor is not pulled from under a live element).
func (b *BrowserOverlay) Close() error {
	b.closed.Do(func() {
		if !b.started {
			return
		}
		b.cancel()
		<-b.done
		if b.sinkUp != nil {
			b.sinkUp()
		}
	})
	return nil
}

// parseConfig resolves url/width/height/fps from the scene descriptor.
// width/height in the scene's Properties are the compositor-pad box (the size
// the compositor scales this layer to); the capture resolution uses them when
// present so we don't over-capture, else a browser-native default. The
// legacy "quality" property is ignored (capture is lossless PNG now).
func (b *BrowserOverlay) parseConfig() {
	b.url = b.L.Source
	b.width = layer.IntProp(b.L, "width", browserDefaultWidth)
	b.height = layer.IntProp(b.L, "height", browserDefaultHeight)
	b.fps = layer.IntProp(b.L, "framerate", browserDefaultFPS)
}

// sanitizeSinkID makes a scene layer id safe for a Pulse sink name.
func sanitizeSinkID(id string) string {
	out := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		out = []byte("layer")
	}
	return string(out)
}
