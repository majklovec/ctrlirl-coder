package srtla

import (
	"sync"
	"time"

	"ctrlirl-coder/internal/sinks"
)

// meter is a small EWMA rate estimator over a cumulative counter. Callers
// pass the current counter total on each Sample; the returned rate is
// smoothed units/second.
type meter struct {
	mu   sync.Mutex
	last float64
	t0   time.Time
	ewma float64
}

func (m *meter) Sample(now time.Time, total float64) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	dt := now.Sub(m.t0).Seconds()
	m.t0 = now
	if dt <= 0 {
		return m.ewma
	}
	inst := (total - m.last) / dt
	m.last = total
	if inst < 0 {
		inst = 0
	}
	if m.ewma == 0 {
		m.ewma = inst
	} else {
		m.ewma = 0.8*m.ewma + 0.2*inst
	}
	return m.ewma
}

func (m *meter) init(now time.Time) {
	m.mu.Lock()
	m.t0 = now
	m.mu.Unlock()
}

// UplinkStat is one uplink's telemetry snapshot (metrics surface).
type UplinkStat struct {
	ID             int
	IP             string
	RTTMs          float64
	InFlight       int
	Window         int
	Weight         float64
	Established    bool
	NAKBurst       int32
	LastActivityMs float64 // ms since last activity (0 = never active)
}

// Detailed is the SRTLA-specific telemetry beyond the shared sinks.Stats the
// balancer consumes. It is the /metrics surface for the aggregator.
type Detailed struct {
	Mode         string
	Uplinks      []UplinkStat
	ActiveConns  int
	HasConnected bool
	Failover     bool
}

// DetailedProvider is implemented by sinks that expose SRTLA-specific
// telemetry (the metrics endpoint type-asserts on it).
type DetailedProvider interface {
	Detailed() Detailed
}

// Detailed snapshots the sender-wide state and every uplink. Safe to call at
// any time (including before Run) and never blocks meaningfully.
func (s *Sender) Detailed() Detailed {
	now := s.now()
	ups, _ := s.snap()
	out := Detailed{
		Mode:         s.mode(),
		ActiveConns:  s.ActiveConnections(),
		HasConnected: s.HasConnected(),
		Failover:     s.Failover(),
		Uplinks:      make([]UplinkStat, 0, len(ups)),
	}
	for _, u := range ups {
		ti := u.Telemetry()
		age := 0.0
		if la := u.LastActivity(); !la.IsZero() {
			age = float64(now.Sub(la).Milliseconds())
		}
		out.Uplinks = append(out.Uplinks, UplinkStat{
			ID:             u.ID,
			IP:             u.IP,
			RTTMs:          float64(ti.RTTMs),
			InFlight:       int(ti.InFlight),
			Window:         int(ti.Window),
			Weight:         u.Weight(),
			Established:    u.Established(),
			NAKBurst:       u.NAKBurst(),
			LastActivityMs: age,
		})
	}
	return out
}

// summary aggregates the live sender state into the shared sinks.Stats shape
// the balancer consumes. Safe to call at any time (including before Run) and
// never blocks meaningfully.
func (s *Sender) summary() sinks.Stats {
	now := s.now()

	// RTT: average of the non-zero uplink RTTs.
	var rttSum float64
	rttN := 0
	var inFlightSum int
	ups, _ := s.snap()
	for _, u := range ups {
		if r := float64(u.RTTMs()); r > 0 {
			rttSum += r
			rttN++
		}
		inFlightSum += u.InFlight()
	}
	rtt := 0.0
	if rttN > 0 {
		rtt = rttSum / float64(rttN)
	}

	// Loss: NAKs as a fraction of all feedback (NAK vs ACK) so far.
	na := s.nakTotal.Load()
	ak := s.ackTotal.Load()
	lossPct := 0.0
	if na+ak > 0 {
		lossPct = 100.0 * float64(na) / float64(na+ak)
	}

	// Send + retransmit rates from EWMA meters over the cumulative counters.
	sendBps := s.sendMeter.Sample(now, float64(s.sentBytes.Load()))
	nakPerSec := s.nakMeter.Sample(now, float64(na))
	sendMbps := sendBps * 8 / 1e6

	// Bandwidth estimate: the smoothed send rate when a path is alive (0
	// when no RTT is measured, mirroring the balancer's "no valid sample"
	// gate).
	bandwidthMbps := 0.0
	if rtt > 0 {
		bandwidthMbps = sendMbps
	}

	return sinks.Stats{
		RTT:                  rtt,
		PacketLossRate:       lossPct,
		SendRateMbps:         sendMbps,
		BandwidthEstimate:    bandwidthMbps,
		RetransmitRate:       nakPerSec,
		PktSndLossTotal:      na,
		PktRetransTotal:      na,
		SendBufferBytes:      inFlightSum * SRTMaxPayload,
		ACKTotal:             ak,
		PacketsSentTotal:     s.sentPkts.Load(),
		PacketsReceivedTotal: 0,
		TimestampMs:          uint64(now.UnixMilli()),
	}
}
