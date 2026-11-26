package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"ctrlirl-coder/internal/compositor"
	"ctrlirl-coder/internal/config"
	"ctrlirl-coder/internal/sinks/srtla"
	"github.com/haivision/srtgo"
)

// TestSRTLARealSRTSession exercises the same sink wiring as the application:
// encoded bytes must cross an SRTLA relay and arrive through a real SRT socket.
func TestSRTLARealSRTSession(t *testing.T) {
	srtgo.InitSRT()
	srtgo.SrtSetLogLevel(srtgo.SrtLogLevelCrit)
	t.Cleanup(func() { srtgo.SrtSetLogLevel(srtgo.SrtLogLevelErr) })

	for _, tc := range []struct {
		name    string
		uplinks []string
	}{
		{name: "single uplink", uplinks: []string{"127.0.0.1"}},
		{name: "bonded uplinks", uplinks: []string{"127.0.0.1", "127.0.0.2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.uplinks) > 1 {
				conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(tc.uplinks[1])})
				if err != nil {
					t.Skipf("second loopback address unavailable: %v", err)
				}
				conn.Close()
			}
			testSRTLARealSRTSession(t, tc.uplinks, false, false)
		})
	}
	t.Run("reference receiver", func(t *testing.T) {
		testSRTLARealSRTSession(t, []string{"127.0.0.1"}, true, false)
	})
	t.Run("encrypted session and stream ID", func(t *testing.T) {
		testSRTLARealSRTSession(t, []string{"127.0.0.1"}, false, true)
	})
}

func testSRTLARealSRTSession(t *testing.T, uplinks []string, reference, authenticated bool) {
	t.Helper()
	const streamID = "srtla-test-stream"
	const passphrase = "srtla-test-passphrase"
	port := availableUDPPort(t)
	opts := map[string]string{
		"mode": "listener", "transtype": "live", "latency": "120",
	}
	if authenticated {
		opts["passphrase"] = passphrase
	}
	listener := srtgo.NewSrtSocket("127.0.0.1", uint16(port), opts)
	if listener == nil {
		t.Fatal("create SRT listener")
	}
	defer listener.Close()
	ids := make(chan string, 1)
	if authenticated {
		listener.SetListenCallback(func(_ *srtgo.SrtSocket, _ int, _ *net.UDPAddr, id string) bool {
			select {
			case ids <- id:
			default:
			}
			return id == streamID
		})
	}
	if err := listener.Listen(1); err != nil {
		t.Fatalf("listen for SRT: %v", err)
	}
	listener.SetPollTimeout(6 * time.Second)

	var receiverPort int
	var receiverErrors <-chan error
	if reference {
		receiverPort, receiverErrors = newReferenceSRTLAReceiver(t, port)
	} else {
		receiver := newTestSRTLAReceiver(t, port)
		receiverPort, receiverErrors = receiver.port(), receiver.errs
	}
	cp, err := compositor.New("fakesrc ! identity name=venc_kbps ! appsink name=appsink")
	if err != nil {
		t.Fatalf("create pipeline: %v", err)
	}
	defer cp.Stop()

	cfg := &config.Config{
		Sink: config.SinkSRTLA,
		SRT:  config.SRTParams{LatencyMs: 120},
		SRTLA: config.SRTLAParams{
			ReceiverHost:  "127.0.0.1",
			ReceiverPort:  receiverPort,
			UplinkIPs:     uplinks,
			ConnTimeoutMs: 5000,
		},
	}
	if authenticated {
		cfg.SRT.StreamID = streamID
		cfg.SRT.Passphrase = passphrase
	}
	sink, puller, start, err := startDataPath(cp, cfg)
	if err != nil {
		t.Fatalf("start data path: %v", err)
	}
	if puller == nil {
		t.Fatal("appsink puller not attached")
	}
	defer puller.Detach()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		start(ctx)
	}()
	defer func() {
		cancel()
		if err := sink.Close(); err != nil {
			t.Errorf("close sink: %v", err)
		}
		<-done
	}()

	type readResult struct {
		payload []byte
		err     error
	}
	payload := []byte("SRTLA must deliver payload over a real SRT session")
	afterReload := []byte("SRTLA must preserve the SRT session after uplink reload")
	result := make(chan readResult, 16)
	go func() {
		peer, _, err := listener.Accept()
		if err != nil {
			result <- readResult{err: fmt.Errorf("SRT accept: %w", err)}
			return
		}
		defer peer.Close()
		peer.SetPollTimeout(3 * time.Second)
		buf := make([]byte, 1316)
		for {
			n, err := peer.Read(buf)
			if err != nil {
				select {
				case result <- readResult{err: fmt.Errorf("SRT read: %w", err)}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case result <- readResult{payload: bytes.Clone(buf[:n])}:
			case <-ctx.Done():
				return
			}
		}
	}()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(12 * time.Second)
	defer timeout.Stop()
	var lastWriteErr error
	reloaded := false
	for {
		select {
		case <-ticker.C:
			_, lastWriteErr = sink.Write(payload)
		case received := <-result:
			if received.err != nil {
				t.Fatalf("real SRT session did not deliver payload: %v (last write: %v)", received.err, lastWriteErr)
			}
			if !bytes.Equal(received.payload, payload) {
				continue
			}
			if authenticated {
				select {
				case id := <-ids:
					if id != streamID {
						t.Fatalf("SRT stream ID = %q, want %q", id, streamID)
					}
				default:
					t.Fatal("SRT listener did not receive a stream ID")
				}
			}
			relay, ok := sink.(*srtlaSink)
			if !ok {
				t.Fatal("data path did not create an SRTLA sink")
			}
			if reloaded {
				return
			}
			deadline := time.Now().Add(2 * time.Second)
			for relay.sender.ActiveConnections() < len(uplinks) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if got := relay.sender.ActiveConnections(); got < len(uplinks) {
				t.Fatalf("registered uplinks = %d, want %d", got, len(uplinks))
			}
			if len(uplinks) > 1 {
				next := cfg.SRTLA.ToSRTLAConfig()
				next.UplinkIPs = []string{"not-an-ip"}
				if err := relay.Reload(next); err == nil {
					t.Fatal("reload accepted an invalid uplink")
				}
				if got := relay.sender.ActiveConnections(); got < len(uplinks) {
					t.Fatalf("failed reload dropped working uplinks: %d", got)
				}
				next.UplinkIPs = []string{uplinks[1]}
				if err := relay.Reload(next); err != nil {
					t.Fatalf("reload bonded uplinks: %v", err)
				}
				payload = afterReload
				reloaded = true
				continue
			}
			return
		case err := <-receiverErrors:
			t.Fatalf("SRTLA test receiver: %v", err)
		case <-timeout.C:
			t.Fatalf("timed out waiting for real SRT payload (last write: %v)", lastWriteErr)
		}
	}
}

func TestSRTLARetransmitsLostPacket(t *testing.T) {
	srtgo.InitSRT()
	srtgo.SrtSetLogLevel(srtgo.SrtLogLevelCrit)
	t.Cleanup(func() { srtgo.SrtSetLogLevel(srtgo.SrtLogLevelErr) })
	port := availableUDPPort(t)
	listener := srtgo.NewSrtSocket("127.0.0.1", uint16(port), map[string]string{
		"mode": "listener", "transtype": "live", "latency": "500",
	})
	if listener == nil {
		t.Fatal("create SRT listener")
	}
	defer listener.Close()
	if err := listener.Listen(1); err != nil {
		t.Fatalf("listen for SRT: %v", err)
	}
	listener.SetPollTimeout(6 * time.Second)
	receiver := newTestSRTLAReceiver(t, port)
	cfg := &config.Config{
		SRT: config.SRTParams{LatencyMs: 500},
		SRTLA: config.SRTLAParams{
			ReceiverHost:  "127.0.0.1",
			ReceiverPort:  receiver.port(),
			UplinkIPs:     []string{"127.0.0.1"},
			ConnTimeoutMs: 5000,
		},
	}
	sink, err := newSRTLASink(cfg, slog.Default())
	if err != nil {
		t.Fatalf("create SRTLA sink: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sink.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("stop SRTLA sink: %v", err)
		}
	}()

	peer, _, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept SRT session: %v", err)
	}
	defer peer.Close()
	peer.SetPollTimeout(4 * time.Second)
	deadline := time.Now().Add(time.Second)
	for !sink.client.Connected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !sink.client.Connected() {
		t.Fatal("SRT sender did not finish connecting")
	}
	initial := []byte("initial message")
	if _, err := sink.Write(initial); err != nil {
		t.Fatalf("send initial message: %v", err)
	}
	buf := make([]byte, 1316)
	n, err := peer.Read(buf)
	if err != nil || !bytes.Equal(buf[:n], initial) {
		t.Fatalf("initial SRT read: payload %q, error %v", buf[:n], err)
	}

	receiver.dropNextData.Store(true)
	lost := []byte("message that must be retransmitted")
	if _, err := sink.Write(lost); err != nil {
		t.Fatalf("send message to drop: %v", err)
	}
	if _, err := sink.Write([]byte("next message")); err != nil {
		t.Fatalf("send following message: %v", err)
	}
	n, err = peer.Read(buf)
	if err != nil || !bytes.Equal(buf[:n], lost) {
		t.Fatalf("retransmitted SRT read: payload %q, error %v", buf[:n], err)
	}
	if !receiver.retransmitted.Load() {
		t.Fatal("receiver did not observe the dropped SRT sequence retransmitted")
	}
}

func newReferenceSRTLAReceiver(t *testing.T, srtPort int) (int, <-chan error) {
	t.Helper()
	bin := os.Getenv("SRTLA_REC_BIN")
	if bin == "" {
		var err error
		bin, err = exec.LookPath("srtla_rec")
		if err != nil {
			t.Skip("srtla_rec not on PATH (set SRTLA_REC_BIN to run)")
		}
	}
	port := availableUDPPort(t)
	var log bytes.Buffer
	cmd := exec.Command(bin,
		"--srtla_port", fmt.Sprint(port),
		"--srt_hostname", "127.0.0.1",
		"--srt_port", fmt.Sprint(srtPort),
		"--log_level", "info",
	)
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start srtla_rec: %v", err)
	}
	errs := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		err := cmd.Wait()
		errs <- fmt.Errorf("srtla_rec exited: %v\n%s", err, log.String())
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errs:
			t.Fatal(err)
		default:
		}
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if errors.Is(err, syscall.EADDRINUSE) {
			return port, errs
		}
		if err != nil {
			t.Fatalf("check srtla_rec listener: %v", err)
		}
		conn.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("srtla_rec did not open its UDP listener")
	return 0, nil
}

func availableUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("reserve UDP port: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	if err := conn.Close(); err != nil {
		t.Fatalf("release UDP port: %v", err)
	}
	return port
}

type testSRTLAReceiver struct {
	control       *net.UDPConn
	srt           *net.UDPConn
	peer          atomic.Pointer[net.UDPAddr]
	dropNextData  atomic.Bool
	droppedSeq    atomic.Int64
	retransmitted atomic.Bool
	errs          chan error
	wg            sync.WaitGroup
}

func newTestSRTLAReceiver(t *testing.T, srtPort int) *testSRTLAReceiver {
	t.Helper()
	control, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen for SRTLA: %v", err)
	}
	srt, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: srtPort})
	if err != nil {
		control.Close()
		t.Fatalf("connect receiver to SRT listener: %v", err)
	}
	r := &testSRTLAReceiver{control: control, srt: srt, errs: make(chan error, 2)}
	r.wg.Add(2)
	go r.receiveSRTLA()
	go r.receiveSRT()
	t.Cleanup(func() {
		control.Close()
		srt.Close()
		r.wg.Wait()
	})
	return r
}

func (r *testSRTLAReceiver) port() int {
	return r.control.LocalAddr().(*net.UDPAddr).Port
}

func (r *testSRTLAReceiver) report(err error) {
	if err != nil && !errors.Is(err, net.ErrClosed) {
		r.errs <- err
	}
}

func (r *testSRTLAReceiver) receiveSRTLA() {
	defer r.wg.Done()
	var group [srtla.IDLen]byte
	haveGroup := false
	registered := make(map[string]bool)
	buf := make([]byte, 4096)
	for {
		n, src, err := r.control.ReadFromUDP(buf)
		if err != nil {
			r.report(err)
			return
		}
		pkt := buf[:n]
		switch srtla.Type(pkt) {
		case srtla.TypeREG1:
			id, ok := srtla.ParseREG1(pkt)
			if !ok {
				r.report(errors.New("invalid REG1"))
				return
			}
			group, haveGroup = id, true
			_, err = r.control.WriteToUDP(srtla.BuildREG2(group), src)
		case srtla.TypeREG2:
			id, ok := srtla.ParseREG2(pkt)
			if !ok {
				r.report(errors.New("invalid REG2"))
				return
			}
			if !haveGroup || id != group {
				_, err = r.control.WriteToUDP([]byte{0x92, 0x11}, src)
			} else {
				registered[src.String()] = true
				_, err = r.control.WriteToUDP(srtla.BuildREG3(), src)
			}
		case srtla.TypeKeepalive:
			_, err = r.control.WriteToUDP(srtla.BuildKeepalive(), src)
		default:
			if !registered[src.String()] {
				continue
			}
			r.peer.Store(src)
			if seq := srtla.SeqNumber(pkt); seq >= 0 {
				if r.dropNextData.CompareAndSwap(true, false) {
					r.droppedSeq.Store(int64(seq) + 1)
					continue
				}
				if r.droppedSeq.Load() == int64(seq)+1 {
					r.retransmitted.Store(true)
				}
				if _, err = r.control.WriteToUDP(srtla.BuildACK(seq), src); err != nil {
					break
				}
			}
			_, err = r.srt.Write(pkt)
		}
		if err != nil {
			r.report(err)
			return
		}
	}
}

func (r *testSRTLAReceiver) receiveSRT() {
	defer r.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, err := r.srt.Read(buf)
		if err != nil {
			r.report(err)
			return
		}
		if peer := r.peer.Load(); peer != nil {
			if _, err := r.control.WriteToUDP(buf[:n], peer); err != nil {
				r.report(err)
				return
			}
		}
	}
}
