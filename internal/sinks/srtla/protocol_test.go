package srtla

import (
	"bytes"
	"encoding/binary"
	"testing"
)

const msgNoSoloInorder = 0xE0000000

func testDataPacket(seq int32, tsMs uint32, payload []byte) []byte {
	pkt := make([]byte, SRTMinLen+len(payload))
	binary.BigEndian.PutUint32(pkt[0:4], uint32(seq)&0x7FFFFFFF)
	binary.BigEndian.PutUint32(pkt[4:8], msgNoSoloInorder)
	binary.BigEndian.PutUint32(pkt[8:12], tsMs)
	copy(pkt[SRTMinLen:], payload)
	return pkt
}

func sameInts(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestType(t *testing.T) {
	if got := Type(nil); got != 0 {
		t.Fatalf("Type(nil) = %d, want 0", got)
	}
	if got := Type([]byte{0x92}); got != 0 {
		t.Fatalf("Type(short) = %d, want 0", got)
	}
	if got := Type(BuildREG1([IDLen]byte{})); got != TypeREG1 {
		t.Fatalf("Type(REG1) = %#04x, want %#04x", got, TypeREG1)
	}
}

func TestSeqNumber(t *testing.T) {
	// Data packet: first 4 bytes big-endian, bit 31 clear.
	data := make([]byte, 28)
	binary.BigEndian.PutUint32(data, 0x0000ABCD)
	if got := SeqNumber(data); got != 0xABCD {
		t.Fatalf("SeqNumber(data) = %d, want 0xABCD", got)
	}
	// Control packet: high bit set in the type word -> -1.
	ctrl := make([]byte, 16)
	binary.BigEndian.PutUint16(ctrl, TypeKeepalive)
	if got := SeqNumber(ctrl); got != -1 {
		t.Fatalf("SeqNumber(control) = %d, want -1", got)
	}
	// Too short.
	if got := SeqNumber([]byte{0x00, 0x01}); got != -1 {
		t.Fatalf("SeqNumber(short) = %d, want -1", got)
	}
}

func TestREGRoundtrip(t *testing.T) {
	id := RandomGroupID()
	if id[128] != 0 || bytes.Count(id[:128], []byte{0}) == 128 {
		t.Fatalf("RandomGroupID should fill the first 128 bytes, got tail=%v", id[128:])
	}
	if !bytes.Equal(id[128:], make([]byte, 128)) {
		t.Fatalf("RandomGroupID must zero the second half, got %x", id[128:])
	}

	// REG1
	p := BuildREG1(id)
	if len(p) != REGLen {
		t.Fatalf("REG1 len = %d, want %d", len(p), REGLen)
	}
	if want := []byte{0x92, 0x00}; !bytes.Equal(p[:2], want) {
		t.Fatalf("REG1 type bytes = %x, want %x", p[:2], want)
	}
	gotID, ok := ParseREG1(p)
	if !ok || gotID != id {
		t.Fatalf("REG1 roundtrip = %v %v, want %v", gotID, ok, id)
	}
	if _, ok := ParseREG1(BuildREG2(id)); ok {
		t.Fatal("ParseREG1 accepted a REG2")
	}
	if _, ok := ParseREG1(p[:REGLen-1]); ok {
		t.Fatal("ParseREG1 accepted a truncated packet")
	}

	// REG2
	p = BuildREG2(id)
	if want := []byte{0x92, 0x01}; !bytes.Equal(p[:2], want) {
		t.Fatalf("REG2 type bytes = %x, want %x", p[:2], want)
	}
	gotID, ok = ParseREG2(p)
	if !ok || gotID != id {
		t.Fatalf("REG2 roundtrip = %v %v, want %v", gotID, ok, id)
	}

	// REG3 is type-only.
	p = BuildREG3()
	if len(p) != REG3Len || !bytes.Equal(p, []byte{0x92, 0x02}) {
		t.Fatalf("REG3 = %x, want 9202", p)
	}
}

func TestKeepaliveLegacy(t *testing.T) {
	p := BuildKeepalive()
	if !bytes.Equal(p, []byte{0x90, 0x00}) {
		t.Fatalf("legacy keepalive = %x, want 9000", p)
	}
	if _, ok := ParseExtendedKeepalive(p); ok {
		t.Fatal("legacy keepalive parsed as extended")
	}
}

func TestExtendedKeepaliveRoundtrip(t *testing.T) {
	info := ConnInfo{
		ConnID:     0xDEADBEEF,
		Window:     -42,
		InFlight:   7,
		RTTMs:      133,
		NAKCount:   99,
		BitrateBPS: 62_500_000,
	}
	p := BuildExtendedKeepalive(info)
	if len(p) != KeepaliveExtLen {
		t.Fatalf("extended keepalive len = %d, want %d", len(p), KeepaliveExtLen)
	}
	// Pin the wire layout: reserved tail, magic, version, fields.
	if got := binary.BigEndian.Uint16(p[10:]); got != KeepaliveMagic {
		t.Fatalf("magic at [10:12] = %#04x, want %#04x", got, KeepaliveMagic)
	}
	if got := binary.BigEndian.Uint16(p[12:]); got != KeepaliveExtVer {
		t.Fatalf("version at [12:14] = %#04x, want %#04x", got, KeepaliveExtVer)
	}
	got, ok := ParseExtendedKeepalive(p)
	if !ok || got != info {
		t.Fatalf("extended keepalive roundtrip = %+v %v, want %+v", got, ok, info)
	}

	// Wrong magic must be rejected.
	p[10] = 0x00
	if _, ok := ParseExtendedKeepalive(p); ok {
		t.Fatal("bad magic accepted")
	}

	// Truncated packet must be rejected.
	p = BuildExtendedKeepalive(info)
	if _, ok := ParseExtendedKeepalive(p[:20]); ok {
		t.Fatal("truncated extended keepalive accepted")
	}
}

func TestACKRoundtrip(t *testing.T) {
	p := BuildACK(1, 2, 3)
	if len(p) != 16 {
		t.Fatalf("ACK len = %d, want 16", len(p))
	}
	// Header word is the type shifted left 16: 0x9100 0x0000.
	if want := []byte{0x91, 0x00, 0x00, 0x00}; !bytes.Equal(p[:4], want) {
		t.Fatalf("ACK header = %x, want %x", p[:4], want)
	}
	if got := ParseACKs(p); !sameInts(got, []int32{1, 2, 3}) {
		t.Fatalf("ACK roundtrip = %v, want [1 2 3]", got)
	}

	if got := ParseACKs(BuildACK()); len(got) != 0 {
		t.Fatalf("empty ACK parsed as %v", got)
	}
	if got := ParseACKs(BuildKeepalive()); got != nil {
		t.Fatalf("keepalive parsed as ACK: %v", got)
	}
}

func TestSRTAckLast(t *testing.T) {
	// 16-byte SRT header (type 0x8002) + last-ack at offset 16.
	p := make([]byte, SRTMinLen+4)
	binary.BigEndian.PutUint16(p, SRTTypeACK)
	binary.BigEndian.PutUint32(p[SRTMinLen:], 0x12345678)
	got, ok := SRTAckLast(p)
	if !ok || got != 0x12345678 {
		t.Fatalf("SRTAckLast = %d %v, want 0x12345678", got, ok)
	}
	if _, ok := SRTAckLast(p[:SRTMinLen]); ok {
		t.Fatal("truncated SRT ACK accepted")
	}
	if _, ok := SRTAckLast(BuildKeepalive()); ok {
		t.Fatal("keepalive parsed as SRT ACK")
	}
}

func TestSRTNAKLosses(t *testing.T) {
	// Layout per reference: 16-byte SRT header (type 0x8003), then a
	// list-count word, then loss list words. The reference reads every
	// word after the header as an id, so the count is treated as a loss
	// id too; the packet below encodes count=2 (a harmless phantom) and
	// the real single loss 100.
	p := make([]byte, SRTMinLen+4+4)
	binary.BigEndian.PutUint16(p, SRTTypeNAK)
	binary.BigEndian.PutUint32(p[SRTMinLen:], 2) // count word
	binary.BigEndian.PutUint32(p[SRTMinLen+4:], 100)
	got := SRTNAKLosses(p)
	want := []int32{2, 100}
	if len(got) != len(want) {
		t.Fatalf("NAK losses = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NAK losses = %v, want %v", got, want)
		}
	}

	// Range entry: 0x80000000 | first, followed by last. Loss 10..11.
	p = make([]byte, SRTMinLen+4+8)
	binary.BigEndian.PutUint16(p, SRTTypeNAK)
	binary.BigEndian.PutUint32(p[SRTMinLen:], 1) // count word (phantom)
	binary.BigEndian.PutUint32(p[SRTMinLen+4:], 0x80000000|10)
	binary.BigEndian.PutUint32(p[SRTMinLen+8:], 11)
	got = SRTNAKLosses(p)
	want = []int32{1, 10, 11}
	if len(got) != len(want) {
		t.Fatalf("NAK range losses = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NAK range losses = %v, want %v", got, want)
		}
	}

	if got := SRTNAKLosses(BuildKeepalive()); got != nil {
		t.Fatalf("keepalive parsed as SRT NAK: %v", got)
	}
}

func TestSRTNAKLossesBoundsHugeRanges(t *testing.T) {
	p := make([]byte, SRTMinLen+8)
	binary.BigEndian.PutUint16(p, SRTTypeNAK)
	binary.BigEndian.PutUint32(p[SRTMinLen:], 0x80000001)
	binary.BigEndian.PutUint32(p[SRTMinLen+4:], 0x7fffffff)
	got := SRTNAKLosses(p)
	if len(got) != MaxNAKEpoch || got[0] != 1 || got[len(got)-1] != MaxNAKEpoch {
		t.Fatalf("bounded NAK range: length=%d", len(got))
	}
}
