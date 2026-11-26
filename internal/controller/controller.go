// Package controller is the pure-Go control loop that sits on top of the
// pipeline. It reads SRT statistics from a sinks.StatsSource, drives the
// bitrate balancer, and pushes bitrate and overlay updates to the pipeline
// through a sinks.Applier.
//
// It deliberately has no GStreamer dependency: in production the applier is
// the compositor (which serializes the mutations), in tests it is a fake.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"ctrlirl-coder/internal/balancer"
	"ctrlirl-coder/internal/config"
	"ctrlirl-coder/internal/sinks"
)

// controlTick is the fixed balancer poll interval (spec §5.8). Increase/
// decrease pacing is internal to each balancer (via Stats.TimestampMs), so
// the controller only needs one fixed tick.
const controlTick = 500 * time.Millisecond

// Liveness is the appsink frame-production signal (the original
// frame_liveness): Stalled reports no pulled frame within the threshold.
// The controller uses it as a hard gate — a zombie-encode (pipeline wedged,
// no frames through the appsink) drops the target to the minimum immediately,
// exactly where the original gated the systemd watchdog ping.
type Liveness interface {
	Stalled() bool
}

// Controller runs the balancer and overlay loops until ctx is cancelled.
type Controller struct {
	src      sinks.StatsSource
	liveness Liveness // may be nil (no appsink → no stall gate)
	applier  sinks.Applier
	bal      balancer.Balancer

	mu      sync.RWMutex
	params  balancer.Params
	overlay config.Overlay

	curBPS atomic.Int64 // current target bitrate, shared with the overlay loop
	rttMin atomic.Int64 // float64 bits of the running minimum RTT (0 = none)
	rttMax atomic.Int64 // float64 bits of the running maximum RTT (0 = none)
}

// New builds a controller. curBPS starts at the configured maximum (the
// adaptive algorithm begins optimistic, matching the original). liveness is
// the appsink puller (nil disables the stall gate).
func New(src sinks.StatsSource, liveness Liveness, ap sinks.Applier, cfg *config.Config) *Controller {
	p := cfg.BalancerParams(cfg.SRT.LatencyMs, 0) // pkt 0 → balancer default (1316)
	c := &Controller{
		src:      src,
		liveness: liveness,
		applier:  ap,
		bal:      balancer.New(cfg.General.Balancer, p),
		params:   p,
		overlay:  cfg.Overlay,
	}
	c.curBPS.Store(int64(p.MaxBitrate))
	return c
}

// CurBitrate returns the current target bitrate in bps.
func (c *Controller) CurBitrate() int { return int(c.curBPS.Load()) }

// RTTRange returns the rolling minimum and maximum RTT (ms) measured since
// controller start; (0, 0) when no valid sample has been seen yet.
func (c *Controller) RTTRange() (min, max float64) {
	return math.Float64frombits(uint64(c.rttMin.Load())), math.Float64frombits(uint64(c.rttMax.Load()))
}

// UpdateConfig swaps in new balancer/overlay settings (SIGHUP reloads).
// The balancer's live limits propagate on the next Next() call; a change to
// the overlay *interval* takes effect on the next controller start.
func (c *Controller) UpdateConfig(cfg *config.Config) {
	c.mu.Lock()
	c.params = cfg.BalancerParams(cfg.SRT.LatencyMs, 0)
	c.overlay = cfg.Overlay
	c.mu.Unlock()
}

func (c *Controller) snapshot() (balancer.Params, config.Overlay) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.params, c.overlay
}

// Run starts both loops and blocks until ctx is cancelled, then waits for
// the loops to drain.
func (c *Controller) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.balancerLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		c.overlayLoop(ctx)
	}()
	<-ctx.Done()
	wg.Wait()
	return nil
}

// balancerLoop polls the stats source on controlTick and applies the
// balancer's decision. Samples with IsValid()==false (RTT <= 0 — before the
// SRT handshake settles) are skipped so the bitrate is not adjusted on a
// cold link (spec §5.8). The balancer is stateful and owned exclusively by
// this goroutine.
func (c *Controller) balancerLoop(ctx context.Context) {
	t := time.NewTicker(controlTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s, err := c.src.Stats()
			if err != nil || !s.IsValid() {
				continue
			}
			c.trackRTT(s)
			p, _ := c.snapshot()
			next := c.bal.Next(c.CurBitrate(), s, p)
			// Appsink stall gate: frame production through the appsink is the
			// only signal that the encoder is alive. A stalled appsink (no
			// frames within the threshold) wedges nothing but proves the
			// encode is dead — drop to minimum so the encoder idles while the
			// supervisor acts (the original withheld the watchdog ping here).
			if c.liveness != nil && c.liveness.Stalled() {
				slog.Error("balancer: frame production stalled (appsink); dropping target to minimum",
					"min_kbps", p.MinBitrate/1000)
				next = p.MinBitrate
			}
			slog.Info("balancer", "target_kbps", next/1000, "rtt_ms", s.RTT, "sndbuf", s.SendBufferBytes, "loss_pct", s.PacketLossRate, "retrans", s.PktRetransTotal)
			c.curBPS.Store(int64(next))
			// A failed SetBitrate (e.g. the pipeline is stopping) is not
			// fatal; the decision is re-applied on the next tick.
			_ = c.applier.SetBitrate(next)
		}
	}
}

// overlayLoop renders the stats text at the configured interval
// (default 1 Hz, spec §5.8 / §2.1).
func (c *Controller) overlayLoop(ctx context.Context) {
	_, ov := c.snapshot()
	if !ov.Enabled {
		<-ctx.Done()
		return
	}
	interval := time.Duration(ov.IntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, ov := c.snapshot()
			if !ov.Enabled {
				continue
			}
			s, err := c.src.Stats()
			if err != nil {
				// The link may be down (ErrNotConnected between
				// reconnects) or failing transiently: keep the overlay
				// alive with the placeholder line; only ctx ends the loop.
				s = sinks.Stats{}
			}
			c.applier.SetOverlayText(renderOverlay(c, s, ov.Format))
		}
	}
}

func (c *Controller) trackRTT(s sinks.Stats) {
	// Rolling min/max since controller start (reset on controller restart).
	// Single writer (the balancer goroutine) — plain loads/stores, no CAS.
	bits := math.Float64bits(s.RTT)
	if old := c.rttMin.Load(); old == 0 || math.Float64frombits(uint64(old)) > s.RTT {
		c.rttMin.Store(int64(bits))
	}
	if old := c.rttMax.Load(); math.Float64frombits(uint64(old)) < s.RTT {
		c.rttMax.Store(int64(bits))
	}
}

// renderOverlay formats one overlay line. Layout matches the original
// overlay_ui.c (config DefaultOverlayFormat):
//
//	b: <set_kbps>/<throughput_mbps> rtt: <rtt>/<min>/<max> bs: <bs>/<t1>/<t2>/<t3>
//
// The bs thresholds (t1..t3) are internal balancer state and are 0 here.
func renderOverlay(c *Controller, s sinks.Stats, format string) string {
	if !s.IsValid() {
		return "  b:    --/  -- rtt:  --/ --/ -- bs:  --/ --/ --/ --"
	}
	if format == "" {
		format = config.DefaultOverlayFormat
	}
	return fmt.Sprintf(format,
		int(c.curBPS.Load())/1000,
		s.SendRateMbps,
		int(s.RTT), int(math.Float64frombits(uint64(c.rttMin.Load()))), int(math.Float64frombits(uint64(c.rttMax.Load()))),
		s.SendBufferBytes, 0, 0, 0,
	)
}
