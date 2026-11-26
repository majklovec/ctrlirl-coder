package srt

import (
	"errors"

	"ctrlirl-coder/internal/sinks"
	"github.com/haivision/srtgo"
)

// ErrNotConnected is returned by the sender data/stats path when the link is
// down. The data path treats it as "drop this buffer"; the controller treats a
// failed Stats call as "no valid measurement this tick".
var ErrNotConnected = errors.New("srt: not connected")

// mapStats converts a raw srtgo.SrtStats snapshot into the balancer-facing
// Stats contract. Field meanings match the original: cumulative counters are
// kept as totals (the adaptive balancer derives rates from deltas), and the
// instantaneous fields map straight through.
//
// SendBufferBytes is the unACKed bytes in the SRT send buffer (SrtStats
// ByteSndBuf) — the Go equivalent of the original's SRTO_SNDDATA read. That is
// the primary congestion input: when in-flight bytes climb, the balancer pulls
// the target bitrate back.
func mapStats(raw *srtgo.SrtStats) sinks.Stats {
	s := sinks.Stats{
		RTT:                  raw.MsRTT,
		SendRateMbps:         raw.MbpsSendRate,
		BandwidthEstimate:    raw.MbpsBandwidth,
		PktSndLossTotal:      int64(raw.PktSndLossTotal),
		PktRetransTotal:      int64(raw.PktRetransTotal),
		SendBufferBytes:      raw.ByteSndBuf,
		ACKTotal:             int64(raw.PktRecvACKTotal),
		PacketsSentTotal:     raw.PktSentTotal,
		PacketsReceivedTotal: raw.PktRecvTotal,
		RetransmitRate:       float64(raw.PktRetrans),
		TimestampMs:          uint64(raw.MsTimeStamp),
	}
	// PacketLossRate as a % of sent packets (0 when nothing sent yet).
	if raw.PktSentTotal > 0 {
		s.PacketLossRate = float64(raw.PktSndLossTotal) / float64(raw.PktSentTotal) * 100
	}
	return s
}
