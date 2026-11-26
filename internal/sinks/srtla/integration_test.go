// Integration test against the real srtla_rec binary (skips when absent).
package srtla

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestIntegrationAgainstSRTLARec checks that the real srtla_rec forwards
// SRT data datagrams unchanged after SRTLA registration.
//
// Topology:
//
//	our Sender --SRTLA(UDP)--> srtla_rec --raw UDP (verbatim SRT datagrams)--> this test
//
// A raw UDP listener validates the fixture header and reassembles its payload.
// This only tests SRTLA forwarding; a real SRT session needs its own handshake
// and is covered by the application-level SRTLA integration test.
//
// The test skips itself when the srtla_rec binary is not on PATH (or
// SRTLA_REC_BIN).
func TestIntegrationAgainstSRTLARec(t *testing.T) {
	bin := os.Getenv("SRTLA_REC_BIN")
	if bin == "" {
		b, err := exec.LookPath("srtla_rec")
		if err != nil {
			t.Skip("srtla_rec not on PATH (set SRTLA_REC_BIN to run) — skipping")
		}
		bin = b
	}

	srtlaPort, err := freeUDPPort()
	if err != nil {
		t.Fatalf("srtla port: %v", err)
	}
	downstreamPort, err := freeUDPPort()
	if err != nil {
		t.Fatalf("downstream port: %v", err)
	}

	// Downstream endpoint: raw UDP listener receiving the rec's verbatim SRT
	// datagrams. Validates each fixture header, then reassembles payloads in
	// arrival order (single uplink on loopback => seq-ordered).
	downAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: downstreamPort}
	downConn, err := net.ListenUDP("udp4", downAddr)
	if err != nil {
		t.Fatalf("downstream listener: %v", err)
	}
	defer downConn.Close()

	var (
		mu        sync.Mutex
		acc       []byte // reassembled payload
		badHeader string // first header validation failure, empty when all good
	)
	go func() {
		buf := make([]byte, 2048)
		for {
			downConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			n, _, err := downConn.ReadFromUDP(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return // listener closed
			}
			if n < SRTMinLen {
				mu.Lock()
				if badHeader == "" {
					badHeader = fmt.Sprintf("datagram too short (%d bytes)", n)
				}
				mu.Unlock()
				continue
			}
			// Bit 31 of the first word marks SRT/SRTLA control datagrams
			// (handshake IND, acks, NAKs, SRTLA keepalive). Skip them: only
			// data datagrams (bit 31 clear) carry a payload.
			if binary.BigEndian.Uint32(buf[0:4])&0x80000000 != 0 {
				continue
			}
			// SRT data header: [0:4) seq, [4:8) msgno, [8:12) ts, [12:16) id
			seq := binary.BigEndian.Uint32(buf[0:4])
			msgNo := binary.BigEndian.Uint32(buf[4:8])
			ts := binary.BigEndian.Uint32(buf[8:12])
			id := binary.BigEndian.Uint32(buf[12:16])
			if err := checkSRTHdr(seq, msgNo, ts, id); err != nil {
				mu.Lock()
				if badHeader == "" {
					badHeader = err.Error()
				}
				mu.Unlock()
				continue
			}
			mu.Lock()
			acc = append(acc, buf[SRTMinLen:n]...)
			mu.Unlock()
		}
	}()

	// Spawn the real receiver.
	var recLog bytes.Buffer
	cmd := exec.Command(bin,
		"--srtla_port", fmt.Sprint(srtlaPort),
		"--srt_hostname", "127.0.0.1",
		"--srt_port", fmt.Sprint(downstreamPort),
		"--log_level", "debug",
	)
	cmd.Stdout = &recLog
	cmd.Stderr = &recLog
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn srtla_rec: %v", err)
	}
	defer func() { _ = commandShutdown(cmd) }()

	// Give the rec a moment to bind its SRTLA socket.
	time.Sleep(500 * time.Millisecond)

	// Run our sender against the rec's SRTLA port.
	cfg := &Config{
		ReceiverHost: "127.0.0.1",
		ReceiverPort: srtlaPort,
		UplinkIPs:    []string{"127.0.0.1"},
		Mode:         ModeEnhanced,
	}
	snd, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = snd.Run(ctx) }()

	// Payload easy to spot after crossing the wire and SRT reassembly.
	payload := make([]byte, 600)
	for i := range payload {
		payload[i] = byte('A' + (i % 13))
	}
	magic := "CTRLIRL-SRTLA-E2E"
	copy(payload, magic)

	// Write the payload a handful of times; the rec forwards each SRT
	// datagram verbatim to the downstream UDP listener.
	got := false
	var connected bool
	var seq int32
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !got {
		seq++
		pkt := testDataPacket(seq, uint32(time.Now().UnixMilli()&0x7FFFFFFF), payload)
		if err := snd.SendPacket(pkt); err != nil && err != ErrNoUplink {
			t.Fatalf("SendPacket: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		got = bytes.Contains(acc, []byte(magic))
		badHeader := badHeader
		mu.Unlock()
		if badHeader != "" {
			_ = commandShutdown(cmd)
			t.Fatalf("SRT data header rejected by downstream: %s\nrec log:\n%s",
				badHeader, tailLines(recLog.String(), 40))
		}
	}

	// Stop the rec before inspecting its log (drains the writer goroutine so
	// reading recLog is race-free).
	_ = commandShutdown(cmd)
	connected = snd.HasConnected()
	logText := tailLines(recLog.String(), 40)
	mu.Lock()
	reassembled := len(acc)
	mu.Unlock()

	if !got {
		t.Fatalf("payload not reassembled downstream within %v (got %d payload bytes)\nrec log:\n%s",
			15*time.Second, reassembled, logText)
	}
	t.Logf("rec forwarded our SRT datagrams verbatim; downstream reassembled %d bytes incl. magic (sender connected=%v)", reassembled, connected)
	t.Logf("rec log tail:\n%s", logText)
}

// checkSRTHdr validates the test fixture's 16-byte SRT data header:
// [0:4) seq (bit 31 clear = data), [4:8) msgno (solo in-order), [8:12)
// timestamp (ms, nonzero after a running send), [12:16) socket id (0).
func checkSRTHdr(seq, msgNo, ts, id uint32) error {
	if seq&0x80000000 != 0 {
		return fmt.Errorf("seq word 0x%08x has control-packet bit 31 set (seq=%d)", seq, seq)
	}
	if msgNo != msgNoSoloInorder {
		return fmt.Errorf("msgno 0x%08x, want 0x%08x (solo in-order, per-message seq 0)", msgNo, msgNoSoloInorder)
	}
	if id != 0 {
		return fmt.Errorf("socket id 0x%08x, want 0", id)
	}
	// SRT timestamps are epoch ms masked to 31 bits; compare in that space.
	nowMasked := uint32(time.Now().UnixMilli() & 0x7FFFFFFF)
	drift := int32(nowMasked - ts)
	if drift < -60000 || drift > 120000 {
		return fmt.Errorf("timestamp 0x%x is not within [now-60s, now+120s] (now=0x%x)", ts, nowMasked)
	}
	return nil
}

// freeUDPPort reserves and releases a UDP port on 127.0.0.1, returning its
// number. A brief post-release window is tolerated by the caller.
func freeUDPPort() (int, error) {
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return 0, err
	}
	port := ln.LocalAddr().(*net.UDPAddr).Port
	_ = ln.Close()
	return port, nil
}

// commandShutdown terminates the rec and waits for its writer goroutines to
// drain so subsequent reads of its captured output are race-free.
func commandShutdown(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Kill()
	return cmd.Wait()
}

// tailLines returns the last n lines of s (for failure diagnostics).
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) < n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
