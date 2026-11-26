package testcode

import (
	"fmt"
	"strings"
	"testing"
)

// bitReader reads bits MSB-first out of a codeword slice.
type bitReader struct {
	cw  []byte
	pos int
}

func (b *bitReader) take(n int) int {
	v := 0
	for i := 0; i < n; i++ {
		v <<= 1
		if b.cw[b.pos/8]&(0x80>>(b.pos%8)) != 0 {
			v |= 1
		}
		b.pos++
	}
	return v
}

func (b *bitReader) takeBytes(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(b.take(8))
	}
	return out
}

// roundtrip encodes the payload, unmask+repacks the matrix with
// qrDecodeData, and checks the byte-mode header and payload codewords.
func roundtrip(t *testing.T, payload []byte) (int, int) {
	t.Helper()
	size, mod := qrEncode(payload)
	if size == 0 || mod == nil {
		t.Fatalf("qrEncode(%q) failed", payload)
	}
	mask, cw, err := qrDecodeData(size, mod)
	if err != nil {
		t.Fatalf("qrDecodeData: %v", err)
	}
	if mask < 0 || mask > 7 {
		t.Fatalf("format info decoded to mask %d, want 0..7", mask)
	}
	// Bit-stream check: 4-bit mode + version-specific count + payload bytes.
	version := (size - 17) / 4
	data := qrDeinterleaveData(cw, qrVersions[version-1].blocks)
	br := bitReader{cw: data}
	if mode := br.take(4); mode != 4 {
		t.Fatalf("mode indicator = %d, want 4 (byte mode)", mode)
	}
	countBits := 8
	if version >= 10 {
		countBits = 16
	}
	if n := br.take(countBits); n != len(payload) {
		t.Fatalf("length field = %d, want %d", n, len(payload))
	}
	if got := br.takeBytes(len(payload)); string(got) != string(payload) {
		t.Fatalf("payload codewords = %q, want %q", got, payload)
	}
	return size, mask
}

func qrDeinterleaveData(codewords []byte, groups []qrDataBlock) []byte {
	type block struct {
		data []byte
	}
	var blocks []block
	offset := 0
	for _, group := range groups {
		for i := 0; i < group.groupCount; i++ {
			blocks = append(blocks, block{data: make([]byte, group.dataLen)})
		}
	}
	maxData := 0
	for _, b := range blocks {
		if len(b.data) > maxData {
			maxData = len(b.data)
		}
	}
	for i := 0; i < maxData; i++ {
		for j := range blocks {
			if i < len(blocks[j].data) {
				blocks[j].data[i] = codewords[offset]
				offset++
			}
		}
	}
	var data []byte
	for _, b := range blocks {
		data = append(data, b.data...)
	}
	return data
}

// TestQRRoundtrip covers small, mid, and large payloads — enough range to
// exercise several versions and the multi-group v8 interleaving.
func TestQRRoundtrip(t *testing.T) {
	cases := []string{
		"A",
		"CTRLIRL|2026-01-02T03:04:05.678Z|f=000006", // the real payload shape
		strings.Repeat("x", 15),
		strings.Repeat("y", 42),
		strings.Repeat("z", 106),
		strings.Repeat("q", 182),
		strings.Repeat("k", 213),
	}
	for _, p := range cases {
		size, _ := roundtrip(t, []byte(p))
		t.Logf("len %d → %dx%d", len(p), size, size)
	}
}

// TestQREnvelope checks the size/version mapping for the standard M-level
// payload capacities.
func TestQREnvelope(t *testing.T) {
	want := []struct {
		n    int
		size int
	}{
		{1, 21},   // v1
		{14, 21},  // v1 full
		{15, 25},  // over v1 → v2
		{26, 25},  // v2 full
		{27, 29},  // over v2 → v3
		{42, 29},  // v3 full
		{43, 33},  // over v3 → v4
		{62, 33},  // v4 full
		{63, 37},  // over v4 → v5
		{84, 37},  // v5 full
		{85, 41},  // over v5 → v6
		{106, 41}, // v6 full
		{107, 45}, // over v6 → v7
		{122, 45}, // v7 full
		{123, 49}, // over v7 → v8
		{152, 49}, // v8 full
		{153, 53}, // over v8 → v9
		{180, 53}, // v9 full
		{181, 57}, // over v9 → v10
		{213, 57}, // v10 full
	}
	for _, c := range want {
		size, _ := roundtrip(t, []byte(strings.Repeat("a", c.n)))
		if size != c.size {
			t.Errorf("payload len %d → %dx%d, want %dx%d", c.n, size, size, c.size, c.size)
		}
	}
}

// TestQRMaxPayload: the largest accepted payload encodes at v10 (57x57),
// one byte more is rejected, and an empty payload is rejected.
func TestQRMaxPayload(t *testing.T) {
	size, _ := roundtrip(t, []byte(strings.Repeat("m", MaxQRPayload)))
	if size != 57 {
		t.Errorf("max payload → %dx%d, want 57x57 (v10)", size, size)
	}
	if s, m := qrEncode([]byte(strings.Repeat("m", MaxQRPayload+1))); s != 0 || m != nil {
		t.Errorf("oversize payload accepted: %d", s)
	}
	if s, m := qrEncode(nil); s != 0 || m != nil {
		t.Errorf("empty payload accepted: %d", s)
	}
}

// TestQRFormatBits pins the format-information encoding for level M.
func TestQRFormatBits(t *testing.T) {
	if got := qrFormatBits(0); got != 0x5412 {
		t.Errorf("M mask 0 = %#x, want 0x5412", got)
	}
	if got := qrFormatBits(1); got != 0x5125 {
		t.Errorf("M mask 1 = %#x, want 0x5125", got)
	}
}

func TestQRFunctionPatterns(t *testing.T) {
	for _, tt := range []struct {
		x, y int
		want bool
	}{
		{-3, 0, true},
		{-2, 0, false},
		{-1, 0, true},
		{0, 0, true},
	} {
		if got := finderDark(tt.x, tt.y); got != tt.want {
			t.Errorf("finderDark(%d,%d) = %v, want %v", tt.x, tt.y, got, tt.want)
		}
	}

	for _, version := range []int{1, 3, 7, 10} {
		t.Run(fmt.Sprintf("version-%d", version), func(t *testing.T) {
			spec := qrVersions[version-1]
			m := newQRMatrix(spec)
			for i, first := range qrFormatCells(spec.size, false) {
				second := qrFormatCells(spec.size, true)[i]
				if m.fun[first[0]][first[1]] != true ||
					m.fun[second[0]][second[1]] != true {
					t.Fatalf("format bit %d not reserved in both copies", i)
				}
			}
			m.writeFormat(3)
			for i, first := range qrFormatCells(spec.size, false) {
				second := qrFormatCells(spec.size, true)[i]
				if m.mod[first[0]][first[1]] != m.mod[second[0]][second[1]] {
					t.Errorf("format copies differ at bit %d", i)
				}
			}
		})
	}

	spec := qrVersions[2]
	m := newQRMatrix(spec)
	if m.fun[20][4] || m.fun[4][20] {
		t.Fatal("alignment patterns overlap bottom-left or top-right finder area")
	}
	if !m.fun[20][20] {
		t.Fatal("version 3 bottom-right alignment pattern is missing")
	}
}

// TestQRDecodeRejectsNotLevelM: a format word whose top two bits are not
// 00 (level M) must be rejected.
func TestQRDecodeRejectsNotLevelM(t *testing.T) {
	size, mat := qrEncode([]byte("probe"))
	// Flip a format bit that carries the level (bit 13 of the format word).
	fc := qrFormatCells(size, false)
	mat[fc[1][0]*size+fc[1][1]] = !mat[fc[1][0]*size+fc[1][1]]
	if _, _, err := qrDecodeData(size, mat); err == nil {
		t.Error("expected level-M rejection")
	}
}
