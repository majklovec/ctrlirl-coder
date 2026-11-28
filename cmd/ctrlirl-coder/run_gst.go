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
	"sync/atomic"
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

// app is the running encoder: what run wires together, and what a SIGHUP
// reload and the API need to reach afterwards.
type app struct {
	opts   *options
	cfg    atomic.Pointer[config.Config] // swapped by a SIGHUP reload, read by /metrics
	cp     *compositor.CompositorPipeline
	sink   sinks.Sink
	puller *gstpull.Puller        // nil: the pipeline has no terminal appsink
	ctl    *controller.Controller // nil: adaptive loop disabled
	rpc    *rpc.Controller        // owns the running scene
}

func run(o *options) error {
	// Config, pipeline text and the initial scene are each loaded exactly
	// once here; --dry-run validates the same inputs and stops before any
	// GStreamer work.
	stage := "startup: "
	if o.dryRun {
		stage = "dry-run: "
	}
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}
	slog.Info(stage+"config OK",
		"balancer", cfg.General.Balancer,
		"min_kbps", cfg.General.MinBitrateKbps,
		"max_kbps", cfg.General.MaxBitrateKbps,
		"srt_uri", cfg.SRT.URI)

	// The contract is validated for the template no matter where it came
	// from (--pipeline or [pipeline] template), dry-run or not.
	pipelineText, err := readPipelineText(o, cfg)
	if err != nil {
		return err
	}
	missing, present := pipelineContract(pipelineText)
	if len(missing) > 0 {
		return fmt.Errorf("pipeline contract: missing mandatory element(s): %s", strings.Join(missing, ", "))
	}
	slog.Info(stage+"pipeline OK", "elements", strings.Join(present, " "))

	var initial *scene.Scene
	if o.scenePath != "" {
		if initial, err = scene.Load(o.scenePath); err != nil {
			return err
		}
		slog.Info(stage+"scene OK", "name", initial.Name, "layers", len(initial.Layers))
	}

	if o.dryRun {
		fmt.Println("dry-run: OK — configuration, scene and pipeline contract are valid")
		return nil
	}

	cp, err := compositor.New(pipelineText)
	if err != nil {
		return fmt.Errorf("pipeline: %w", err)
	}
	a := &app{opts: o, cp: cp}
	a.cfg.Store(cfg)
	ctx, cancel := context.WithCancel(context.Background())

	// Single ordered teardown on every exit path. The order is critical:
	//
	//  1. cancel the data path: the drain goroutine stops at its next loop
	//     turn,
	//  2. cp.Stop() freezes the pipeline (SetState NULL stops the streaming
	//     thread and makes a drain blocked in the appsink pull return),
	//  3. puller.Detach() drops the appsink reference — only AFTER the
	//     freeze, since releasing a live element while PLAYING deadlocks,
	//  4. sink.Close() last, so no in-flight write races the teardown.
	defer func() {
		cancel()
		if err := cp.Stop(); err != nil {
			slog.Error("pipeline stop", "err", err)
		}
		if a.puller != nil {
			a.puller.Detach()
		}
		if a.sink != nil {
			a.sink.Close()
		}
	}()

	if err := attachScene(cp, initial); err != nil {
		return err
	}

	var startSink func(context.Context)
	if a.sink, a.puller, startSink, err = startDataPath(cp, cfg); err != nil {
		return err
	}

	if err := cp.Start(); err != nil {
		return fmt.Errorf("pipeline start: %w", err)
	}
	slog.Info("pipeline playing")

	if a.puller != nil {
		go startSink(ctx)                  // srt: connect w/ backoff; srtla: aggregate uplinks
		go a.puller.Run(ctx, a.sink.Write) // appsink bytes → sink; 1×-paced, backpressure on VOD
		slog.Info("sink: data path running", "sink", cfg.Sink)
		go logAppsinkStats(ctx, a.puller)
		go logSinkStats(ctx, a.sink)
	}

	a.ctl = startController(ctx, a.sink, a.puller, cp, cfg)

	// Pace the appsink drain at the adaptive target so VOD sources (HLS
	// EXT-X-ENDLIST, local file) play at 1× wall clock instead of draining
	// at network speed and hitting EOS. Wired after the controller exists
	// because the target is the balancer's live state.
	if a.puller != nil && a.ctl != nil {
		a.puller.SetPaceRateSupplier(func() int64 { return int64(a.ctl.CurBitrate()) })
	}

	// The scene controller exists even without an API listener: SIGHUP
	// reloads go through it.
	a.rpc = rpc.New(cp, initial, slog.Default())
	apiSrv, err := a.startAPI(ctx)
	if err != nil {
		return err
	}
	if apiSrv != nil {
		defer apiSrv.Stop()
	}

	a.signalLoop(ctx)
	return nil
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

// srtClientConfig is the [srt] session configuration for a client dialing
// uri (the configured destination, or the local SRTLA relay).
func srtClientConfig(cfg *config.Config, uri string) srt.ClientConfig {
	return srt.ClientConfig{
		URI:              uri,
		LatencyMs:        cfg.SRT.LatencyMs,
		StreamID:         cfg.SRT.StreamID,
		Passphrase:       cfg.SRT.Passphrase,
		MaxBandwidthKbps: cfg.SRT.MaxBandwidthKbps,
	}
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
		client, err := srt.NewClient(srtClientConfig(cfg, cfg.SRT.URI), slog.Default())
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
// [control] addr. Returns a nil server when no address is configured.
func (a *app) startAPI(ctx context.Context) (*server.Server, error) {
	addr := a.cfg.Load().Control.Addr
	if a.opts.apiAddr != "" {
		addr = a.opts.apiAddr
	}
	if addr == "" {
		return nil, nil
	}
	srv, err := server.New(server.Config{Addr: addr}, map[string]http.Handler{
		"/rpc":     rpc.NewServer(a.rpc),
		"/metrics": metrics.Handler(a.metricPoints),
	})
	if err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}
	srv.Start(ctx)
	slog.Info("api: HTTP server listening", "addr", srv.Addr())
	return srv, nil
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
// (graceful stop; run's deferred teardown does the ordered shutdown).
func (a *app) signalLoop(ctx context.Context) {
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	for sig := range sigCh {
		if sig == syscall.SIGHUP {
			a.reload(ctx)
			continue
		}
		slog.Info("signal received; shutting down", "signal", sig)
		return
	}
}

// reload re-reads the config and the scene on SIGHUP. A config that fails
// to load leaves everything as it is. The scene file is authoritative: its
// layers replace the controller's current scene (RPC mutations not in the
// file are removed; a failed op rolls the reload back).
func (a *app) reload(ctx context.Context) {
	slog.Info("SIGHUP: reloading config and scene")
	cfg, err := config.Load(a.opts.configPath)
	if err != nil {
		slog.Error("config reload failed; keeping previous", "err", err)
		return
	}
	a.cfg.Store(cfg)
	if a.ctl != nil {
		a.ctl.UpdateConfig(cfg)
	}
	if err := a.reloadSink(cfg); err != nil {
		slog.Error("sink reload failed", "sink", cfg.Sink, "err", err)
	}
	if a.opts.scenePath == "" || !a.cp.HasCompositor() {
		return
	}
	next, err := scene.Load(a.opts.scenePath)
	if err != nil {
		slog.Error("scene reload: load", "err", err)
		return
	}
	ops, err := a.rpc.Replace(ctx, next)
	if err != nil {
		slog.Error("scene reload: apply rolled back", "err", err, "applied", len(ops))
		return
	}
	slog.Info("scene reload: applied", "layers", len(next.Layers), "ops", len(ops))
}

// reloadSink re-applies a reloaded config to the output sink. Only SRTLA has
// reloadable state (the uplink set is swapped without restarting the
// pipeline); the sink kind itself cannot change at runtime.
func (a *app) reloadSink(cfg *config.Config) error {
	r, ok := a.sink.(interface{ Reload(srtla.Config) error })
	if !ok || cfg.Sink != config.SinkSRTLA {
		return nil
	}
	return r.Reload(cfg.SRTLA.ToSRTLAConfig())
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
