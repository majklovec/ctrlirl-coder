// Pure-Go QR Code encoder for the testcode layer's machine-readable
// timestamp. Scope: byte mode only, versions 1–10, error-correction level M.
//
// The payload is an ASCII timestamp + frame counter (~41 bytes), so the code
// lands on version 3 (29 modules) — large enough for a camera at stream
// distance, small enough to sit in a 1280x720 canvas.
//
// Deliberately self-contained (no third-party QR dep): the block tables are
// the ISO/IEC 18004 version-1–10, level-M rows. Unit tests unmask,
// deinterleave, and verify the byte-mode payload.
package testcode

import (
	"errors"
	"fmt"
)

const maxQRVersion = 10

// MaxQRPayload is the largest payload the encoder accepts (version 10,
// level M, byte mode). Computed at init from the block tables because
// function calls cannot appear in constant expressions.
var MaxQRPayload = qrByteCapacity(maxQRVersion)

// qrDataBlock is one EC-block group: groupCount blocks of dataLen data
// codewords with ecLen parity codewords each.
type qrDataBlock struct {
	dataLen    int
	ecLen      int
	groupCount int
}

// qrVersionSpec is the level-M block structure of one version.
type qrVersionSpec struct {
	size      int // matrix dimension in modules (17 + 4*version)
	alignment []int
	blocks    []qrDataBlock
}

// qrVersions holds the level-M specs for versions 1–10.
var qrVersions = [...]qrVersionSpec{
	{21, nil, []qrDataBlock{{16, 10, 1}}},
	{25, []int{6, 18}, []qrDataBlock{{28, 16, 1}}},
	{29, []int{6, 22}, []qrDataBlock{{44, 26, 1}}},
	{33, []int{6, 26}, []qrDataBlock{{32, 18, 2}}},
	{37, []int{6, 30}, []qrDataBlock{{43, 24, 2}}},
	{41, []int{6, 34}, []qrDataBlock{{27, 16, 4}}},
	{45, []int{6, 38}, []qrDataBlock{{31, 18, 4}}},
	{49, []int{6, 42}, []qrDataBlock{{38, 22, 2}, {39, 22, 2}}},
	{53, []int{6, 46}, []qrDataBlock{{36, 22, 3}, {37, 22, 2}}},
	{57, []int{6, 50}, []qrDataBlock{{43, 26, 4}, {44, 26, 1}}},
}

// qrFormatXorMask is the constant XOR'd into every 15-bit format string.
const qrFormatXorMask = 0x5412

// qrByteCapacity returns the maximum byte-mode payload of version v at
// level M.
func qrByteCapacity(v int) int {
	n := 0
	for _, b := range qrVersions[v-1].blocks {
		n += b.dataLen * b.groupCount
	}
	return (n*8 - 4 - qrByteCountBits(v)) / 8
}

func qrByteCountBits(v int) int {
	if v < 10 {
		return 8
	}
	return 16
}

// qrEncode encodes payload (1..MaxQRPayload bytes) as a byte-mode, level-M
// QR code and returns its module matrix (true = dark module, row-major) and
// the matrix size. It picks the smallest fitting version, selects the best
// of the 8 masking patterns by penalty score, and writes the format info.
// An empty payload or an oversized one returns size 0 and a nil matrix.
func qrEncode(payload []byte) (int, []bool) {
	size, mod, err := qrEncodeMatrix(payload)
	if err != nil {
		return 0, nil
	}
	flat := make([]bool, size*size)
	for y := 0; y < size; y++ {
		copy(flat[y*size:], mod[y])
	}
	return size, flat
}

// qrEncodeMatrix renders the payload and returns the matrix size plus the
// best masked module grid.
func qrEncodeMatrix(payload []byte) (int, [][]bool, error) {
	if len(payload) == 0 || len(payload) > MaxQRPayload {
		return 0, nil, fmt.Errorf("payload %d bytes outside QR byte range 1..%d",
			len(payload), MaxQRPayload)
	}
	v, all, err := qrBuildCodewords(payload)
	if err != nil {
		return 0, nil, err
	}
	spec := qrVersions[v-1]

	// 3. Function modules, then the data zigzag.
	m := newQRMatrix(spec)
	m.placeData(all)

	// 4. Best mask by penalty score.
	bestScore := -1
	var bestMod [][]bool
	for msk := 0; msk < 8; msk++ {
		cand := m.copy()
		cand.applyMask(msk)
		cand.writeFormat(msk)
		score := cand.penalty()
		if bestScore < 0 || score < bestScore {
			bestScore, bestMod = score, cand.mod
		}
	}
	return spec.size, bestMod, nil
}

// qrBuildCodewords assembles the full interleaved codeword stream (data +
// RS parity) for payload and returns the smallest fitting version.
func qrBuildCodewords(payload []byte) (int, []byte, error) {
	if len(payload) == 0 || len(payload) > MaxQRPayload {
		return 0, nil, fmt.Errorf("payload %d bytes outside QR byte range 1..%d",
			len(payload), MaxQRPayload)
	}
	v := 1
	for v <= maxQRVersion && len(payload) > qrByteCapacity(v) {
		v++
	}
	data := qrDataCodewords(v)

	// 1. Data bit stream: mode 0100, byte count, payload, terminator,
	//    byte alignment, pad bytes 0xEC/0x11 up to the codeword count.
	var bits []bool
	add := func(val, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, val&(1<<i) != 0)
		}
	}
	add(0x4, 4)
	add(len(payload), qrByteCountBits(v))
	for _, b := range payload {
		add(int(b), 8)
	}
	add(0, min(4, data*8-len(bits))) // terminator
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	pad := []byte{0xEC, 0x11}
	for i := 0; len(bits) < data*8; i = (i + 1) % 2 {
		add(int(pad[i]), 8)
	}
	dataCw := make([]byte, data)
	for i := range dataCw {
		var acc byte
		for j := 0; j < 8; j++ {
			acc <<= 1
			if bits[i*8+j] {
				acc |= 1
			}
		}
		dataCw[i] = acc
	}

	// 2. Split into blocks, append per-block RS parity, interleave.
	return v, qrInterleave(dataCw, qrVersions[v-1].blocks), nil
}

// qrDataCodewords returns the total data codewords of version v (level M).
func qrDataCodewords(v int) int {
	n := 0
	for _, b := range qrVersions[v-1].blocks {
		n += b.dataLen * b.groupCount
	}
	return n
}

// qrInterleave splits the data codewords into the version's blocks,
// computes each block's RS parity, and returns the full codeword sequence
// in QR interleave order (data bytes block by block, then parity bytes
// block by block).
func qrInterleave(data []byte, groups []qrDataBlock) []byte {
	type blk struct{ data, ec []byte }
	blocks := make([]blk, 0)
	off := 0
	for _, g := range groups {
		for i := 0; i < g.groupCount; i++ {
			d := data[off : off+g.dataLen]
			off += g.dataLen
			blocks = append(blocks, blk{data: d, ec: rsAppend(d, g.ecLen)})
		}
	}
	maxData, maxEc := 0, 0
	for _, b := range blocks {
		if len(b.data) > maxData {
			maxData = len(b.data)
		}
		if len(b.ec) > maxEc {
			maxEc = len(b.ec)
		}
	}
	out := make([]byte, 0, maxData+len(data))
	for i := 0; i < maxData; i++ {
		for _, b := range blocks {
			if i < len(b.data) {
				out = append(out, b.data[i])
			}
		}
	}
	for i := 0; i < maxEc; i++ {
		for _, b := range blocks {
			if i < len(b.ec) {
				out = append(out, b.ec[i])
			}
		}
	}
	return out
}

// qrMatrix holds a QR matrix under construction: mod (current module value)
// and fun (true where a function/reserved module forbids data).
type qrMatrix struct {
	size int
	mod  [][]bool
	fun  [][]bool
}

// newQRMatrix builds the function-module layout for a version: finders with
// separators, timing, the dark module, alignment patterns, and the reserved
// format regions.
func newQRMatrix(spec qrVersionSpec) *qrMatrix {
	m := &qrMatrix{size: spec.size, mod: make([][]bool, spec.size), fun: make([][]bool, spec.size)}
	for i := range m.mod {
		m.mod[i] = make([]bool, spec.size)
		m.fun[i] = make([]bool, spec.size)
	}
	m.markFinders()
	m.markTiming()
	m.markAlignment(spec.alignment)
	m.markVersionInfo()
	m.markFormatRegions()
	// The data region is filled by the caller (qrEncode places the
	// interleaved codewords before the mask selection).
	return m
}

// markFinders stamps the three finder patterns with their light separators.
func (m *qrMatrix) markFinders() {
	// Stamp the 7x7 finder plus its 1-module separator (8x8 total) at the
	// three corners. cx,cy is the finder center (row/col 3 from the corner).
	stamp := func(cx, cy int) {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				px, py := cx+dx, cy+dy
				if px < 0 || py < 0 || px >= m.size || py >= m.size {
					continue
				}
				m.fun[py][px] = true
				if absInt(dx) <= 3 && absInt(dy) <= 3 {
					m.mod[py][px] = finderDark(dx, dy)
				}
			}
		}
	}
	stamp(3, 3)
	stamp(m.size-4, 3)
	stamp(3, m.size-4)
}

// finderDark reports whether (x, y) relative to a finder center (-3..3) is
// dark: the 7x7 dark/white/dark concentric rings.
func finderDark(x, y int) bool {
	ring := max(absInt(x), absInt(y))
	return ring == 3 || ring <= 1
}

// markTiming writes the alternating timing patterns on row/col 6 between the
// finders (the finder regions already own the ends).
func (m *qrMatrix) markTiming() {
	for i := 8; i < m.size-8; i++ {
		dark := i%2 == 0
		m.mod[6][i] = dark
		m.fun[6][i] = true
		m.mod[i][6] = dark
		m.fun[i][6] = true
	}
}

// markAlignment stamps the version's alignment patterns (5x5), skipping the
// three that would overlap the finder corners.
func (m *qrMatrix) markAlignment(coords []int) {
	if len(coords) < 2 {
		return
	}
	for _, cx := range coords {
		for _, cy := range coords {
			last := m.size - 7
			if (cx == 6 && cy == 6) ||
				(cx == last && cy == 6) ||
				(cx == 6 && cy == last) {
				continue // finder corner
			}
			for y := -2; y <= 2; y++ {
				for x := -2; x <= 2; x++ {
					if max(absInt(x), absInt(y)) != 1 {
						m.mod[cy+y][cx+x] = true
					}
					m.fun[cy+y][cx+x] = true
				}
			}
		}
	}
}

// markVersionInfo writes and reserves the duplicated version-information
// fields used by versions 7 and above.
func (m *qrMatrix) markVersionInfo() {
	version := (m.size - 17) / 4
	if version < 7 {
		return
	}

	word := uint32(version) << 12
	remainder := word
	for bit := 17; bit >= 12; bit-- {
		if remainder&(1<<bit) != 0 {
			remainder ^= 0x1F25 << (bit - 12)
		}
	}
	word |= remainder & 0xFFF
	for i := 0; i < 18; i++ {
		dark := word&(1<<(17-i)) != 0
		row, col := i/3, i%3+m.size-11
		m.mod[row][col], m.fun[row][col] = dark, true
		row, col = i%3+m.size-11, i/3
		m.mod[row][col], m.fun[row][col] = dark, true
	}
}

// markFormatRegions reserves the two 15-module format-info placements and
// the always-dark module at (size-1, 8) (values written after masking).
func (m *qrMatrix) markFormatRegions() {
	for i := 0; i < 9; i++ {
		m.fun[8][i] = true
		m.fun[i][8] = true
	}
	for i := 0; i < 8; i++ {
		m.fun[8][m.size-1-i] = true
		m.fun[m.size-1-i][8] = true
	}
	// Dark module ("always dark") sits at (S-8, 8), not (S-1, 8).
	m.mod[m.size-8][8] = true
	m.fun[m.size-8][8] = true
}

// placeData fills the remaining modules with the codeword bits along the
// standard upward zigzag (right column pair first, skipping the timing
// column 7). It must be called after the function modules are marked.
func (m *qrMatrix) placeData(cw []byte) {
	if cw == nil {
		return
	}
	bit := 0
	get := func() bool {
		if bit >= len(cw)*8 {
			bit++ // remainder bits are always 0
			return false
		}
		b := cw[bit/8]&(1<<(7-bit%8)) != 0
		bit++
		return b
	}
	up := true
	for col := m.size - 1; col > 0; col -= 2 {
		if col == 6 {
			col-- // step over the timing column
		}
		var rows []int
		if up {
			for y := m.size - 1; y >= 0; y-- {
				rows = append(rows, y)
			}
		} else {
			for y := 0; y < m.size; y++ {
				rows = append(rows, y)
			}
		}
		up = !up
		for _, y := range rows {
			if x := col; !m.fun[y][x] {
				m.mod[y][x] = get()
			}
			if x := col - 1; x >= 0 && !m.fun[y][x] {
				m.mod[y][x] = get()
			}
		}
	}
}

// copy returns an independent copy of the matrix.
func (m *qrMatrix) copy() *qrMatrix {
	c := &qrMatrix{size: m.size, mod: make([][]bool, m.size), fun: make([][]bool, m.size)}
	for y := 0; y < m.size; y++ {
		c.mod[y] = append([]bool(nil), m.mod[y]...)
		c.fun[y] = append([]bool(nil), m.fun[y]...)
	}
	return c
}

// applyMask XORs mask pattern m over every data module (function modules
// are untouched).
func (m *qrMatrix) applyMask(pattern int) {
	for y := 0; y < m.size; y++ {
		for x := 0; x < m.size; x++ {
			if !m.fun[y][x] && qrMaskHit(pattern, x, y) {
				m.mod[y][x] = !m.mod[y][x]
			}
		}
	}
}

// qrMaskHit evaluates the 8 standard mask conditions (x = column, y = row).
func qrMaskHit(pattern, x, y int) bool {
	switch pattern {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (y/2+x/3)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	case 7:
		return ((x+y)%2+x*y%3)%2 == 0
	}
	return false
}

// qrFormatBits returns the 15-bit format string for level M and the given
// mask: (level<<3|mask) CRC-5'd (polynomial x^10+x^8+x^5+x^4+x^2+x+1) and
// XOR'd with 0x5412.
func qrFormatBits(mask int) uint32 {
	// 15-bit format string (ISO/IEC 18004 §8.8.2): 5-bit level/mask code
	// (level M = 00) + 10-bit BCH remainder mod x^10+x^8+x^5+x^4+x^2+x+1,
	// the whole 15 bits XORed with 0x5412.
	data := uint32(mask) << 10
	d := data
	for i := 0; i < 5; i++ {
		d = (d << 1) ^ (uint32(d>>14&1) * 0x537)
	}
	return (data | (d & 0x3FF)) ^ qrFormatXorMask
}

// qrFormatCells is the (row, col) order of the 15 format bits in one
// placement (bit 0 = most significant).
func qrFormatCells(size int, second bool) [15][2]int {
	if !second {
		return [15][2]int{{8, 0}, {8, 1}, {8, 2}, {8, 3}, {8, 4}, {8, 5}, {8, 7}, {8, 8}, {7, 8}, {5, 8}, {4, 8}, {3, 8}, {2, 8}, {1, 8}, {0, 8}}
	}
	return [15][2]int{{size - 1, 8}, {size - 2, 8}, {size - 3, 8}, {size - 4, 8}, {size - 5, 8}, {size - 6, 8}, {size - 7, 8}, {8, size - 8}, {8, size - 7}, {8, size - 6}, {8, size - 5}, {8, size - 4}, {8, size - 3}, {8, size - 2}, {8, size - 1}}
}

// writeFormat stamps the format bits into both placements.
func (m *qrMatrix) writeFormat(mask int) {
	f := qrFormatBits(mask)
	bit := func(i int) bool { return f&(1<<(14-i)) != 0 }
	for i, p := range qrFormatCells(m.size, false) {
		m.mod[p[0]][p[1]] = bit(i)
	}
	for i, p := range qrFormatCells(m.size, true) {
		m.mod[p[0]][p[1]] = bit(i)
	}
}

// penalty scores the matrix per the four spec rules (lower = better mask).
func (m *qrMatrix) penalty() int {
	score := 0
	// Rule 1: runs of 5+ same-color modules in rows and columns.
	for line := 0; line < m.size*2; line++ {
		run, color, started := 0, false, false
		for i := 0; i < m.size; i++ {
			var c bool
			if line < m.size {
				c = m.mod[line][i]
			} else {
				c = m.mod[i][line-m.size]
			}
			if started && c == color {
				run++
			} else {
				if started && run >= 5 {
					score += 3 + run - 5
				}
				run, color, started = 1, c, true
			}
		}
		if run >= 5 {
			score += 3 + run - 5
		}
	}
	// Rule 2: 2x2 blocks of one color.
	for y := 0; y < m.size-1; y++ {
		for x := 0; x < m.size-1; x++ {
			c := m.mod[y][x]
			if c == m.mod[y][x+1] && c == m.mod[y+1][x] && c == m.mod[y+1][x+1] {
				score += 3
			}
		}
	}
	// Rule 3: the 1:1:3:1:1 scanner patterns (rows and columns), either
	// polarity.
	pat := []bool{true, false, true, true, true, false, true, false, false, false}
	inv := make([]bool, len(pat))
	for i, p := range pat {
		inv[i] = !p
	}
	for line := 0; line < m.size*2; line++ {
		for start := 0; start+10 <= m.size; start++ {
			hit := false
			for p := 0; p < 2; p++ {
				match := true
				for k := 0; k < 10; k++ {
					var c bool
					if line < m.size {
						c = m.mod[line][start+k]
					} else {
						c = m.mod[start+k][line-m.size]
					}
					patRef := pat
					if p == 1 {
						patRef = inv
					}
					if c != patRef[k] {
						match = false
						break
					}
				}
				if match {
					hit = true
				}
			}
			if hit {
				score += 40
			}
		}
	}
	// Rule 4: deviation of the dark-module ratio from 50%.
	dark := 0
	for y := 0; y < m.size; y++ {
		for x := 0; x < m.size; x++ {
			if m.mod[y][x] {
				dark++
			}
		}
	}
	percent := dark * 100 / (m.size * m.size)
	return score + 10*(absInt(percent-50)/5)
}

// qrDecodeData is the inverse of qrEncode used by the tests: it reads the
// format info of an encoded matrix, unmasks the data region, and repackets
// the codewords in placement order.
func qrDecodeData(size int, mod []bool) (mask int, dataCw []byte, err error) {
	spec, ok := qrSpecForSize(size)
	if !ok {
		return 0, nil, fmt.Errorf("qr: no version for matrix size %d", size)
	}
	m := newQRMatrix(spec)

	var f uint32
	for i, p := range qrFormatCells(size, false) {
		if mod[p[0]*size+p[1]] {
			f |= 1 << (14 - i)
		}
	}
	data := f ^ qrFormatXorMask
	if data>>13 != 0 { // the 2 MSB of the un-XORed word are the level code: M = 00
		return 0, nil, errors.New("qr: format bits not level M")
	}
	mask = int(data >> 10 & 0x7)

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if m.fun[y][x] {
				continue
			}
			v := mod[y*size+x]
			if qrMaskHit(mask, x, y) {
				v = !v
			}
			m.mod[y][x] = v
		}
	}

	var bits []bool
	up := true
	for col := size - 1; col > 0; col -= 2 {
		if col == 6 {
			col--
		}
		var rows []int
		if up {
			for y := size - 1; y >= 0; y-- {
				rows = append(rows, y)
			}
		} else {
			for y := 0; y < size; y++ {
				rows = append(rows, y)
			}
		}
		up = !up
		for _, y := range rows {
			if x := col; !m.fun[y][x] {
				bits = append(bits, m.mod[y][x])
			}
			if x := col - 1; x >= 0 && !m.fun[y][x] {
				bits = append(bits, m.mod[y][x])
			}
		}
	}
	dataCw = make([]byte, len(bits)/8)
	for i := range dataCw {
		var acc byte
		for j := 0; j < 8; j++ {
			acc <<= 1
			if bits[i*8+j] {
				acc |= 1
			}
		}
		dataCw[i] = acc
	}
	return mask, dataCw, nil
}

// qrSpecForSize maps a matrix dimension back to its version spec.
func qrSpecForSize(size int) (qrVersionSpec, bool) {
	for _, s := range qrVersions {
		if s.size == size {
			return s, true
		}
	}
	return qrVersionSpec{}, false
}

// absInt is the small integer absolute value.
func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// rsAppend computes the ecLen Reed-Solomon parity codewords of data over
// GF(2^8) with the QR polynomial (0x11D) and returns them.
func rsAppend(data []byte, ecLen int) []byte {
	// Generator polynomial: product of (x - alpha^i), i = 0..ecLen-1
	// (characteristic 2: minus == plus). Coefficients low order first.
	gen := []byte{1}
	for i := 0; i < ecLen; i++ {
		ng := make([]byte, len(gen)+1)
		for j, c := range gen {
			ng[j+1] ^= c                // x * term
			ng[j] ^= rsMul(c, rsExp[i]) // alpha^i * term
		}
		gen = ng
	}
	out := append(append([]byte(nil), data...), make([]byte, ecLen)...)
	for i := 0; i < len(data); i++ {
		factor := out[i]
		if factor == 0 {
			continue
		}
		// gen is low-order-first (gen[ecLen] == 1): fold each generator
		// coefficient into the ecLen parity positions behind it.
		for j := 0; j < ecLen; j++ {
			out[i+1+j] ^= rsMul(gen[ecLen-1-j], factor)
		}
	}
	return out[len(data):]
}

// rsExp/rsLog are the GF(2^8) primitive-element tables (primitive
// polynomial 0x11D, as mandated by the QR spec).
var rsExp = make([]byte, 512)
var rsLog = make([]byte, 256)

func init() {
	x := byte(1)
	for i := 0; i < 255; i++ {
		rsExp[i] = x
		rsLog[x] = byte(i)
		if x&0x80 != 0 {
			x = x<<1 ^ 0x1D
		} else {
			x <<= 1
		}
	}
	copy(rsExp[255:], rsExp[:255])
}

// rsMul multiplies two GF(2^8) elements via the log/exp tables.
func rsMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return rsExp[int(rsLog[a])+int(rsLog[b])]
}
