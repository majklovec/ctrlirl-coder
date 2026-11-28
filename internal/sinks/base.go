// Package sinks is the neutral transport layer the pipeline writes encoded
// bytes into and the control loop reads statistics from. It is deliberately
// free of any cgo dependency: the concrete transports (sinks/srt, which wraps
// libsrt via srtgo, and sinks/srtla, the pure-Go link aggregator) implement
// the Sink interface here, and the balancer and controller consume the Stats
// contract and StatsSource view defined here.
//
// Moving the stats shape and the controller-facing interfaces out of the cgo
// srt package is what lets the balancer, controller, and config build without
// a C toolchain, and lets SRTLA (which has no SRT socket of its own) report
// telemetry through the same Sink surface.
package sinks

import "strings"

// Stats mirrors the libSRT srt_bstats structure for the fields that the
// adaptive balancer consumes. Field names are part of the contract with
// internal/balancer and the controller and must not be renamed without
// updating both.
//
// Values are cumulative (totals) where the underlying SRT counter is
// cumulative, and instantaneous where the underlying field is instantaneous.
type Stats struct {
	RTT               float64 // ms, round-trip time
	PacketLossRate    float64 // %
	SendRateMbps      float64 // Mbps, current send rate
	BandwidthEstimate float64 // Mbps
	RetransmitRate    float64 // packets/s

	// Cumulative counters (used for loss-rate deltas by the adaptive
	// balancer). PktSndLoss + PktRetrans deltas are the congestion signal.
	PktSndLossTotal int64
	PktRetransTotal int64

	// SendBufferBytes is the current send-buffer occupancy in bytes — the
	// "buffer size" congestion input the balancer pulls the target back on.
	SendBufferBytes int

	// ACKTotal is the cumulative received-ACK counter (informational / liveness).
	ACKTotal int64

	// PacketsSentTotal / PacketsReceivedTotal are cumulative packet counters,
	// informational for monitoring.
	PacketsSentTotal     int64
	PacketsReceivedTotal int64

	// TimestampMs is the monotonic millisecond clock value associated with
	// this sample. The controller/stamper sets it at read time.
	TimestampMs uint64
}

// IsValid reports whether the stats sample is usable for bitrate control.
// A sample with a non-positive RTT is treated as "no valid measurement" by
// the adaptive balancer (matching the original RTT_IGNORE behaviour).
func (s Stats) IsValid() bool {
	return s.RTT > 0
}

// Sink is a transport the pipeline writes encoded MPEG-TS bytes into. The
// data path (the appsink puller) calls Write with each buffer; the control
// loop reads Stats() to drive the balancer. Both SRT (single socket) and
// SRTLA (multi-uplink aggregation) implement this.
type Sink interface {
	// Write sends one buffer of SRT payload. It may block for a bounded
	// window while the send buffer drains; a transient congestion is dropped
	// (returns a partial count) rather than tearing the link down.
	Write(p []byte) (int, error)

	// Stats returns a current statistics sample for the control loop. It
	// blocks no data path.
	Stats() (Stats, error)

	// Close releases the transport's resources. Safe to call more than once.
	Close() error
}

// StatsSource is the read-only statistics view the balancer + controller
// consume. A Sink is a StatsSource (Write is simply not part of the view).
type StatsSource interface {
	// Stats returns a current statistics sample.
	Stats() (Stats, error)
	// Close releases any resources held by the source.
	Close() error
}

// Applier is the interface the controller uses to act on the pipeline. Units
// are bps, matching balancer.Balancer.Next. The gstreamer compositor
// implements it; tests use a fake.
type Applier interface {
	// SetBitrate sets the encoder target bitrate (bps).
	SetBitrate(bps int) error
	// SetOverlayText updates the on-screen stats text.
	SetOverlayText(text string)
}

// EncoderUnits returns the unit scale for an encoder element name:
// 1 for "venc_bps" (the encoder's bitrate property is in bps), 1000 for
// "venc_kbps" (property in kbps). Unknown names fall back to kbps (the
// x264enc convention). See spec §7.4: detect once at startup, cache the mode.
func EncoderUnits(encoderName string) int {
	if strings.HasSuffix(encoderName, "_bps") {
		return 1
	}
	return 1000
}
