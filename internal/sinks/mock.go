package sinks

import (
	"errors"
	"sync"
	"time"
)

// MockProfile controls the statistics the mock reports. It is a pure-Go
// stand-in for a real SRT link so the control loop and balancer can be
// exercised without GStreamer or libsrt. Tests use it to simulate congestion.
type MockProfile struct {
	RTT                   float64 // ms
	SendRateMbps          float64
	BandwidthMbps         float64
	SendBufferBytes       int
	PktLossPerInterval    int64 // cumulative increment per Stats() call
	PktRetransPerInterval int64 // cumulative increment per Stats() call
}

// DefaultMockProfile is a healthy link: low RTT, no loss, empty send buffer.
// Feeding a fixed balancer this profile drives the bitrate to its max; an
// adaptive balancer increases toward max then holds.
func DefaultMockProfile() MockProfile {
	return MockProfile{
		RTT:           20.0,
		SendRateMbps:  5.0,
		BandwidthMbps: 20.0,
	}
}

// MockStatsSource is a StatsSource that reports profile-driven statistics with
// monotonically increasing cumulative loss/retrans counters.
type MockStatsSource struct {
	mu       sync.Mutex
	profile  MockProfile
	loss     int64
	retrans  int64
	pktSent  int64
	closed   bool
	interval time.Duration
}

// NewMockStatsSource builds a mock with the default healthy profile.
func NewMockStatsSource() *MockStatsSource {
	return &MockStatsSource{profile: DefaultMockProfile(), interval: time.Second}
}

// NewMockStatsSourceWith builds a mock around a specific profile.
func NewMockStatsSourceWith(p MockProfile) *MockStatsSource {
	return &MockStatsSource{profile: p, interval: time.Second}
}

// SetProfile atomically replaces the reported statistics profile.
func (m *MockStatsSource) SetProfile(p MockProfile) {
	m.mu.Lock()
	m.profile = p
	m.mu.Unlock()
}

// Profile returns a copy of the current profile.
func (m *MockStatsSource) Profile() MockProfile {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.profile
}

// SetInterval controls how fast the mock's monotonic clock advances between
// Stats() calls. The default is one second; tests shrink it to exercise the
// AIMD pacing logic.
func (m *MockStatsSource) SetInterval(d time.Duration) {
	m.mu.Lock()
	m.interval = d
	m.mu.Unlock()
}

// Stats advances the cumulative loss/retrans counters by the profile's
// per-interval amounts and returns a sample stamped with a monotonic millisecond
// clock.
func (m *MockStatsSource) Stats() (Stats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Stats{}, ErrClosed
	}
	p := m.profile
	m.loss += p.PktLossPerInterval
	m.retrans += p.PktRetransPerInterval
	m.pktSent += int64(p.SendRateMbps * 125) // ~100kbit per Mbps per second
	return Stats{
		TimestampMs:       uint64(time.Now().UnixMilli()),
		RTT:               p.RTT,
		SendRateMbps:      p.SendRateMbps,
		BandwidthEstimate: p.BandwidthMbps,
		PktSndLossTotal:   m.loss,
		PktRetransTotal:   m.retrans,
		SendBufferBytes:   p.SendBufferBytes,
		PacketsSentTotal:  m.pktSent,
	}, nil
}

// Close marks the source closed; subsequent Stats() calls return ErrClosed.
func (m *MockStatsSource) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return nil
}

// ErrClosed is returned by a closed mock stats source.
var ErrClosed = errors.New("sink: stats source closed")
