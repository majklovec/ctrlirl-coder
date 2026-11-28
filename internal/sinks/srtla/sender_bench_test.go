package srtla

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// benchSender is a sender with n established loopback uplinks pointed at a
// bound (never read) UDP socket: the kernel accepts every datagram, so the
// benchmark sees the real per-packet syscall without a peer goroutine.
func benchSender(b *testing.B, mode string, n int) *Sender {
	b.Helper()
	ips := make([]string, n)
	for i := range ips {
		ips[i] = "127.0.0.1"
	}
	s, err := New(&Config{
		ReceiverHost:  "127.0.0.1",
		ReceiverPort:  1,
		UplinkIPs:     ips,
		Mode:          mode,
		ConnTimeoutMs: 4000,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
	if err != nil {
		b.Fatal(err)
	}
	recv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { recv.Close() })
	for _, u := range s.uplinks {
		if err := u.Connect(); err != nil {
			b.Fatal(err)
		}
		u.SetDest(recv.LocalAddr().(*net.UDPAddr))
		u.RegDone()
		u.TouchRecv()
	}
	b.Cleanup(func() { s.Close() })
	s.SetSRTFeedback(func([]byte) error { return nil })
	return s
}

// srtDataPacket is a full-size SRT live data packet carrying seq.
func srtDataPacket(seq int32) []byte {
	pkt := make([]byte, SRTMinLen+SRTMaxPayload)
	binary.BigEndian.PutUint32(pkt, uint32(seq))
	return pkt
}

// srtAckPacket is an SRT cumulative ACK up to (excluding) seq.
func srtAckPacket(seq int32) []byte {
	pkt := make([]byte, SRTMinLen+4)
	binary.BigEndian.PutUint16(pkt, SRTTypeACK)
	binary.BigEndian.PutUint32(pkt[SRTMinLen:], uint32(seq))
	return pkt
}

// BenchmarkSendPacket benchmarks the per-packet relay path with two bonded
// uplinks: scheduler select, in-flight bookkeeping and the UDP send. A
// cumulative SRT ACK every 64 packets keeps the in-flight windows in the
// range a healthy link runs at.
func BenchmarkSendPacket(b *testing.B) {
	for _, mode := range []string{ModeClassic, ModeEnhanced} {
		b.Run(mode, func(b *testing.B) {
			s := benchSender(b, mode, 2)
			u := s.uplinks[0]
			pkt := srtDataPacket(0)
			ack := srtAckPacket(0)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				seq := int32(i & 0x3fffffff)
				binary.BigEndian.PutUint32(pkt, uint32(seq))
				if err := s.SendPacket(pkt); err != nil {
					b.Fatal(err)
				}
				if i%64 == 63 {
					binary.BigEndian.PutUint32(ack[SRTMinLen:], uint32(seq))
					s.handleFeedback(u, ack)
				}
			}
		})
	}
}

// BenchmarkSchedulerSelect isolates the uplink choice from the UDP syscall
// (which dominates BenchmarkSendPacket).
func BenchmarkSchedulerSelect(b *testing.B) {
	for _, mode := range []string{ModeClassic, ModeEnhanced} {
		b.Run(mode, func(b *testing.B) {
			s := benchSender(b, mode, 2)
			ups, sc := s.snap()
			for i := int32(1); i <= 8; i++ {
				ups[0].NoteSent(i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if sc.Select(ups, s.now()) < 0 {
					b.Fatal("no uplink selected")
				}
			}
		})
	}
}

// BenchmarkHandleFeedbackACK benchmarks one SRTLA ACK packet (the reference
// receiver acknowledges data packets in groups of ten) through the feedback
// path: ring lookup, window growth and stall bookkeeping per sequence.
func BenchmarkHandleFeedbackACK(b *testing.B) {
	s := benchSender(b, ModeEnhanced, 2)
	u := s.uplinks[0]
	ack := make([]byte, 4+4*10)
	binary.BigEndian.PutUint32(ack, uint32(TypeACK)<<16)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		base := int32(i&0x00ffffff)*10 + 1
		for j := int32(0); j < 10; j++ {
			u.NoteSent(base + j)
			binary.BigEndian.PutUint32(ack[4+4*j:], uint32(base+j))
		}
		s.handleFeedback(u, ack)
	}
}
