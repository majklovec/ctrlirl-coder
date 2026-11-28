// Package srtla implements the SRTLA (SRT Link Aggregation) sender protocol.
//
// Wire format matches the reference implementation (irlserver/srtla, forked
// from BELABOX): data is a plain SRT stream carried as raw SRT datagrams over
// several bound UDP sockets; only the control packets are SRTLA-specific and
// are distinguished by the 2-byte big-endian type field shared with SRT:
//
//	SRT control types   0x8000 (handshake) .. 0x8005 (shutdown)
//	SRTLA control types 0x9000 (keepalive) .. 0x9212 (reg nak)
//
// Registration (group = one SRT stream, connection = one uplink):
//
//	REG1 (0x9200)  sender -> receiver: 2-byte type + 256-byte proposed group ID
//	REG2 (0x9201)  receiver -> sender: 2-byte type + 256-byte group ID
//	                 (the receiver's ID is authoritative; the sender adopts it
//	                 and re-sends REG2 on every uplink to register each one)
//	REG3 (0x9202)  receiver -> sender: 2-byte type; uplink connection confirmed
//	REG_ERR(0x9210) receiver: registration refused
//	REG_NGP(0x9211) receiver: no group found for the given ID (re-register)
//
// Data packets have no SRTLA wrapper: the receiver reorders them using the
// SRT sequence number (first 4 bytes, big-endian, bit 31 clear).
//
// NOTE: SPECS.md §14.3.1 sketches an 8-byte SRTLA header (type/length/seq)
// and REG types 0x9300-0x9500; the reference implementation uses none of
// that. This package follows the reference (spec revision recorded in
// IMPLEMENTATION.md §4).
package srtla

import (
	"crypto/rand"
	"encoding/binary"
)

// Control packet types. The first 2 bytes of every datagram (big endian) are
// the type: SRT types for the underlying stream, SRTLA types for the
// aggregation control plane.
const (
	TypeKeepalive uint16 = 0x9000
	TypeACK       uint16 = 0x9100 // SRTLA per-packet ack list
	TypeREG1      uint16 = 0x9200 // register group
	TypeREG2      uint16 = 0x9201 // group confirmed / register connection
	TypeREG3      uint16 = 0x9202 // connection confirmed
	TypeREGErr    uint16 = 0x9210 // registration refused
	TypeREGNGP    uint16 = 0x9211 // no group: re-register
	TypeREGNAK    uint16 = 0x9212 // registration NAK

	SRTTypeHandshake uint16 = 0x8000
	SRTTypeACK       uint16 = 0x8002
	SRTTypeNAK       uint16 = 0x8003
	SRTTypeShutdown  uint16 = 0x8005
)

// Wire geometry, mirrored from the reference common.h.
const (
	// IDLen is the group-identifier length carried in REG1/REG2 payloads.
	IDLen = 256

	REGLen  = 2 + IDLen // REG1 and REG2 are the same length
	REG3Len = 2

	// KeepaliveStdLen is the legacy keepalive size: the 2-byte type plus
	// 8 reserved bytes. The reference sender in fact emits only the 2-byte
	// type; receivers accept any 2-byte-or-longer keepalive.
	KeepaliveStdLen = 10
	// KeepaliveExtLen is the extended keepalive size: standard 10 bytes,
	// then magic + version + the 24-byte connection-info block.
	KeepaliveExtLen = 38

	KeepaliveMagic  uint16 = 0xC01F
	KeepaliveExtVer uint16 = 0x0001

	// SRTMinLen is the size of a bare SRT header (type, subtype, info,
	// timestamp, destination). ACK/NAK payloads start right after it.
	SRTMinLen = 16
)

// SRTMaxPayload is the SRT live-mode per-packet payload cap (SRT_MAXSRTPAYLOAD:
// 1500 MTU − 84 bytes of IP/UDP/SRT headers).
const SRTMaxPayload = 1316

// ConnInfo is the extended-keepalive telemetry block (24 bytes, big endian).
type ConnInfo struct {
	ConnID     uint32
	Window     int32
	InFlight   int32
	RTTMs      uint32
	NAKCount   uint32
	BitrateBPS uint32 // bytes per second
}

// Type returns the 2-byte big-endian packet type, or 0 when the packet is
// too short to carry one.
func Type(b []byte) uint16 {
	if len(b) < 2 {
		return 0
	}
	return binary.BigEndian.Uint16(b)
}

// SeqNumber returns the SRT sequence number of a data packet: the first 4
// bytes big endian. It returns -1 for control packets (bit 31 set in the
// first word, where the type lives) or packets shorter than 4 bytes.
func SeqNumber(b []byte) int32 {
	if len(b) < 4 {
		return -1
	}
	sn := binary.BigEndian.Uint32(b)
	if sn&0x80000000 != 0 {
		return -1
	}
	return int32(sn)
}

// RandomGroupID returns a fresh random 128-byte group identifier, as the
// reference seeds from /dev/urandom.
func RandomGroupID() [IDLen]byte {
	var id [IDLen / 2]byte
	if _, err := rand.Read(id[:]); err != nil {
		// crypto/rand only fails on broken platforms; panic is appropriate.
		panic("srtla: reading group id: " + err.Error())
	}
	var out [IDLen]byte
	copy(out[:IDLen/2], id[:])
	return out
}

// BuildREG1 is the group-registration request: type + proposed group ID.
func BuildREG1(id [IDLen]byte) []byte {
	b := make([]byte, REGLen)
	binary.BigEndian.PutUint16(b, TypeREG1)
	copy(b[2:], id[:])
	return b
}

// BuildREG2 is the group-confirmation / per-uplink registration packet:
// type + group ID.
func BuildREG2(id [IDLen]byte) []byte {
	b := make([]byte, REGLen)
	binary.BigEndian.PutUint16(b, TypeREG2)
	copy(b[2:], id[:])
	return b
}

// BuildREG3 is the connection-confirmation packet (type only).
func BuildREG3() []byte {
	b := make([]byte, REG3Len)
	binary.BigEndian.PutUint16(b, TypeREG3)
	return b
}

// ParseREG1 extracts the proposed group ID from a well-formed REG1 packet.
func ParseREG1(b []byte) ([IDLen]byte, bool) {
	var id [IDLen]byte
	if len(b) != REGLen || Type(b) != TypeREG1 {
		return id, false
	}
	copy(id[:], b[2:])
	return id, true
}

// ParseREG2 extracts the group ID from a well-formed REG2 packet.
func ParseREG2(b []byte) ([IDLen]byte, bool) {
	var id [IDLen]byte
	if len(b) != REGLen || Type(b) != TypeREG2 {
		return id, false
	}
	copy(id[:], b[2:])
	return id, true
}

// BuildKeepalive is the legacy keepalive: the 2-byte type only, exactly what
// the reference sender emits when an uplink has been idle.
func BuildKeepalive() []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, TypeKeepalive)
	return b
}

// BuildExtendedKeepalive is the 38-byte keepalive carrying per-uplink
// telemetry for receivers that understand the extension.
func BuildExtendedKeepalive(info ConnInfo) []byte {
	b := make([]byte, KeepaliveExtLen)
	binary.BigEndian.PutUint16(b, TypeKeepalive)
	// b[2:10] stays zero: reserved tail of the legacy 10-byte keepalive.
	binary.BigEndian.PutUint16(b[10:], KeepaliveMagic)
	binary.BigEndian.PutUint16(b[12:], KeepaliveExtVer)
	binary.BigEndian.PutUint32(b[14:], info.ConnID)
	binary.BigEndian.PutUint32(b[18:], uint32(info.Window))
	binary.BigEndian.PutUint32(b[22:], uint32(info.InFlight))
	binary.BigEndian.PutUint32(b[26:], info.RTTMs)
	binary.BigEndian.PutUint32(b[30:], info.NAKCount)
	binary.BigEndian.PutUint32(b[34:], info.BitrateBPS)
	return b
}

// ParseExtendedKeepalive decodes the extended-keepalive telemetry block. It
// reports false for legacy keepalives and any packet that is not an
// extended keepalive.
func ParseExtendedKeepalive(b []byte) (ConnInfo, bool) {
	if len(b) < KeepaliveExtLen || Type(b) != TypeKeepalive {
		return ConnInfo{}, false
	}
	if binary.BigEndian.Uint16(b[10:]) != KeepaliveMagic ||
		binary.BigEndian.Uint16(b[12:]) != KeepaliveExtVer {
		return ConnInfo{}, false
	}
	return ConnInfo{
		ConnID:     binary.BigEndian.Uint32(b[14:]),
		Window:     int32(binary.BigEndian.Uint32(b[18:])),
		InFlight:   int32(binary.BigEndian.Uint32(b[22:])),
		RTTMs:      binary.BigEndian.Uint32(b[26:]),
		NAKCount:   binary.BigEndian.Uint32(b[30:]),
		BitrateBPS: binary.BigEndian.Uint32(b[34:]),
	}, true
}

// BuildACK is the SRTLA per-packet ack list: a 4-byte header (type << 16,
// matching the reference's `htobe32(SRTLA_TYPE_ACK << 16)`) followed by one
// big-endian sequence number per acknowledged data packet.
func BuildACK(acks ...int32) []byte {
	b := make([]byte, 4+4*len(acks))
	binary.BigEndian.PutUint32(b, uint32(TypeACK)<<16)
	for i, sn := range acks {
		binary.BigEndian.PutUint32(b[4+4*i:], uint32(sn))
	}
	return b
}

// ParseACKs returns the acknowledged sequence numbers in an SRTLA ACK packet.
// The 4-byte header is skipped, word by word, per the reference.
func ParseACKs(b []byte) []int32 {
	if Type(b) != TypeACK {
		return nil
	}
	p := ackSeqs(b)
	acks := make([]int32, 0, len(p)/4)
	for ; len(p) >= 4; p = p[4:] {
		acks = append(acks, seqAt(p))
	}
	return acks
}

// ackSeqs is the sequence-number list of an SRTLA ACK packet: what follows
// the 4-byte header, 4 bytes per acknowledged packet. The feedback path
// walks it in place (seqAt) instead of allocating a slice per ACK packet.
func ackSeqs(b []byte) []byte {
	if len(b) < 4 {
		return nil
	}
	return b[4:]
}

// seqAt reads the big-endian sequence number at the start of p.
func seqAt(p []byte) int32 { return int32(binary.BigEndian.Uint32(p)) }

// SRTAckLast returns the cumulative "last acked sequence" from an SRT ACK
// packet, stored big endian right after the 16-byte SRT header.
func SRTAckLast(b []byte) (int32, bool) {
	if len(b) < SRTMinLen+4 || Type(b) != SRTTypeACK {
		return 0, false
	}
	return int32(binary.BigEndian.Uint32(b[SRTMinLen:])), true
}

// SRTNAKLosses expands the loss list of an SRT NAK packet into the set of
// lost sequence numbers. Words read big endian starting right after the 16-
// byte SRT header (reference behavior, list-length word included); a word
// with bit 31 set begins a range whose following word is the last sequence
// number of the run.
func SRTNAKLosses(b []byte) []int32 {
	if len(b) < SRTMinLen || Type(b) != SRTTypeNAK {
		return nil
	}
	losses := make([]int32, 0, 8)
	words := (len(b) - SRTMinLen) / 4
	for i := 0; i < words && len(losses) < MaxNAKEpoch; i++ {
		w := int32(binary.BigEndian.Uint32(b[SRTMinLen+4*i:]))
		if w < 0 { // range start
			first := w & 0x7FFFFFFF
			if i+1 >= words {
				break
			}
			last := binary.BigEndian.Uint32(b[SRTMinLen+4*(i+1):])
			if last&0x80000000 != 0 {
				i++
				continue
			}
			for sn := int64(first); sn <= int64(last) && len(losses) < MaxNAKEpoch; sn++ {
				losses = append(losses, int32(sn))
			}
			i++
		} else {
			losses = append(losses, w)
		}
	}
	return losses
}
