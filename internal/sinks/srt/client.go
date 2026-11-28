// Package srt sends the encoded MPEG-TS bytes out over SRT and exposes the
// live SRT statistics the adaptive balancer consumes. This is the Go stand-in
// for the original CtrlIRLCoder's appsink-pull + direct-libsrt sender: GStreamer
// hands us the bytes (via the terminal appsink tap, internal/gstpull) and this package
// owns the SRT socket, the write path, reconnection, and the statistics that
// feed bitrate control.
//
// We send SRT ourselves (no in-pipeline SRT sink) because the controller
// needs low-level SRT feedback (RTT, send-buffer occupancy, packet loss,
// ACKs) at high frequency. A Go-owned SRTSOCKET gives direct access to all
// of that via srtgo's stats API.
package srt

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ctrlirl-coder/internal/sinks"
	"github.com/haivision/srtgo"
)

// writePollTimeout bounds one wait for write readiness inside the batched
// send (sendbatch_cgo.go): when the SRT send buffer is full the C loop waits
// this long on the socket's epoll before trying again. 120 ms matches the
// default SRT latency.
const writePollTimeout = 120 * time.Millisecond

// writeBudget is the total time a single Write may spend waiting for the send
// buffer — several poll waits — before the rest of the batch is dropped. The
// reference encoder blocks in srt_send until the SRT send buffer drains; we
// approximate that with a bounded, pipeline-latency-scale window (a few
// hundred ms) so a momentarily-full buffer is not shed. Bounded so a
// genuinely dead/congested link can't stall the GStreamer data path forever.
const writeBudget = 400 * time.Millisecond

var srtInitOnce sync.Once

func initSRT() { srtInitOnce.Do(srtgo.InitSRT) }

// EnsureSRTBackend verifies the loaded libsrt is the embedded CERALIVE fork
// before any SRT work. It is idempotent. The app calls it once during startup so
// a mis-built (stock-libsrt) binary fails fast instead of silently using the
// wrong backend.
func EnsureSRTBackend() error { return verifyLoadedSRT() }

// Client owns one SRT sender socket to a fixed destination. It is safe for
// concurrent use: the gstpull data path calls Write from its own goroutine
// while the housekeeping goroutine may be (re)connecting.
//
// The live socket sits behind an atomic.Pointer (nil = down), so the write
// hot path reads it with a single atomic load — no mutex. The mutex guards
// only lifecycle state (reconnect backoff: downAt, attempts), touched on
// the rare down/up transition.
type Client struct {
	host string
	port uint16
	opts map[string]string
	cfg  ClientConfig

	sock atomic.Pointer[srtgo.SrtSocket] // live socket; nil = down

	mu       sync.Mutex
	downAt   time.Time // last time we marked the link down (for backoff)
	attempts int

	// rate measures the actual bytes we push per second. SRT's own
	// MbpsSendRate is unreliable (it reads ~0 on low-RTT / loopback links),
	// and the adaptive balancer's send-buffer threshold collapses to zero when
	// it is fed a zero throughput — so we report our measured rate instead as
	// the authoritative throughput. See sendRateTracker.
	rate sendRateTracker

	log *slog.Logger

	closeOnce sync.Once
	done      chan struct{} // closed by Close
}

// ClientConfig is the SRT sender configuration, taken from config.toml [srt].
type ClientConfig struct {
	URI        string // srt://host:port — the authoritative destination
	LatencyMs  int
	StreamID   string
	Passphrase string
	// MaxBandwidthKbps, when > 0, caps the SRT send rate in kbit/s.
	MaxBandwidthKbps int
}

// socketOptions maps the config onto srtgo socket options.
func socketOptions(cfg ClientConfig) map[string]string {
	opts := map[string]string{
		// Live transport type: this is a live stream, and it also keeps the
		// SRT payload cap at the 1316-byte live value that Write chunks to.
		"transtype": "live",
		"latency":   strconv.Itoa(cfg.LatencyMs),
	}
	if cfg.StreamID != "" {
		opts["streamid"] = cfg.StreamID
	}
	if cfg.Passphrase != "" {
		opts["passphrase"] = cfg.Passphrase
	}
	if cfg.MaxBandwidthKbps > 0 {
		// SRTO_MAXBW is in bytes per second.
		opts["maxbw"] = strconv.Itoa(cfg.MaxBandwidthKbps * 1000 / 8)
	}
	return opts
}

// NewClient parses the destination URI and prepares a (not-yet-connected)
// sender. It does not block; call Start (or Dial) to connect.
func NewClient(cfg ClientConfig, log *slog.Logger) (*Client, error) {
	host, port, err := parseURI(cfg.URI)
	if err != nil {
		return nil, err
	}
	initSRT()
	return &Client{
		host: host,
		port: port,
		opts: socketOptions(cfg),
		cfg:  cfg,
		log:  log,
		done: make(chan struct{}),
	}, nil
}

// parseURI extracts host and port from an srt:// URI. It also accepts a bare
// host:port so tests don't have to write the scheme.
func parseURI(uri string) (string, uint16, error) {
	s := strings.TrimPrefix(strings.TrimSpace(uri), "srt://")
	if s == "" {
		return "", 0, fmt.Errorf("empty SRT URI")
	}
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		// No port given — SRT requires one.
		return "", 0, fmt.Errorf("SRT URI %q must be host:port", uri)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return "", 0, fmt.Errorf("SRT URI %q has bad port %q: %w", uri, portStr, err)
	}
	if host == "" {
		return "", 0, fmt.Errorf("SRT URI %q has no host", uri)
	}
	return host, uint16(port), nil
}

// Dial performs a single connection attempt.
func (c *Client) Dial() error {
	initSRT()
	sock := srtgo.NewSrtSocket(c.host, c.port, c.opts)
	if sock == nil {
		return fmt.Errorf("srt: socket create failed for %s:%d", c.host, c.port)
	}
	if err := sock.Connect(); err != nil {
		sock.Close()
		return fmt.Errorf("srt: connect %s:%d: %w", c.host, c.port, err)
	}
	if old := c.sock.Swap(sock); old != nil {
		old.Close()
	}
	c.mu.Lock()
	c.attempts = 0
	c.mu.Unlock()
	c.log.Info("srt: connected", "host", c.host, "port", c.port, "latency_ms", c.cfg.LatencyMs)
	return nil
}

// backoffDuration is the reconnect delay after `attempts` failures:
// 500 ms base, doubling, 5 s cap.
func backoffDuration(attempts int) time.Duration {
	d := 500 * time.Millisecond
	for i := 0; i < attempts && d < 5*time.Second; i++ {
		d *= 2
	}
	return d
}

// Start dials with exponential backoff until the first connection succeeds or
// ctx is cancelled, then launches the housekeeping goroutine that keeps the
// link alive (reconnecting after write failures). The pipeline keeps running
// throughout; a down link drops data (see the data path) rather than stalling.
func (c *Client) Start(ctx context.Context) error {
	attempts := 0
	for {
		if err := c.Dial(); err == nil {
			break
		} else {
			c.log.Warn("srt: connect failed, retrying", "err", err, "backoff", backoffDuration(attempts).String())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return fmt.Errorf("srt: client closed")
		case <-time.After(backoffDuration(attempts)):
			attempts++
		}
	}
	go c.housekeeping(ctx)
	go c.monitor(ctx)
	return nil
}

// monitor periodically logs an SRT stats snapshot so link health (RTT, send
// buffer, retransmits, loss) is visible without a receiver. Logging is cheap
// (1/s) and matches the original's stream-stats line.
func (c *Client) monitor(ctx context.Context) {
	tick := time.NewTicker(1000 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-tick.C:
			st, err := c.Stats()
			if err != nil {
				continue
			}
			c.log.Info("stream stats",
				"rtt_ms", st.RTT,
				"send_rate_mbps", round2(st.SendRateMbps),
				"bandwidth_mbps", round2(st.BandwidthEstimate),
				"sndbuf_bytes", st.SendBufferBytes,
				"pkt_loss_pct", round2(st.PacketLossRate),
				"retrans_pkt", st.PktRetransTotal,
				"sent_total", st.PacketsSentTotal,
			)
		}
	}
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

// housekeeping watches for a down link and reconnects. It is a no-op while the
// link is healthy.
func (c *Client) housekeeping(ctx context.Context) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-tick.C:
			if c.sock.Load() != nil {
				continue
			}
			// Exponential backoff between attempts (base 0.5s, cap 5s).
			c.mu.Lock()
			next := backoffDuration(c.attempts) - time.Since(c.downAt)
			c.mu.Unlock()
			if next > 0 {
				select {
				case <-ctx.Done():
					return
				case <-c.done:
					return
				case <-time.After(next):
				}
			}
			c.mu.Lock()
			c.attempts++
			c.mu.Unlock()
			_ = c.Dial()
		}
	}
}

// Write sends one batch of SRT payload (accumulated TS packets) to the socket
// in a single cgo crossing — see sendbatch_cgo.go. The C side splits the batch
// into SRT live-safe 1316-byte chunks and holds the batch for a bounded window
// (writeBudget) while the SRT send buffer drains, doing the write-ready wait in
// C rather than round-tripping to Go per packet. A temporary "send buffer not
// ready" (budget exhausted) drops the batch and keeps the link UP; only genuine
// connection loss tears the link down. If the link is down the caller drops the
// batch.
func (c *Client) Write(b []byte) (int, error) {
	sock := c.sock.Load()
	if sock == nil {
		return 0, ErrNotConnected
	}
	sent, code := cgoSendBatch(sock, b)
	if code == 0 {
		c.rate.record(sent)
		return sent, nil
	}
	if code.Temporary() {
		// Send buffer not ready within budget: drop this batch but keep the
		// link UP — the socket is healthy, it was just momentarily full.
		return sent, fmt.Errorf("srt: send buffer not ready, dropped: srt=%d", int(code))
	}
	// Genuine connection loss (EInvSock, ESClosed, rejected, ...).
	c.markDown()
	return sent, fmt.Errorf("srt: write: srt=%d", int(code))
}

// markDown records a link failure so housekeeping reconnects with backoff.
func (c *Client) markDown() {
	if c.sock.Swap(nil) == nil {
		return // already down
	}
	c.mu.Lock()
	c.downAt = time.Now()
	c.mu.Unlock()
	c.log.Warn("srt: send failed, link down (reconnecting)")
}

// Stats returns a mapped statistics sample, or an error when the link is down.
func (c *Client) Stats() (sinks.Stats, error) {
	sock := c.sock.Load()
	if sock == nil {
		return sinks.Stats{}, ErrNotConnected
	}
	raw, err := sock.Stats()
	if err != nil {
		return sinks.Stats{}, err
	}
	st := mapStats(raw)
	// Report the throughput we actually push, not SRT's own rate field, which
	// reads ~0 on low-RTT links and would zero the balancer's send-buffer
	// threshold (see sendRateTracker).
	if m := c.rate.mbps(); m > 0 {
		st.SendRateMbps = m
	}
	return st, nil
}

// Connected reports whether the link is currently up.
func (c *Client) Connected() bool { return c.sock.Load() != nil }

// Close releases the socket and stops housekeeping. Safe to call more than
// once.
func (c *Client) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	if old := c.sock.Swap(nil); old != nil {
		old.Close()
	}
	return nil
}

// sendRateTracker measures the actual bytes-per-second handed to the SRT
// socket. srtgo's own MbpsSendRate stat reads ~0 on low-RTT / loopback links
// (its packet-rate accounting doesn't tick there), and the adaptive balancer's
// send-buffer threshold collapses to zero when fed a zero throughput — so we
// measure on the write path and report it as the authoritative throughput.
type sendRateTracker struct {
	mu     sync.Mutex
	bytes  atomic.Uint64
	lastT  time.Time
	lastB  uint64
	smooth float64
}

// record accounts for n successfully-sent bytes (lock-free: the write hot
// path calls it per chunk).
func (t *sendRateTracker) record(n int) {
	if n <= 0 {
		return
	}
	t.bytes.Add(uint64(n))
}

// mbps returns the measured send rate in Mb/s, smoothed so a quiet interval
// cannot swing it to zero.
func (t *sendRateTracker) mbps() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	cur := t.bytes.Load()
	if t.lastT.IsZero() {
		// Prime the baseline. Without this we return on the first call without
		// ever setting lastT, so the tracker can never start measuring and
		// mbps() returns 0 forever — which zeroes the balancer's buffer
		// threshold (RTT_TO_BS) and pins the bitrate to the floor.
		t.lastT = now
		t.lastB = cur
		return t.smooth
	}
	dt := now.Sub(t.lastT).Seconds()
	if dt < 0.05 {
		return t.smooth
	}
	t.lastT = now
	inst := float64(cur-t.lastB) * 8.0 / dt / 1e6
	t.lastB = cur
	if t.smooth == 0 {
		t.smooth = inst
	} else {
		t.smooth = t.smooth*0.7 + inst*0.3
	}
	return t.smooth
}
