package srtla

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Window tuning constants, mirrored from the reference sender (sender.cpp):
// the window is scaled by WindowMult so adjustments stay whole in the
// reference's integer math.
const (
	WindowMin   = 1
	WindowDef   = 20
	WindowMax   = 60
	WindowMult  = 1000
	WindowDecr  = 100 // per NAKed packet
	WindowIncr  = 30  // per ACK with a near-full backlog (minus 1 in the reference)
	MaxNAKEpoch = 10000
)

// KeepaliveIdle is how long an uplink may be idle before a keepalive is sent
// (reference IDLE_TIME). FailoverAfter is how long all uplinks may be dead
// before walking the receiver chain (reference GLOBAL_TIMEOUT).
const (
	KeepaliveIdle = 1 * time.Second
	FailoverAfter = 10 * time.Second
)

// pktRingSz is the per-uplink log of recent data sequence numbers (the
// reference keeps 256).
const pktRingSz = 256

// Uplink is one bonded path: a UDP socket bound to a local source IP,
// carrying the shared SRT stream to the receiver, plus the per-path
// congestion state the scheduler ranks by. Its ACK/NAK math is a faithful
// port of the reference sender's conn_t handling.
type Uplink struct {
	ID   int     // 1-based connection id (sent in extended keepalives)
	IP   string  // the local source address
	cfg  *Config // sender-wide config (window-independent)
	now  func() time.Time
	dest netip.AddrPort // receiver endpoint (swapped on failover); guarded by mu

	conn *net.UDPConn

	mu       sync.Mutex
	ring     [pktRingSz]int32 // 0 = empty slot, else last sent data SN
	nextIdx  int
	inFlight int
	window   int
	lastRecv time.Time // zero = never active
	lastSent time.Time
	dead     bool // force-timeout flag (send failure / housekeeping)
	regDone  bool
	weight   float64 // hysteresis-smoothed enhanced score
	rttMs    uint32  // last measured RTT (0 = unknown → no penalty)
	nakSec   int64   // wall-clock second nakCount belongs to
	nakCount int32   // NAKs seen during nakSec

	// Enhanced-scheduler bookkeeping. It outlives Reset on purpose: a
	// re-registered path keeps its recent NAK history.
	lastNAK time.Time // zero = never NAKed
	stalls  int       // consecutive marks with a full, non-draining backlog
}

// schedState is the snapshot of an uplink a scheduler ranks by.
type schedState struct {
	usable   bool    // confirmed by the receiver (REG3) and not timed out
	window   int     // congestion window (window units * WindowMult)
	inFlight int     // unACKed data packets
	rttMs    uint32  // last measured RTT, 0 = unknown
	nakBurst int32   // NAKs in the current second
	stalls   int     // consecutive non-draining marks
	weight   float64 // previous smoothed enhanced score
}

// schedState reads everything a scheduler needs under one lock.
func (u *Uplink) schedState(now time.Time) schedState {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := schedState{
		usable:   u.regDone && !u.timedOutLocked(now),
		window:   u.window,
		inFlight: u.inFlight,
		rttMs:    u.rttMs,
		stalls:   u.stalls,
		weight:   u.weight,
	}
	if now.Unix() == u.nakSec {
		st.nakBurst = u.nakCount
	}
	return st
}

// NewUplink builds an unconnected uplink. ip is the local address a
// non-blocking UDP socket will bind to (the reference binds the source
// address; port 0 lets the kernel assign one). now overrides the clock in
// tests.
func NewUplink(id int, ip string, cfg *Config, now func() time.Time) *Uplink {
	if now == nil {
		now = time.Now
	}
	return &Uplink{ID: id, IP: ip, cfg: cfg, now: now, window: WindowDef * WindowMult}
}

// SetDest swaps the receiver endpoint (address failover).
func (u *Uplink) SetDest(d *net.UDPAddr) {
	var ap netip.AddrPort
	if d != nil {
		ap = unmapAddrPort(d.AddrPort())
	}
	u.mu.Lock()
	u.dest = ap
	u.mu.Unlock()
}

// isReceiver reports whether a datagram's source is the receiver endpoint.
func (u *Uplink) isReceiver(src netip.AddrPort) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.dest.IsValid() && unmapAddrPort(src) == u.dest
}

// unmapAddrPort normalizes an IPv4-mapped IPv6 address to plain IPv4, so
// endpoints compare equal whichever form the socket layer hands out.
func unmapAddrPort(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// Connect (re)opens and binds the socket. Replaces any existing socket, as
// the reference's open_socket does.
func (u *Uplink) Connect() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.conn != nil {
		_ = u.conn.Close()
		u.conn = nil
	}
	ip := net.ParseIP(u.IP)
	if ip == nil {
		return fmt.Errorf("srtla: invalid uplink ip %q", u.IP)
	}
	bind := &net.UDPAddr{IP: ip}
	conn, err := net.ListenUDP("udp4", bind)
	if err != nil {
		return fmt.Errorf("srtla: bind %s: %w", u.IP, err)
	}
	u.conn = conn
	return nil
}

// SetConn injects an already-connected socket (tests).
func (u *Uplink) SetConn(c *net.UDPConn) {
	u.mu.Lock()
	u.conn = c
	u.mu.Unlock()
}

// Conn returns the current socket, or nil when closed.
func (u *Uplink) Conn() *net.UDPConn {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conn
}

// Close shuts the socket down (graceful shutdown).
func (u *Uplink) Close() {
	u.mu.Lock()
	if u.conn != nil {
		_ = u.conn.Close()
		u.conn = nil
	}
	u.mu.Unlock()
}

// Send delivers a control datagram toward the current receiver endpoint.
func (u *Uplink) Send(b []byte) error {
	u.mu.Lock()
	c, d := u.conn, u.dest
	u.mu.Unlock()
	return u.write(c, d, b, u.now())
}

// SendData relays one SRT datagram. A data packet (one carrying a sequence
// number) is logged in the ring and counted in flight before the write, as
// the reference does — whether or not the write then succeeds.
func (u *Uplink) SendData(pkt []byte, now time.Time) error {
	u.mu.Lock()
	c, d := u.conn, u.dest
	if seq := SeqNumber(pkt); seq >= 0 {
		u.noteSentLocked(seq)
	}
	u.mu.Unlock()
	return u.write(c, d, pkt, now)
}

// write sends b on socket c and, on success, stamps the send time.
func (u *Uplink) write(c *net.UDPConn, dest netip.AddrPort, b []byte, now time.Time) error {
	if c == nil {
		return fmt.Errorf("srtla: uplink %d (%s) not connected", u.ID, u.IP)
	}
	if _, err := c.WriteToUDPAddrPort(b, dest); err != nil {
		return err
	}
	u.markSent(now)
	return nil
}

// markSent stamps the last successful send (keepalive idle timer, stats).
func (u *Uplink) markSent(now time.Time) {
	u.mu.Lock()
	u.lastSent = now
	u.mu.Unlock()
}

// MarkDead forces the uplink to read as timed out until Reset (mirrors the
// reference's last_rcvd=1 "disable until reconnection" nudge).
func (u *Uplink) MarkDead() {
	u.mu.Lock()
	u.dead = true
	u.mu.Unlock()
}

// Window exposes the congestion window (window units * WindowMult).
func (u *Uplink) Window() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.window
}

// InFlight exposes the unACKed data-packet count.
func (u *Uplink) InFlight() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.inFlight
}

// Established reports whether the receiver confirmed this path (REG3).
func (u *Uplink) Established() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.regDone
}

// RegDone marks the connection confirmed by the receiver's REG3.
func (u *Uplink) RegDone() {
	u.mu.Lock()
	u.regDone = true
	u.mu.Unlock()
}

// TimedOut reports whether no packet has arrived within the configured
// connection timeout (the reference's CONN_TIMEOUT gate).
func (u *Uplink) TimedOut() bool {
	now := u.now()
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.timedOutLocked(now)
}

func (u *Uplink) timedOutLocked(now time.Time) bool {
	if u.dead || u.lastRecv.IsZero() {
		return true
	}
	return now.Sub(u.lastRecv) > time.Duration(u.cfg.ConnTimeoutMs)*time.Millisecond
}

// LastActivity returns the most recent receive/send timestamp.
func (u *Uplink) LastActivity() time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	lr, ls := u.lastRecv, u.lastSent
	if ls.After(lr) {
		return ls
	}
	return lr
}

// NoteSent records a data packet in the ring and raises the in-flight count.
func (u *Uplink) NoteSent(sn int32) {
	u.mu.Lock()
	u.noteSentLocked(sn)
	u.mu.Unlock()
}

func (u *Uplink) noteSentLocked(sn int32) {
	u.ring[u.nextIdx] = sn
	u.nextIdx = (u.nextIdx + 1) % pktRingSz
	u.inFlight++
}

// RingIndex is the reference's get_pkt_idx (wrap arithmetic).
func RingIndex(idx, inc int) int {
	idx = idx + inc
	if idx < 0 {
		idx += pktRingSz
	}
	return idx % pktRingSz
}

// RegisterACK applies one SRTLA per-packet ACK: the oldest matching ring
// entry clears, in-flight drops, and the window grows per the reference:
// +WindowIncr-1 when the backlog sits at the window edge, +1 for the
// received packet itself.
func (u *Uplink) RegisterACK(sn int32) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.stalls = 0 // an ACK means the backlog is draining
	idx := RingIndex(u.nextIdx, -1)
	for i := 0; i < pktRingSz && !ringEq(u.ring[idx], sn); i++ {
		idx = RingIndex(idx, -1)
	}
	if ringEq(u.ring[idx], sn) {
		if u.inFlight > 0 {
			u.inFlight--
		}
		u.ring[idx] = 0
		if u.inFlight*WindowMult > u.window {
			u.window += WindowIncr - 1
		}
	}
	u.window += 1
	if u.window > WindowMax*WindowMult {
		u.window = WindowMax * WindowMult
	}
}

// RegisterSRTAck applies a cumulative SRT ACK: every ring entry below the
// acked sequence clears and the in-flight count resets to the remainder
// (reference conn_register_srt_ack).
func (u *Uplink) RegisterSRTAck(ack int32) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.stalls = 0 // an ACK means the backlog is draining
	count := 0
	idx := RingIndex(u.nextIdx, -1)
	for i := 0; i < pktRingSz; i++ {
		if sn := u.ring[idx]; sn != 0 {
			if sn < ack {
				u.ring[idx] = 0
			} else {
				count++
			}
		}
		idx = RingIndex(idx, -1)
	}
	u.inFlight = count
}

// registerNAK finds the NAKed packet in the ring and shrinks the window.
// Returns false when the packet is not in the log (reference: "Didn't find
// NAKed packet ... in our logs").
func (u *Uplink) registerNAK(sn int32) bool {
	idx := RingIndex(u.nextIdx, -1)
	for i := 0; i < pktRingSz; i++ {
		if ringEq(u.ring[idx], sn) {
			u.ring[idx] = 0
			u.window -= WindowDecr
			if u.window < WindowMin*WindowMult {
				u.window = WindowMin * WindowMult
			}
			return true
		}
		idx = RingIndex(idx, -1)
	}
	return false
}

// ringEq is ring-cell equality (0 means "empty").
func ringEq(cell, v int32) bool { return cell != 0 && cell == v }

// RegisterNAK applies one lost SRT sequence number and returns whether it
// was in the log.
func (u *Uplink) RegisterNAK(sn int32) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	found := u.registerNAK(sn)
	u.noteNAK()
	return found
}

// noteNAK counts a NAK in the current one-second bucket (the enhanced
// penalty needs ≥5 NAKs in a short window). Caller must hold u.mu.
func (u *Uplink) noteNAK() {
	now := u.now()
	if sec := now.Unix(); sec != u.nakSec {
		u.nakSec, u.nakCount = sec, 0
	}
	u.nakCount++
	u.lastNAK = now
}

// noteUnacked is the enhanced scheduler's stall mark, taken after SRTLA ACKs
// were applied: a backlog still at enhancedStallThreshold counts as one more
// stall, anything less clears the count. A recent NAK leaves the count alone.
func (u *Uplink) noteUnacked(now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.lastNAK.IsZero() && now.Sub(u.lastNAK) < enhancedNAKQuiet {
		return
	}
	if u.inFlight >= enhancedStallThreshold {
		u.stalls++
	} else {
		u.stalls = 0
	}
}

// NAKBurst returns the NAK count of the current second (enhanced scoring);
// 0 once that second has passed. The bucket is matched by its second, so a
// wall clock that steps (NTP) reads as an empty bucket.
func (u *Uplink) NAKBurst() int32 {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.now().Unix() != u.nakSec {
		return 0
	}
	return u.nakCount
}

// SetRTT stores the last measured RTT in milliseconds (0 = unknown).
func (u *Uplink) SetRTT(ms uint32) {
	u.mu.Lock()
	u.rttMs = ms
	u.mu.Unlock()
}

// RTTMs returns the last measured RTT, 0 when unknown.
func (u *Uplink) RTTMs() uint32 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.rttMs
}

// Telemetry builds the extended-keepalive payload from the live state.
func (u *Uplink) Telemetry() ConnInfo {
	u.mu.Lock()
	defer u.mu.Unlock()
	return ConnInfo{
		ConnID:   uint32(u.ID),
		Window:   int32(u.window / WindowMult),
		InFlight: int32(u.inFlight),
		RTTMs:    u.rttMs,
	}
}

// SetWeight stores the hysteresis-smoothed enhanced score.
func (u *Uplink) SetWeight(w float64) {
	u.mu.Lock()
	u.weight = w
	u.mu.Unlock()
}

// Weight returns the last smoothed enhanced score.
func (u *Uplink) Weight() float64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.weight
}

// Reset drops the connection state when the timeout is first detected
// (reference: window to minimum, in-flight and logs cleared).
func (u *Uplink) Reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lastRecv = time.Time{}
	u.lastSent = time.Time{}
	u.dead = false
	u.window = WindowMin * WindowMult
	u.inFlight = 0
	for i := range u.ring {
		u.ring[i] = 0
	}
	u.regDone = false
	u.weight = 0
}

// TouchRecv is the receive-liveness update (every packet of any type other
// than REG_NGP, per the reference).
func (u *Uplink) TouchRecv() {
	u.mu.Lock()
	u.lastRecv = u.now()
	u.mu.Unlock()
}
