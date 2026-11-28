package srtla

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"ctrlirl-coder/internal/sinks"
)

// regRetryAfter is how long a registration attempt (REG1 sent, REG2 awaited)
// blocks the next one.
const regRetryAfter = 4 * time.Second

// Sender aggregates complete SRT datagrams across registered SRTLA uplinks.
// The SRT endpoint owns session negotiation and retransmission; SRT feedback
// is returned to it through the handler installed with SetSRTFeedback.
//
// Lock order (outermost first): lifecycleMu → upMu → mu → the scheduler's
// lock → Uplink.mu. feedbackMu is a leaf. The per-packet path (SendPacket)
// takes upMu shared, then the scheduler and uplink locks.
type Sender struct {
	cfg *Config
	log *slog.Logger
	now func() time.Time

	// Allow overriding the resolved receiver chain (tests).
	resolver func(host string, port int) ([]*net.UDPAddr, error)

	groupID     [IDLen]byte
	lifecycleMu sync.Mutex
	upMu        sync.RWMutex // guards config, uplinks, scheduler, and destinations
	uplinks     []*Uplink
	sched       Scheduler

	feedbackMu sync.RWMutex
	feedback   func([]byte) error

	// Receiver failover chain + current index (spec: walk resolved addrs).
	dests   []*net.UDPAddr
	destIdx int
	dead    atomic.Bool // fatal: all addrs exhausted

	mu           sync.Mutex
	pendingReg2  *Uplink // uplink awaiting its REG2; nil = none
	pendingRegT  time.Time
	activeConns  int
	hasConnected bool
	allFailedAt  time.Time // zero = at least one uplink alive

	stopOnce sync.Once
	stop     chan struct{}
	start    sync.Once
	done     chan struct{}

	// Telemetry counters for Stats.
	sentPkts  atomic.Int64
	sentBytes atomic.Int64
	nakTotal  atomic.Int64
	ackTotal  atomic.Int64
	sendMeter *meter
	nakMeter  *meter
}

// New constructs a Sender from cfg. The receiver is resolved lazily in Run so
// construction does not touch the network. log may be nil (a Noop logger is
// used).
func New(cfg *Config, log *slog.Logger, now func() time.Time) (*Sender, error) {
	norm := cfg.Normalized()
	if norm.ReceiverHost == "" {
		return nil, errors.New("srtla: ReceiverHost required")
	}
	if len(norm.UplinkIPs) == 0 {
		return nil, errors.New("srtla: at least one UplinkIP required")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if now == nil {
		now = time.Now
	}
	s := &Sender{
		cfg:  &norm,
		log:  log,
		now:  now,
		done: make(chan struct{}),
		stop: make(chan struct{}),
	}
	s.resolver = s.resolveReceiver
	s.uplinks = buildUplinks(&norm, now)
	s.sched = NewScheduler(norm.Mode, log)
	s.groupID = RandomGroupID()
	s.sendMeter = &meter{}
	s.nakMeter = &meter{}
	s.sendMeter.init(now())
	s.nakMeter.init(now())
	return s, nil
}

// buildUplinks constructs one Uplink per configured source IP.
func buildUplinks(cfg *Config, now func() time.Time) []*Uplink {
	us := make([]*Uplink, 0, len(cfg.UplinkIPs))
	for i, ip := range cfg.UplinkIPs {
		us = append(us, NewUplink(i+1, ip, cfg, now))
	}
	return us
}

// snap returns a consistent snapshot of the current uplink set and scheduler
// (safe to use while Reload swaps them).
func (s *Sender) snap() (ups []*Uplink, sc Scheduler) {
	s.upMu.RLock()
	ups, sc = s.uplinks, s.sched
	s.upMu.RUnlock()
	return
}

// mode returns the current scheduler mode (safe to call while Reload swaps
// the config).
func (s *Sender) mode() string {
	s.upMu.RLock()
	defer s.upMu.RUnlock()
	return s.cfg.Mode
}

// Reload re-configures the sender from a new config (e.g. after a SIGHUP
// config reload changes uplink_ips, mode, or the receiver). New destinations
// and sockets are prepared before replacing the current uplinks, so a failed
// reload leaves the existing sender running. It is safe to call at runtime.
func (s *Sender) Reload(cfg Config) error {
	norm := cfg.Normalized()
	if norm.ReceiverHost == "" {
		return errors.New("srtla: ReceiverHost required")
	}
	if len(norm.UplinkIPs) == 0 {
		return errors.New("srtla: at least one UplinkIP required")
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.stopped() {
		return errors.New("srtla: sender is closed")
	}
	addrs, err := s.resolveDestinations(&norm)
	if err != nil {
		return err
	}
	ups := buildUplinks(&norm, s.now)
	if err := connectUplinks(ups, addrs[0]); err != nil {
		return err
	}

	s.upMu.Lock()
	if s.stopped() {
		s.upMu.Unlock()
		closeUplinks(ups)
		return errors.New("srtla: sender is closed")
	}
	old := s.uplinks
	s.cfg = &norm
	s.sched = NewScheduler(norm.Mode, s.log)
	s.uplinks = ups
	s.dests = addrs
	s.destIdx = 0
	s.mu.Lock()
	s.pendingReg2 = nil
	s.pendingRegT = time.Time{}
	s.activeConns = 0
	s.hasConnected = false
	s.allFailedAt = time.Time{}
	s.mu.Unlock()
	s.upMu.Unlock()

	closeUplinks(old)
	for _, u := range ups {
		go s.uplinkLoop(u)
	}

	s.log.Info("srtla: sender reloaded", "uplinks", len(norm.UplinkIPs), "mode", norm.Mode)
	return nil
}

// resolveReceiver resolves the receiver host:port into its UDP addresses.
func (s *Sender) resolveReceiver(host string, port int) ([]*net.UDPAddr, error) {
	res, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return nil, err
	}
	return []*net.UDPAddr{res}, nil
}

func (s *Sender) resolveDestinations(cfg *Config) ([]*net.UDPAddr, error) {
	addrs, err := s.resolver(cfg.ReceiverHost, cfg.ReceiverPort)
	if err != nil {
		return nil, fmt.Errorf("srtla: resolve receiver %s:%d: %w", cfg.ReceiverHost, cfg.ReceiverPort, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("srtla: no addresses for receiver %s", cfg.ReceiverHost)
	}
	return addrs, nil
}

func connectUplinks(ups []*Uplink, dest *net.UDPAddr) error {
	for _, u := range ups {
		u.SetDest(dest)
		if err := u.Connect(); err != nil {
			closeUplinks(ups)
			return err
		}
	}
	return nil
}

func closeUplinks(ups []*Uplink) {
	for _, u := range ups {
		u.Close()
	}
}

// Uplinks exposes the uplinks for inspection (tests, stats).
func (s *Sender) Uplinks() []*Uplink {
	s.upMu.RLock()
	defer s.upMu.RUnlock()
	return s.uplinks
}

// ActiveConnections reports the count of live (non-timed-out) uplinks.
func (s *Sender) ActiveConnections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeConns
}

// HasConnected reports whether at least one uplink reached REG3.
func (s *Sender) HasConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hasConnected
}

// Failover reports whether the receiver address chain is exhausted.
func (s *Sender) Failover() bool { return s.dead.Load() }

// SendPacket relays an intact SRT data or control datagram. The caller retains
// ownership of pkt and may reuse it after this method returns.
func (s *Sender) SendPacket(pkt []byte) error {
	if len(pkt) < SRTMinLen {
		return fmt.Errorf("srtla: SRT packet is too short: %d bytes", len(pkt))
	}
	now := s.now()
	ups, sc := s.snap()
	idx := sc.Select(ups, now)
	if idx < 0 {
		return ErrNoUplink
	}
	u := ups[idx]
	if err := u.SendData(pkt, now); err != nil {
		if !s.stopped() {
			s.log.Error("srtla sendto failed, disabling link", "ip", u.IP, "err", err)
			u.MarkDead()
		}
		return err
	}
	s.sentPkts.Add(1)
	s.sentBytes.Add(int64(len(pkt)))
	return nil
}

// SetSRTFeedback installs the synchronous handler for SRT packets from the
// receiver. The handler must not retain the supplied datagram.
func (s *Sender) SetSRTFeedback(handler func([]byte) error) {
	s.feedbackMu.Lock()
	s.feedback = handler
	s.feedbackMu.Unlock()
}

func (s *Sender) forwardSRT(pkt []byte) {
	s.feedbackMu.RLock()
	handler := s.feedback
	s.feedbackMu.RUnlock()
	if handler == nil {
		s.log.Warn("srtla: no SRT feedback handler")
		return
	}
	if err := handler(pkt); err != nil && !s.stopped() {
		s.log.Error("srtla: forward SRT feedback", "err", err)
	}
}

// Stats implements sinks.StatsSource, reporting the congestion-derived
// telemetry the balancer consumes.
func (s *Sender) Stats() (sinks.Stats, error) {
	return s.summary(), nil
}

// Run binds the uplink sockets and starts the per-uplink read loops plus
// housekeeping, blocking until ctx is cancelled or a fatal failover occurs.
func (s *Sender) Run(ctx context.Context) error {
	var startErr error
	s.start.Do(func() {
		startErr = s.startNow()
	})
	if startErr != nil {
		return startErr
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.housekeeping(ctx)

	select {
	case <-ctx.Done():
		s.Close()
		return nil
	case <-s.stop:
		return s.runErr()
	case <-s.done:
		return s.runErr()
	}
}

// startNow binds the sockets once.
func (s *Sender) startNow() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.stopped() {
		return errors.New("srtla: sender is closed")
	}
	s.upMu.RLock()
	cfg := *s.cfg
	s.upMu.RUnlock()
	addrs, err := s.resolveDestinations(&cfg)
	if err != nil {
		return err
	}
	s.upMu.Lock()
	defer s.upMu.Unlock()
	if s.stopped() {
		return errors.New("srtla: sender is closed")
	}
	if err := connectUplinks(s.uplinks, addrs[0]); err != nil {
		return err
	}
	s.dests = addrs
	s.destIdx = 0
	for _, u := range s.uplinks {
		go s.uplinkLoop(u)
	}
	return nil
}

// runErr surfaces a fatal condition that closed s.done.
func (s *Sender) runErr() error {
	if s.dead.Load() {
		return fmt.Errorf("srtla: no available receiver (all uplinks failed, chain exhausted)")
	}
	return nil
}

// stopped reports whether the sender is shutting down.
func (s *Sender) stopped() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

// uplinkLoop reads receiver feedback on one uplink and dispatches it.
func (s *Sender) uplinkLoop(u *Uplink) {
	buf := make([]byte, 4096)
	for {
		c := u.Conn()
		if c == nil {
			return
		}
		n, src, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			if !s.stopped() {
				s.log.Debug("srtla uplink read", "ip", u.IP, "err", err)
			}
			return
		}
		if !u.isReceiver(src) {
			s.log.Debug("srtla: ignoring feedback from unexpected address", "ip", u.IP, "source", src)
			continue
		}
		s.handleFeedback(u, buf[:n:n])
	}
}

// handleFeedback consumes SRTLA control packets and forwards SRT packets to
// the local SRT endpoint, updating uplink congestion state where applicable.
// Every packet except REG_NGP and a rejected registration refreshes the
// uplink's receive liveness.
func (s *Sender) handleFeedback(u *Uplink, b []byte) {
	s.upMu.RLock()
	defer s.upMu.RUnlock()
	ups, sc := s.uplinks, s.sched
	if !slices.Contains(ups, u) {
		return // an uplink a reload has already replaced
	}
	now := s.now()

	switch typ := Type(b); typ {
	case TypeREGNGP:
		// NGP must not update last_recv (it would mask dead links).
		s.startRegistration(u, now)

	case TypeREG2:
		s.confirmGroup(u, ups, b, now)

	case TypeREG3:
		s.mu.Lock()
		s.hasConnected = true
		if !u.Established() {
			s.activeConns++
		}
		s.mu.Unlock()
		u.RegDone()
		u.TouchRecv()
		s.log.Info("srtla connection established", "ip", u.IP)

	case TypeACK:
		for p := ackSeqs(b); len(p) >= 4; p = p[4:] {
			sc.Feedback(u, seqAt(p), FeedbackSrtlaAck, now)
			s.ackTotal.Add(1)
		}
		u.TouchRecv()
		sc.MarkUnacked(u, now)

	case TypeKeepalive:
		// Extended keepalives may carry receiver telemetry.
		if info, ok := ParseExtendedKeepalive(b); ok && info.RTTMs > 0 {
			u.SetRTT(info.RTTMs)
		}
		u.TouchRecv()

	case SRTTypeACK:
		// A cumulative SRT ACK covers the whole stream: every uplink.
		if ack, ok := SRTAckLast(b); ok {
			for _, up := range ups {
				sc.Feedback(up, ack, FeedbackSrtAck, now)
			}
			s.ackTotal.Add(1)
		}
		u.TouchRecv()
		s.forwardSRT(b)

	case SRTTypeNAK:
		// The lost packet sits in exactly one uplink's ring; the others
		// ignore the sequence.
		for _, sn := range SRTNAKLosses(b) {
			for _, up := range ups {
				sc.Feedback(up, sn, FeedbackSrtNak, now)
			}
			s.nakTotal.Add(1)
		}
		u.TouchRecv()
		s.forwardSRT(b)

	case TypeREGErr, TypeREGNAK:
		s.log.Error("srtla: receiver rejected registration", "ip", u.IP, "type", fmt.Sprintf("%#04x", typ))
		u.MarkDead()

	default:
		// SRT data, or any other SRT control packet: the SRT endpoint's.
		if len(b) >= SRTMinLen && (SeqNumber(b) >= 0 || typ <= 0x8008 || typ == 0xffff) {
			u.TouchRecv()
			s.forwardSRT(b)
		} else {
			s.log.Warn("srtla: ignoring unknown receiver packet", "ip", u.IP, "type", fmt.Sprintf("%#04x", typ))
		}
	}
}

// startRegistration answers REG_NGP ("no group, register"): when no uplink
// is active and no attempt is pending, u becomes the pending path and sends
// REG1 with the proposed group ID.
func (s *Sender) startRegistration(u *Uplink, now time.Time) {
	s.mu.Lock()
	canReg := s.activeConns == 0 && s.pendingReg2 == nil && now.After(s.pendingRegT)
	if canReg {
		s.pendingReg2 = u
		s.pendingRegT = now.Add(regRetryAfter)
	}
	group := s.groupID
	s.mu.Unlock()
	if !canReg || u.Send(BuildREG1(group)) == nil {
		return
	}
	// REG1 did not go out: free the slot for another uplink.
	s.mu.Lock()
	if s.pendingReg2 == u {
		s.pendingReg2 = nil
	}
	s.mu.Unlock()
}

// confirmGroup handles REG2 on the pending uplink: the receiver's group ID
// is adopted and every uplink registers itself under it with its own REG2.
func (s *Sender) confirmGroup(u *Uplink, ups []*Uplink, b []byte, now time.Time) {
	s.mu.Lock()
	if s.pendingReg2 != u {
		s.mu.Unlock()
		return
	}
	id, ok := ParseREG2(b)
	if !ok || !halfEquals(id, s.groupID) {
		s.mu.Unlock()
		s.log.Error("srtla REG2 id mismatch", "ip", u.IP)
		return
	}
	s.groupID = id // adopt (normally identical; mirrors reference memcpy)
	s.pendingReg2 = nil
	s.pendingRegT = now.Add(regRetryAfter)
	s.mu.Unlock()
	for _, up := range ups {
		if err := up.Send(BuildREG2(id)); err != nil && !s.stopped() {
			s.log.Warn("srtla: register uplink", "ip", up.IP, "err", err)
		}
	}
}

// housekeeping runs the 1-second maintenance tick (reference
// connection_housekeeping).
func (s *Sender) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			s.tick()
		}
	}
}

// tick is one housekeeping pass.
func (s *Sender) tick() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.stopped() {
		return
	}
	now := s.now()
	s.mu.Lock()
	if s.pendingReg2 != nil && now.After(s.pendingRegT) {
		s.pendingReg2 = nil
	}
	s.mu.Unlock()

	active := 0
	ups, _ := s.snap()
	for _, u := range ups {
		if u.Conn() == nil {
			_ = u.Connect()
		}
		if u.TimedOut() {
			u.MarkDead()
			s.reRegister(u)
			continue
		}
		active++
		if now.Sub(u.LastActivity()) > KeepaliveIdle {
			s.sendKeepalive(u)
		}
	}

	s.upMu.Lock()
	s.mu.Lock()
	s.activeConns = active
	if active == 0 {
		if s.allFailedAt.IsZero() {
			s.allFailedAt = now
		}
		if now.Sub(s.allFailedAt) > FailoverAfter {
			if s.destIdx+1 < len(s.dests) {
				s.destIdx++
				for _, u := range s.uplinks {
					u.SetDest(s.dests[s.destIdx])
				}
				s.allFailedAt = now
				s.log.Warn("srtla failover to next receiver addr", "idx", s.destIdx)
			} else {
				s.log.Error("srtla: no available receiver")
				s.mu.Unlock()
				s.upMu.Unlock()
				s.dead.Store(true)
				s.closeAll()
				return
			}
		}
	} else {
		s.allFailedAt = time.Time{}
	}
	s.mu.Unlock()
	s.upMu.Unlock()
}

// reRegister re-establishes a timed-out uplink (reference housekeeping).
func (s *Sender) reRegister(u *Uplink) {
	s.mu.Lock()
	pending := s.pendingReg2
	group := s.groupID
	s.mu.Unlock()
	if pending == nil {
		_ = u.Send(BuildREG2(group))
	} else if pending == u {
		_ = u.Send(BuildREG1(group))
	}
	u.Reset()
}

// sendKeepalive emits a keepalive for an idle uplink. Enhanced mode carries
// the extended telemetry block; classic uses the legacy 2-byte form.
func (s *Sender) sendKeepalive(u *Uplink) {
	var pkt []byte
	if s.mode() == ModeEnhanced {
		pkt = BuildExtendedKeepalive(u.Telemetry())
	} else {
		pkt = BuildKeepalive()
	}
	_ = u.Send(pkt) // a failed keepalive shows up as a timeout
}

// Close releases the uplink sockets, idempotently.
func (s *Sender) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.closeUplinks()
	return nil
}

func (s *Sender) closeUplinks() {
	s.upMu.Lock()
	closeUplinks(s.uplinks)
	s.upMu.Unlock()
}

// closeAll runs with lifecycleMu held and signals a fatal Run failure.
func (s *Sender) closeAll() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.closeUplinks()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// halfEquals compares the first IDLen/2 bytes (reference REG2 id check).
func halfEquals(a, b [IDLen]byte) bool {
	for i := 0; i < IDLen/2; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ErrNoUplink is returned by SendPacket when no uplink is currently selectable.
var ErrNoUplink = errors.New("srtla: no uplink available")
