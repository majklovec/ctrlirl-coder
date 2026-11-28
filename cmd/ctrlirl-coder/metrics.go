package main

import (
	"fmt"

	"ctrlirl-coder/internal/config"
	"ctrlirl-coder/internal/controller"
	"ctrlirl-coder/internal/gstpull"
	"ctrlirl-coder/internal/metrics"
	"ctrlirl-coder/internal/sinks"
	"ctrlirl-coder/internal/sinks/srtla"
)

// metricPoints gathers one /metrics scrape: sink statistics, appsink puller
// counters, the encoder target and limits, and the SRTLA uplink state.
// Sections whose source is absent (link down, no appsink, adaptive loop off,
// direct SRT) are left out.
func (a *app) metricPoints() []metrics.Point {
	pts := make([]metrics.Point, 0, 24)
	if st, err := a.sink.Stats(); err == nil {
		pts = append(pts, sinkPoints(st)...)
	}
	if a.puller != nil {
		pts = append(pts, pullerPoints(a.puller)...)
	}
	if a.ctl != nil {
		pts = append(pts, encoderPoints(a.ctl, a.cfg.Load())...)
	}
	if d, ok := a.sink.(srtla.DetailedProvider); ok {
		pts = append(pts, srtlaPoints(d.Detailed())...)
	}
	return pts
}

func gauge(name string, v float64) metrics.Point { return metrics.Point{Name: name, Value: v} }

func counter(name string, v float64) metrics.Point {
	return metrics.Point{Name: name, Value: v, Type: metrics.TypeCounter}
}

func sinkPoints(st sinks.Stats) []metrics.Point {
	return []metrics.Point{
		gauge("sink_rtt_ms", st.RTT),
		gauge("sink_loss_pct", st.PacketLossRate),
		gauge("sink_send_mbps", st.SendRateMbps),
		gauge("sink_retrans_pps", st.RetransmitRate),
		gauge("sink_send_buffer_bytes", float64(st.SendBufferBytes)),
		counter("sink_acks_total", float64(st.ACKTotal)),
		counter("sink_pkts_total", float64(st.PacketsSentTotal)),
	}
}

func pullerPoints(p *gstpull.Puller) []metrics.Point {
	return []metrics.Point{
		counter("frames", float64(p.Frames())),
		counter("frames_dropped", float64(p.Dropped())),
		counter("bytes_sent", float64(p.BytesSent())),
		gauge("encoder_kbps", float64(p.ThroughputKbps())),
		gauge("last_frame_age_ms", p.LastFrameAge().Seconds()*1000),
	}
}

func encoderPoints(ctl *controller.Controller, cfg *config.Config) []metrics.Point {
	rttMin, rttMax := ctl.RTTRange()
	return []metrics.Point{
		gauge("encoder_target_kbps", float64(ctl.CurBitrate())/1000),
		gauge("encoder_min_kbps", float64(cfg.General.MinBitrateKbps)),
		gauge("encoder_max_kbps", float64(cfg.General.MaxBitrateKbps)),
		gauge("encoder_rtt_min_ms", rttMin),
		gauge("encoder_rtt_max_ms", rttMax),
	}
}

func srtlaPoints(det srtla.Detailed) []metrics.Point {
	pts := []metrics.Point{
		gauge("srtla_uplinks_total", float64(len(det.Uplinks))),
		gauge("srtla_uplinks_active", float64(det.ActiveConns)),
		gauge("srtla_connected", metrics.Bool(det.HasConnected)),
		gauge("srtla_failover", metrics.Bool(det.Failover)),
		gauge("srtla_mode_enhanced", metrics.Bool(det.Mode == srtla.ModeEnhanced)),
	}
	for _, u := range det.Uplinks {
		prefix := fmt.Sprintf("srtla_uplink_%d", u.ID)
		pts = append(pts,
			gauge(prefix+"_rtt_ms", u.RTTMs),
			gauge(prefix+"_inflight", float64(u.InFlight)),
			gauge(prefix+"_window", float64(u.Window)),
			gauge(prefix+"_weight", u.Weight),
			gauge(prefix+"_up", metrics.Bool(u.Established)),
			gauge(prefix+"_nak_burst", float64(u.NAKBurst)),
			gauge(prefix+"_last_activity_ms", u.LastActivityMs),
		)
	}
	return pts
}
