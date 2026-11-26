package srtla

// Uplink-selection modes.
const (
	// ModeClassic is the reference srtla_send behavior: per-uplink
	// window/congestion selection with no receiver telemetry awareness.
	ModeClassic = "classic"
	// ModeEnhanced is the quality-aware scheduler (default): scores uplinks
	// using receiver telemetry (RTT, loss, in-flight) from extended
	// keepalives, falling back to classic signals when telemetry is absent.
	ModeEnhanced = "enhanced"
)

// Config is the SRTLA sender configuration, as consumed by the Sender.
//
// The Sender relays complete SRT datagrams over the uplink sockets. Its caller
// owns the SRT session and supplies the feedback handler.
type Config struct {
	// ReceiverHost / ReceiverPort address the SRTLA receiver (srtla_rec).
	ReceiverHost string
	ReceiverPort int
	// UplinkIPs are the local source addresses one UDP socket binds to each.
	UplinkIPs []string
	// Mode selects the scheduler (ModeClassic or ModeEnhanced).
	Mode string
	// ConnTimeoutMs is how long an uplink may be idle before it is treated
	// as failed and re-registered.
	ConnTimeoutMs int
}

// Normalized returns a copy of cfg with defaults and the mode canonicalized
// (unknown/empty mode defaults to ModeEnhanced).
func (c Config) Normalized() Config {
	if c.Mode == "" {
		c.Mode = ModeEnhanced
	}
	if c.Mode != ModeClassic && c.Mode != ModeEnhanced {
		c.Mode = ModeEnhanced
	}
	if c.ConnTimeoutMs <= 0 {
		c.ConnTimeoutMs = 5000
	}
	return c
}
