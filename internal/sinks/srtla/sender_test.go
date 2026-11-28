package srtla

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"testing"
	"time"
)

func udpAddr(a net.Addr) *net.UDPAddr { return a.(*net.UDPAddr) }

// readDatagram reads one UDP datagram from c with a timeout.
func readDatagram(t *testing.T, c *net.UDPConn, d time.Duration) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 512)
	n, _, err := c.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("readDatagram: %v", err)
	}
	return buf[:n]
}

// newLocalUDP binds a UDP socket to 127.0.0.1:0.
func newLocalUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("newLocalUDP: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestRegistrationHandshake drives the SRTLA REG1→REG2→REG3 exchange and
// verifies the group-ID adoption and REG2 broadcast, without a full Run().
func TestRegistrationHandshake(t *testing.T) {
	cfg := &Config{
		ReceiverHost:  "127.0.0.1",
		ReceiverPort:  1,
		UplinkIPs:     []string{"127.0.0.1"},
		Mode:          ModeClassic,
		ConnTimeoutMs: 4000,
	}
	s, err := New(cfg, slog.Default(), time.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u := s.uplinks[0]

	// The fake receiver socket the uplink sends into.
	recv := newLocalUDP(t)
	upSock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("uplink sock: %v", err)
	}
	u.SetConn(upSock)
	u.SetDest(udpAddr(recv.LocalAddr()))

	// 1. Receiver solicits registration with REG_NGP.
	ngp := []byte{0x92, 0x11}
	s.handleFeedback(u, ngp)
	if s.pendingReg2 == nil {
		t.Fatalf("after NGP: pendingReg2 not set")
	}

	// 2. The sender should have emitted REG1 carrying its group ID.
	reg1 := readDatagram(t, recv, 2*time.Second)
	if Type(reg1) != TypeREG1 {
		t.Fatalf("REG1 type = %#04x, want %#04x", Type(reg1), TypeREG1)
	}
	id1, ok := ParseREG1(reg1)
	if !ok {
		t.Fatalf("ParseREG1 failed")
	}
	if id1 != s.groupID {
		t.Fatalf("REG1 group id != sender group id")
	}

	// 3. Receiver confirms with REG2 (echoing the group ID).
	u.SetDest(udpAddr(recv.LocalAddr()))
	reg2 := BuildREG2(s.groupID)
	if _, err := recv.WriteToUDP(reg2, udpAddr(upSock.LocalAddr())); err != nil {
		t.Fatalf("send REG2: %v", err)
	}
	s.handleFeedback(u, reg2)
	if s.pendingReg2 != nil {
		t.Fatalf("after REG2: pendingReg2 should be cleared")
	}

	// 4. The sender broadcasts REG2 back to the receiver.
	bcast := readDatagram(t, recv, 2*time.Second)
	if Type(bcast) != TypeREG2 {
		t.Fatalf("broadcast type = %#04x, want %#04x", Type(bcast), TypeREG2)
	}
	if id, ok := ParseREG2(bcast); !ok || id != s.groupID {
		t.Fatalf("broadcast REG2 group id mismatch")
	}

	// 5. Receiver confirms the connection with REG3.
	reg3 := BuildREG3()
	if _, err := recv.WriteToUDP(reg3, udpAddr(upSock.LocalAddr())); err != nil {
		t.Fatalf("send REG3: %v", err)
	}
	s.handleFeedback(u, reg3)
	if !s.HasConnected() {
		t.Fatalf("after REG3: HasConnected = false")
	}
	if !u.Established() {
		t.Fatalf("after REG3: uplink not established")
	}
}

// TestREG2MismatchDropped verifies a REG2 with the wrong half-id is refused.
func TestREG2MismatchDropped(t *testing.T) {
	cfg := &Config{
		ReceiverHost:  "127.0.0.1",
		ReceiverPort:  1,
		UplinkIPs:     []string{"127.0.0.1"},
		Mode:          ModeClassic,
		ConnTimeoutMs: 4000,
	}

	s, _ := New(cfg, nil, time.Now)
	u := s.uplinks[0]
	recv := newLocalUDP(t)
	upSock, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	u.SetConn(upSock)
	u.SetDest(udpAddr(recv.LocalAddr()))

	s.handleFeedback(u, []byte{0x92, 0x11}) // NGP
	readDatagram(t, recv, 2*time.Second)    // consume REG1

	// Corrupt the group id's first byte and send a mismatching REG2.
	bad := s.groupID
	bad[0] ^= 0xFF
	if _, err := recv.WriteToUDP(BuildREG2(bad), udpAddr(upSock.LocalAddr())); err != nil {
		t.Fatalf("send bad REG2: %v", err)
	}
	s.handleFeedback(u, BuildREG2(bad))
	if s.pendingReg2 == nil {
		t.Fatalf("mismatch REG2 should not clear the pending reg2 connection")
	}
}

func TestSRTFeedbackForwardedWithoutSRTLAControl(t *testing.T) {
	s, err := New(testCfg(), nil, time.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u := s.uplinks[0]
	var received [][]byte
	s.SetSRTFeedback(func(pkt []byte) error {
		received = append(received, append([]byte(nil), pkt...))
		return nil
	})

	for _, typ := range []uint16{SRTTypeHandshake, SRTTypeACK, SRTTypeNAK, SRTTypeShutdown, 0x8006} {
		pkt := make([]byte, SRTMinLen+4)
		binary.BigEndian.PutUint16(pkt, typ)
		s.handleFeedback(u, pkt)
		if len(received) == 0 || !bytes.Equal(received[len(received)-1], pkt) {
			t.Fatalf("SRT control %#04x was not forwarded", typ)
		}
	}
	data := testDataPacket(7, 0, []byte("response"))
	s.handleFeedback(u, data)
	if !bytes.Equal(received[len(received)-1], data) {
		t.Fatal("SRT data response was not forwarded")
	}
	before := len(received)
	s.handleFeedback(u, BuildACK(7))
	s.handleFeedback(u, BuildKeepalive())
	if len(received) != before {
		t.Fatal("SRTLA control leaked to the local SRT endpoint")
	}
}

func TestSenderIgnoresFeedbackFromOtherAddresses(t *testing.T) {
	receiver := newLocalUDP(t)
	attacker := newLocalUDP(t)
	cfg := *testCfg()
	cfg.ReceiverPort = udpAddr(receiver.LocalAddr()).Port
	s, err := New(&cfg, nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan []byte, 2)
	s.SetSRTFeedback(func(pkt []byte) error {
		received <- bytes.Clone(pkt)
		return nil
	})
	if err := s.startNow(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dest := udpAddr(s.Uplinks()[0].Conn().LocalAddr())
	forged := make([]byte, SRTMinLen)
	binary.BigEndian.PutUint16(forged, SRTTypeHandshake)
	forged[len(forged)-1] = 1
	genuine := bytes.Clone(forged)
	genuine[len(genuine)-1] = 2
	if _, err := attacker.WriteToUDP(BuildREG3(), dest); err != nil {
		t.Fatal(err)
	}
	if _, err := attacker.WriteToUDP(forged, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.WriteToUDP(genuine, dest); err != nil {
		t.Fatal(err)
	}
	select {
	case pkt := <-received:
		if !bytes.Equal(pkt, genuine) {
			t.Fatalf("forwarded feedback from unexpected address: %x", pkt)
		}
	case <-time.After(time.Second):
		t.Fatal("configured receiver's feedback was not forwarded")
	}
	if s.HasConnected() {
		t.Fatal("accepted registration from an unexpected address")
	}
}

// dataPacket builds SRT data bytes with a sequence number (bit 31 clear).
// TestE2EDatapass runs the full Sender.Run() loop against a fake receiver
// and a fake local SRT client, and verifies a data packet makes the trip.
func TestE2EDatapass(t *testing.T) {
	// Fake receiver: completes registration, records data packets.
	recv := newLocalUDP(t)
	received := make(chan []byte, 8)
	go func() {
		buf := make([]byte, 1024)
		registered := false
		for {
			_ = recv.SetReadDeadline(time.Now().Add(3 * time.Second))
			n, src, err := recv.ReadFromUDP(buf)
			if err != nil {
				return
			}
			pkt := append([]byte(nil), buf[:n]...)
			switch Type(pkt) {
			case TypeREG1:
				id, _ := ParseREG1(pkt)
				_, _ = recv.WriteToUDP(BuildREG2(id), src)
			case TypeREG2:
				_, _ = recv.WriteToUDP(BuildREG3(), src)
				registered = true
			default:
				if registered {
					select {
					case received <- pkt:
					default:
					}
				}
			}
		}
	}()

	cfg := &Config{
		ReceiverHost:  "127.0.0.1",
		ReceiverPort:  udpAddr(recv.LocalAddr()).Port,
		UplinkIPs:     []string{"127.0.0.1"},
		Mode:          ModeClassic,
		ConnTimeoutMs: 4000,
	}
	norm := cfg.Normalized()
	s, err := New(&norm, slog.Default(), time.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Give the sockets a moment to bind.
	time.Sleep(150 * time.Millisecond)

	// Registration is receiver-initiated: nudge it by sending an NGP on the
	// uplink's source address, then let the fake receiver complete the
	// REG1 -> REG2 -> REG3 exchange.
	upAddr := udpAddr(s.uplinks[0].Conn().LocalAddr())
	if upAddr == nil {
		t.Fatal("uplink socket not connected")
	}
	_, _ = recv.WriteToUDP([]byte{0x92, 0x11}, upAddr)
	waitForConnected(t, s)

	// Data path: relay the SRT datagram unchanged to the uplink.
	payload := []byte("hello-srtla")
	packet := testDataPacket(1, 123, payload)
	if err := s.SendPacket(packet); err != nil {
		t.Fatalf("send packet: %v", err)
	}

	select {
	case got := <-received:
		if !bytes.Equal(got, packet) {
			t.Fatalf("received packet %x, want %x", got, packet)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatalf("no data packet reached the receiver")
	}
	cancel()
	<-done
}

// waitForConnected polls until at least one uplink is established.
func waitForConnected(t *testing.T, s *Sender) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.HasConnected() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("SRTLA uplink did not register")
}

// TestSenderReloadUplinks verifies a runtime SIGHUP-style uplink reload: the
// old sockets are closed, the new set is bound, and the registration state is
// reset so the housekeeping loop re-registers.
func TestSenderReloadUplinks(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	cfg := testCfg()
	cfg.ReceiverHost = "127.0.0.1"
	cfg.ReceiverPort = 1
	s, err := New(cfg, nil, fc.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.startNow(); err != nil {
		t.Fatalf("startNow: %v", err)
	}
	defer s.Close()

	if got := len(s.Uplinks()); got != 1 {
		t.Fatalf("uplinks before reload = %d, want 1", got)
	}
	old := s.Uplinks()[0]

	rcfg := testCfg()
	rcfg.ReceiverHost = "127.0.0.1"
	rcfg.ReceiverPort = 1
	rcfg.UplinkIPs = []string{"127.0.0.1", "127.0.0.1"}
	if err := s.Reload(*rcfg); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if got := len(s.Uplinks()); got != 2 {
		t.Fatalf("uplinks after reload = %d, want 2", got)
	}
	if old.Conn() != nil {
		t.Fatal("old uplink socket not closed after reload")
	}
	for _, u := range s.Uplinks() {
		if u.Conn() == nil {
			t.Fatal("reloaded uplink not connected")
		}
	}
	if s.HasConnected() {
		t.Fatal("registration state not reset after reload")
	}
}

func TestSenderReloadFailureKeepsExistingUplink(t *testing.T) {
	s, err := New(testCfg(), nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.startNow(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := s.Uplinks()[0]
	cfg := *testCfg()
	cfg.UplinkIPs = []string{"not-an-ip"}
	if err := s.Reload(cfg); err == nil {
		t.Fatal("reload accepted an invalid uplink")
	}
	if s.Uplinks()[0] != old || old.Conn() == nil {
		t.Fatal("failed reload replaced or closed the existing uplink")
	}
}

func TestSenderReloadUpdatesReceiver(t *testing.T) {
	first, second := newLocalUDP(t), newLocalUDP(t)
	cfg := *testCfg()
	cfg.ReceiverPort = udpAddr(first.LocalAddr()).Port
	s, err := New(&cfg, nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.startNow(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg.ReceiverPort = udpAddr(second.LocalAddr()).Port
	if err := s.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.Uplinks()[0].Send([]byte("new receiver")); err != nil {
		t.Fatal(err)
	}
	if got := readDatagram(t, second, time.Second); string(got) != "new receiver" {
		t.Fatalf("new receiver got %q", got)
	}
}

func TestSenderCloseDuringHousekeepingDoesNotReopenUplink(t *testing.T) {
	s, err := New(testCfg(), nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.startNow(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u := s.Uplinks()[0]
	u.Close()
	started := make(chan struct{})
	resume := make(chan struct{})
	s.now = func() time.Time {
		close(started)
		<-resume
		return time.Now()
	}
	done := make(chan struct{})
	go func() {
		s.tick()
		close(done)
	}()
	<-started
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	<-s.stop
	close(resume)
	<-done
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if u.Conn() != nil {
		t.Fatal("housekeeping reopened an uplink after shutdown")
	}
}

func TestSenderCloseStopsRun(t *testing.T) {
	s, err := New(testCfg(), nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for s.Uplinks()[0].Conn() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Uplinks()[0].Conn() == nil {
		t.Fatal("sender did not start")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after Close")
	}
}

func TestSenderFatalFailoverClosesUplinks(t *testing.T) {
	now := time.Now()
	s, err := New(testCfg(), nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.startNow(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.mu.Lock()
	s.allFailedAt = now.Add(-FailoverAfter - time.Second)
	s.mu.Unlock()
	s.tick()
	if !s.Failover() {
		t.Fatal("sender did not report exhausted receiver addresses")
	}
	if s.Uplinks()[0].Conn() != nil {
		t.Fatal("fatal failover left an uplink open")
	}
	select {
	case <-s.done:
	default:
		t.Fatal("fatal failover did not stop Run")
	}
}
