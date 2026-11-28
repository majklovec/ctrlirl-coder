package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"ctrlirl-coder/internal/config"
	"ctrlirl-coder/internal/sinks"
	"ctrlirl-coder/internal/sinks/srt"
	"ctrlirl-coder/internal/sinks/srtla"
)

// srtlaSink keeps the SRT session in the existing SRT client and relays its
// UDP datagrams through the pure-Go SRTLA sender.
type srtlaSink struct {
	sender *srtla.Sender
	client *srt.Client
	relay  *net.UDPConn
	peer   atomic.Pointer[netip.AddrPort] // the local SRT socket; nil until its handshake
	log    *slog.Logger

	closeOnce sync.Once
	closed    chan struct{}
}

func newSRTLASink(cfg *config.Config, log *slog.Logger) (*srtlaSink, error) {
	norm := cfg.SRTLA.ToSRTLAConfig()
	sender, err := srtla.New(&norm, log, time.Now)
	if err != nil {
		return nil, err
	}
	relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, fmt.Errorf("srtla: bind local SRT relay: %w", err)
	}
	client, err := srt.NewClient(srtClientConfig(cfg, "srt://"+relay.LocalAddr().String()), log)
	if err != nil {
		relay.Close()
		return nil, fmt.Errorf("srtla: create SRT client: %w", err)
	}
	s := &srtlaSink{
		sender: sender,
		client: client,
		relay:  relay,
		log:    log,
		closed: make(chan struct{}),
	}
	sender.SetSRTFeedback(s.forwardSRT)
	return s, nil
}

func (s *srtlaSink) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer s.Close()

	stopped := make(chan error, 3)
	go func() { stopped <- s.sender.Run(ctx) }()
	go func() { stopped <- s.relaySRT() }()
	go func() {
		if err := s.client.Start(ctx); err != nil && ctx.Err() == nil {
			stopped <- fmt.Errorf("srtla: SRT client stopped: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-stopped:
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		return errors.New("srtla: transport stopped unexpectedly")
	}
}

func (s *srtlaSink) relaySRT() error {
	buf := make([]byte, 4096)
	for {
		n, addr, err := s.relay.ReadFromUDPAddrPort(buf)
		if err != nil {
			if s.isClosed() {
				return nil
			}
			return fmt.Errorf("srtla: read local SRT packet: %w", err)
		}
		pkt := buf[:n]
		if len(pkt) < srtla.SRTMinLen {
			s.log.Warn("srtla: ignoring short local SRT packet", "bytes", n)
			continue
		}
		if peer := s.peer.Load(); peer == nil || *peer != addr {
			// Only a handshake may introduce (or replace) the local peer.
			if srtla.Type(pkt) != srtla.SRTTypeHandshake {
				s.log.Warn("srtla: ignoring packet from unknown local SRT peer", "peer", addr)
				continue
			}
			s.peer.Store(&addr)
		}
		if err := s.sender.SendPacket(pkt); err != nil && !errors.Is(err, srtla.ErrNoUplink) && !s.isClosed() {
			s.log.Warn("srtla: failed to relay SRT packet", "err", err)
		}
	}
}

func (s *srtlaSink) forwardSRT(pkt []byte) error {
	peer := s.peer.Load()
	if peer == nil {
		return errors.New("local SRT peer has not connected")
	}
	if _, err := s.relay.WriteToUDPAddrPort(pkt, *peer); err != nil {
		return fmt.Errorf("write local SRT packet: %w", err)
	}
	return nil
}

func (s *srtlaSink) Write(pkt []byte) (int, error) {
	return s.client.Write(pkt)
}

func (s *srtlaSink) Stats() (sinks.Stats, error) {
	session, err := s.client.Stats()
	if err != nil {
		return sinks.Stats{}, err
	}
	aggregate, err := s.sender.Stats()
	if err != nil {
		return sinks.Stats{}, err
	}
	// The SRT session knows the end-to-end link (RTT, loss, rates, send
	// buffer); only the feedback and packet counters and the sample time
	// come from the aggregator, which sees every uplink.
	st := session
	st.ACKTotal = aggregate.ACKTotal
	st.PacketsSentTotal = aggregate.PacketsSentTotal
	st.TimestampMs = aggregate.TimestampMs
	return st, nil
}

func (s *srtlaSink) Reload(cfg srtla.Config) error {
	return s.sender.Reload(cfg)
}

// Detailed delegates to the SRTLA sender (metrics surface).
func (s *srtlaSink) Detailed() srtla.Detailed {
	return s.sender.Detailed()
}

func (s *srtlaSink) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

func (s *srtlaSink) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		close(s.closed)
		if err := s.client.Close(); err != nil {
			closeErr = err
		}
		if err := s.sender.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
		if err := s.relay.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	})
	return closeErr
}
