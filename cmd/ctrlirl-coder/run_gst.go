// GStreamer runtime: builds the pipeline, applies the initial scene, runs the
// controller, and serves SIGHUP (in-place reload, §7.8) / SIGINT / SIGTERM
// (graceful shutdown).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ctrlirl-coder/internal/compositor"
	"ctrlirl-coder/internal/config"
	"ctrlirl-coder/internal/controller"
	"ctrlirl-coder/internal/gstpull"
	"ctrlirl-coder/internal/metrics"
	"ctrlirl-coder/internal/rpc"
	"ctrlirl-coder/internal/scene"
	"ctrlirl-coder/internal/server"
	"ctrlirl-coder/internal/sinks"
	"ctrlirl-coder/internal/sinks/srt"
	"ctrlirl-coder/internal/sinks/srtla"
)

func run(o *options) error {
	// Config, pipeline text and the initial scene are each loaded exactly
	// once here; --dry-run validates the same text and stops before any
	// GStreamer work (bug H: the contract is checked whether the template
	// comes from --pipeline or [pipeline] template).
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}
	slog.Info("dry-run: config OK",
		"balancer", cfg.General.Balancer,
		"min_kbps", cfg.General.MinBitrateKbps,
		"max_kbps", cfg.General.MaxBitrateKbps,
		"srt_uri", cfg.SRT.URI)

	pipelineText, err := readPipelineText(o, cfg)
	if err != nil {
		return err
	}
	// Bug H: the contract is validated for the template no matter where it
	// came from (--pipeline or [pipeline] template), not just in dry-run.
	if err := validatePipelineContract(pipelineText); err != nil {
		return err
	}
	if o.dryRun || o.pipeline != "" {
		_, present := pipelineContract(pipelineText)
		slog.Info("dry-run: pipeline OK", "elements", strings.Join(present, " "))
	}

	var curScene *scene.Scene
	if o.scenePath != "" {
		curScene, err = scene.Load(o.scenePath)
		if err != nil {
			return err
		}
		slog.Info("dry-run: scene OK", "name", curScene.Name, "layers", len(curScene.Layers))
	}

	if o.dryRun {
		fmt.Println("dry-run: OK — configuration, scene and pipeline contract are valid")
		return nil
	}

	cp, err := compositor.New(pipelineText)
	if err != nil {
		return fmt.Errorf("pipeline: %w", err)
	}
	var (
		client    sinks.Sink
		puller    *gstpull.Puller
		startSink func(context.Context)
	)
	ctx, cancel := context.WithCancel(context.Background())

	// Single ordered teardown on every exit path. The order is critical:
	//
	//  1. cancel the data path (stops the ring consumer),
	//  2. puller.Stop() so a streaming-thread handoff blocked on a now-full
	//     ring drops and returns instead of blocking — the freeze below needs
	//     that thread to advance,
	//  3. cp.Stop() freeze the pipeline (SetState NULL stops the streaming
	//     thread),
	//  4. puller.Detach() unref the tap element — only safe AFTER the freeze,
	//     since unrefing a live element while PLAYING deadlocks,
	//  5. client.Close() last, so no in-flight write races the unref.
	defer func() {
		cancel()
		if puller != nil {
			puller.Stop()
		}
		if err := cp.Stop(); err != nil {
			slog.Error("pipeline stop", "err", err)
		}
		if puller != nil {
			puller.Detach()
		}
		if client != nil {
			client.Close()
		}
	}()

	if err := attachScene(cp, curScene); err != nil {
		return err
	}

	client, puller, startSink, err = startDataPath(cp, cfg)
	if err != nil {
		return err
	}

	if err := cp.Start(); err != nil {
		return fmt.Errorf("pipeline start: %w", err)
	}
	slog.Info("pipeline playing")

	if puller != nil {
		go startSink(ctx)                // srt: connect w/ backoff; srtla: aggregate uplinks
		go puller.Run(ctx, client.Write) // pop bytes → sink; 1×-paced, backpressure on VOD
		slog.Info("sink: data path running", "sink", cfg.Sink)
		go logAppsinkStats(ctx, puller)
		go logSinkStats(ctx, client)
	}

	ctl := startController(ctx, client, puller, cp, cfg)

	// Pace the appsink drain at the adaptive target so VOD sources (HLS
	// EXT-X-ENDLIST, local file) play at 1× wall clock instead of draining
	// at network speed and hitting EOS. Live sources are self-pacing, so
	// the pacer is a no-op for them. Wired after the controller exists
	// because the target is the balancer's live state.
	if puller != nil {
		puller.SetPaceRateSupplier(func() int64 { return int64(ctl.CurBitrate()) })
	}

	apiSrv, rpcCtl, err := startAPI(ctx, o, cfg, cp, curScene, client, puller, ctl)
	if err != nil {
		return err
	}
	if apiSrv != nil {
		defer apiSrv.Stop()
	}

	sinkReload := makeSinkReload(client, cfg)

	return signalLoop(ctx, o, cfg, ctl, rpcCtl, cp, sinkReload)
}

// makeSinkReload returns a closure that re-applies a reloaded config to the
// output sink, or nil when the sink does not support it (direct SRT has no
// reloadable state). Used by SIGHUP to swap the SRTLA uplink set without
// restarting the pipeline.
func makeSinkReload(client sinks.Sink, cfg *config.Config) func(*config.Config) error {
	if cfg.Sink != config.SinkSRTLA {
		return nil
	}
	r, ok := client.(interface {
		Reload(srtla.Config) error
	})
	if !ok {
		return nil
	}
	return func(nx *config.Config) error {
		if nx.Sink != config.SinkSRTLA {
			return nil
		}
		return r.Reload(nx.SRTLA.ToSRTLAConfig())
	}
}

// attachScene applies every layer of the initial scene to the compositor.
func attachScene(cp *compositor.CompositorPipeline, sc *scene.Scene) error {
	if sc == nil {
		return nil
	}
	if !cp.HasCompositor() {
		slog.Warn("scene: pipeline has no compositor element; scene layers will be ignored")
		return nil
	}
	for i := range sc.Layers {
		l := sc.Layers[i]
		if err := cp.AddLayer(context.Background(), l); err != nil {
			return fmt.Errorf("scene: attach %s: %w", l.ID, err)
		}
		slog.Info("scene: layer attached", "id", l.ID, "type", l.Type, "filters", len(l.Filters))
	}
	return nil
}

// startDataPath builds the configured output sink (srt or srtla, chosen by the
// top-level `sink` selector), attaches the appsink puller, and returns a
// startSink closure that launches the sink's background loop. Both happen
// BEFORE the pipeline reaches PLAYING — the first pull must not race the
// streaming thread. We send the encoded bytes ourselves (no in-pipeline sink).
//
// startSink blocks for the life of ctx (srt: connect-with-backoff +
// housekeeping; srtla: uplink read loops + housekeeping) and is meant to run
// in its own goroutine.
func startDataPath(cp *compositor.CompositorPipeline, cfg *config.Config) (sinks.Sink, *gstpull.Puller, func(context.Context), error) {
	var (
		sk    sinks.Sink
		start func(context.Context)
	)
	// Both sinks drive a real SRT session, so verify the CERALIVE fork is the
	// mapped SRT backend before building either. Fails fast on a stock-libsrt
	// (bare `go build`) binary.
	if err := srt.EnsureSRTBackend(); err != nil {
		return nil, nil, nil, err
	}
	switch cfg.Sink {
	case config.SinkSRTLA:
		sender, err := newSRTLASink(cfg, slog.Default())
		if err != nil {
			return nil, nil, nil, fmt.Errorf("srtla: %w", err)
		}
		sk = sender
		start = func(ctx context.Context) {
			if err := sender.Run(ctx); err != nil {
				slog.Error("srtla: data path stopped", "err", err)
			}
		}
	default: // config.SinkSRT
		client, err := srt.NewClient(srt.ClientConfig{
			URI:              cfg.SRT.URI,
			LatencyMs:        cfg.SRT.LatencyMs,
			StreamID:         cfg.SRT.StreamID,
			Passphrase:       cfg.SRT.Passphrase,
			MaxBandwidthKbps: cfg.SRT.MaxBandwidthKbps,
		}, slog.Default())
		if err != nil {
			return nil, nil, nil, fmt.Errorf("srt: %w", err)
		}
		sk = client
		start = func(ctx context.Context) {
			if err := client.Start(ctx); err != nil {
				slog.Error("srt: data path stopped", "err", err)
			}
		}
	}

	var puller *gstpull.Puller
	if cp.HasTap() {
		tap := cp.TapElement()
		puller = gstpull.Attach(tap, uint64(cfg.General.FrameStallMS))
		slog.Info("sink: appsink puller attached", "sink", cfg.Sink, "tap", "appsink", "stall_ms", cfg.General.FrameStallMS)
	} else {
		slog.Error("sink: pipeline has no terminal appsink; output disabled")
	}
	return sk, puller, start, nil
}

// startAPI starts the shared HTTP API server: the JSON-RPC 2.0 control
// endpoint (POST /rpc) and the /metrics endpoint (GET, Prometheus text
// exposition format) under one listener — the --api-addr flag wins over
// [control] addr. The controller owns the scene state and is returned even
// when no address is configured, because SIGHUP reloads go through it.
// Returns a nil server when disabled.
func startAPI(ctx context.Context, o *options, cfg *config.Config, cp *compositor.CompositorPipeline, curScene *scene.Scene, sink sinks.StatsSource, puller *gstpull.Puller, ctl *controller.Controller) (*server.Server, *rpc.Controller, error) {
	rpcCtl := rpc.New(cp, curScene, slog.Default())
	addr := cfg.Control.Addr
	if o.apiAddr != "" {
		addr = o.apiAddr
	}
	if addr == "" {
		return nil, rpcCtl, nil
	}
	srv, err := server.New(server.Config{
		Addr: addr,
	}, map[string]http.Handler{
		"/rpc": rpc.NewServer(rpcCtl),
		"/metrics": metrics.Handler(func() []metrics.Point {
			pts := make([]metrics.Point, 0, 24)
			if st, err := sink.Stats(); err == nil {
				pts = append(pts,
					metrics.Point{Name: "sink_rtt_ms", Value: st.RTT},
					metrics.Point{Name: "sink_loss_pct", Value: st.PacketLossRate},
					metrics.Point{Name: "sink_send_mbps", Value: st.SendRateMbps},
					metrics.Point{Name: "sink_retrans_pps", Value: st.RetransmitRate},
					metrics.Point{Name: "sink_send_buffer_bytes", Value: float64(st.SendBufferBytes)},
					metrics.Point{Name: "sink_acks_total", Value: float64(st.ACKTotal), Type: metrics.TypeCounter},
					metrics.Point{Name: "sink_pkts_total", Value: float64(st.PacketsSentTotal), Type: metrics.TypeCounter},
				)
			}
			if puller != nil {
				pts = append(pts,
					metrics.Point{Name: "frames", Value: float64(puller.Frames()), Type: metrics.TypeCounter},
					metrics.Point{Name: "frames_dropped", Value: float64(puller.Dropped()), Type: metrics.TypeCounter},
					metrics.Point{Name: "bytes_sent", Value: float64(puller.BytesSent()), Type: metrics.TypeCounter},
					metrics.Point{Name: "encoder_kbps", Value: float64(puller.ThroughputKbps())},
					metrics.Point{Name: "last_frame_age_ms", Value: puller.LastFrameAge().Seconds() * 1000},
				)
			}
			if ctl != nil {
				rttMin, rttMax := ctl.RTTRange()
				pts = append(pts,
					metrics.Point{Name: "encoder_target_kbps", Value: float64(ctl.CurBitrate()) / 1000},
					metrics.Point{Name: "encoder_min_kbps", Value: float64(cfg.General.MinBitrateKbps)},
					metrics.Point{Name: "encoder_max_kbps", Value: float64(cfg.General.MaxBitrateKbps)},
					metrics.Point{Name: "encoder_rtt_min_ms", Value: rttMin},
					metrics.Point{Name: "encoder_rtt_max_ms", Value: rttMax},
				)
			}
			if d, ok := sink.(srtla.DetailedProvider); ok {
				det := d.Detailed()
				pts = append(pts,
					metrics.Point{Name: "srtla_uplinks_total", Value: float64(len(det.Uplinks))},
					metrics.Point{Name: "srtla_uplinks_active", Value: float64(det.ActiveConns)},
					metrics.Point{Name: "srtla_connected", Value: metrics.Bool(det.HasConnected)},
					metrics.Point{Name: "srtla_failover", Value: metrics.Bool(det.Failover)},
					metrics.Point{Name: "srtla_mode_enhanced", Value: metrics.Bool(det.Mode == srtla.ModeEnhanced)},
				)
				for _, u := range det.Uplinks {
					prefix := fmt.Sprintf("srtla_uplink_%d", u.ID)
					pts = append(pts,
						metrics.Point{Name: prefix + "_rtt_ms", Value: u.RTTMs},
						metrics.Point{Name: prefix + "_inflight", Value: float64(u.InFlight)},
						metrics.Point{Name: prefix + "_window", Value: float64(u.Window)},
						metrics.Point{Name: prefix + "_weight", Value: u.Weight},
						metrics.Point{Name: prefix + "_up", Value: metrics.Bool(u.Established)},
						metrics.Point{Name: prefix + "_nak_burst", Value: float64(u.NAKBurst)},
						metrics.Point{Name: prefix + "_last_activity_ms", Value: u.LastActivityMs},
					)
				}
			}
			return pts
		}),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("api: %w", err)
	}
	srv.Start(ctx)
	slog.Info("api: HTTP server listening", "addr", srv.Addr())
	return srv, rpcCtl, nil
}

// logSinkStats logs a sink statistics sample every 5 s — the structured
// stats view (rtt, loss, send rate, retransmits, send buffer) that keeps a
// silent degradation visible in the log (spec §6 Phase 6).
func logSinkStats(ctx context.Context, src sinks.StatsSource) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		st, err := src.Stats()
		if err != nil {
			slog.Warn("sink stats: read failed", "err", err)
			continue
		}
		slog.Info("sink stats",
			"rtt_ms", st.RTT,
			"loss_pct", st.PacketLossRate,
			"send_mbps", st.SendRateMbps,
			"retrans_pps", st.RetransmitRate,
			"send_buffer_bytes", st.SendBufferBytes,
			"acks", st.ACKTotal,
			"pkts", st.PacketsSentTotal,
		)
	}
}

// logAppsinkStats logs the appsink pull counters at 1 Hz so a wedged
// pipeline (frames vs dropped vs last-frame age) is visible without a
// receiver.
func logAppsinkStats(ctx context.Context, puller *gstpull.Puller) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			slog.Info("appsink diag",
				"frames", puller.Frames(),
				"dropped", puller.Dropped(),
				"bytes_sent", puller.BytesSent(),
				"last_frame_age_ms", puller.LastFrameAge().Milliseconds(),
				"encoded_kbps", puller.ThroughputKbps(),
			)
		}
	}
}

// startController starts the adaptive controller in its own goroutine. The
// appsink is the original's condition: no appsink or no encoder, no dynamic
// bitrate (the housekeeping timer only ran when the sink was present).
func startController(ctx context.Context, src sinks.StatsSource, puller *gstpull.Puller, cp *compositor.CompositorPipeline, cfg *config.Config) *controller.Controller {
	if puller == nil || !cp.HasEncoder() {
		slog.Warn("controller: no appsink or encoder; adaptive loop disabled")
		return nil
	}
	ctl := controller.New(src, puller, cp, cfg)
	go func() {
		if err := ctl.Run(ctx); err != nil {
			slog.Error("controller", "err", err)
		}
	}()
	return ctl
}

// signalLoop serves SIGHUP (in-place reload, §7.8) until SIGINT/SIGTERM
// (graceful stop; the caller's deferred teardown runs the ordered shutdown).
func signalLoop(ctx context.Context, o *options, cfg *config.Config, ctl *controller.Controller, rpcCtl *rpc.Controller, cp *compositor.CompositorPipeline, sinkReload func(*config.Config) error) error {
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	for sig := range sigCh {
		switch sig {
		case syscall.SIGHUP:
			cfg = reload(ctx, o, cfg, ctl, rpcCtl, cp, sinkReload)
		case syscall.SIGINT, syscall.SIGTERM:
			slog.Info("signal received; shutting down", "signal", sig)
			return nil
		}
	}
	return nil
}

// reload re-reads the config and the scene on SIGHUP and returns the
// (possibly updated) config. File reload is authoritative: the file's layers
// replace the controller's current scene (RPC mutations not in the file are
// removed; a failed op rolls the reload back).
func reload(ctx context.Context, o *options, cfg *config.Config, ctl *controller.Controller, rpcCtl *rpc.Controller, cp *compositor.CompositorPipeline, sinkReload func(*config.Config) error) *config.Config {
	slog.Info("SIGHUP: reloading config and scene")
	nx, err := config.Load(o.configPath)
	if err != nil {
		slog.Error("config reload failed; keeping previous", "err", err)
		return cfg
	}
	cfg = nx
	if ctl != nil {
		ctl.UpdateConfig(cfg)
	}
	if sinkReload != nil {
		if err := sinkReload(cfg); err != nil {
			slog.Error("sink reload failed", "sink", cfg.Sink, "err", err)
		}
	}
	if o.scenePath != "" && cp.HasCompositor() {
		newScene, err := scene.Load(o.scenePath)
		if err != nil {
			slog.Error("scene reload: load", "err", err)
			return cfg
		}
		ops, err := rpcCtl.Replace(ctx, newScene)
		if err != nil {
			slog.Error("scene reload: apply rolled back", "err", err, "applied", len(ops))
		} else {
			slog.Info("scene reload: applied", "layers", len(newScene.Layers), "ops", len(ops))
		}
	}
	return cfg
}

// readPipelineText resolves the launch string: --pipeline file wins, then the
// [pipeline] template from the config.
func readPipelineText(o *options, cfg *config.Config) (string, error) {
	path := o.pipeline
	if path == "" {
		path = cfg.Pipeline.Template
	}
	if path == "" {
		return "", fmt.Errorf("no pipeline: pass --pipeline or set [pipeline] template in the config")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("pipeline: %w", err)
	}
	return string(data), nil
}
