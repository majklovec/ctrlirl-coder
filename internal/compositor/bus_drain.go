// Bus-message drain: the GStreamer bus carries element errors, warnings,
// EOS and state-change chatter. Without a consumer, errors surface only as
// a frozen pipeline (spec §6 Phase 6: "bus-message draining to the log").
// One goroutine per pipeline pumps bus.Messages and routes each message
// to the structured log by severity.
package compositor

import (
	"context"
	"log/slog"
	"strings"

	"github.com/go-gst/go-gst/pkg/gst"
)

// startBusDrain launches the bus drain and returns its cancel function.
// The drain stops when cancel is called (cp.Stop) — the Messages iterator
// exits, uninstalls the sync handler, and the goroutine returns. Message
// severity mapping: ERROR/CRITICAL → slog.Error, WARNING → slog.Warn,
// EOS/STATE_CHANGED/ASYNC_DONE → slog.Debug (chatter), everything else
// (INFO, ELEMENT, TAG, QOS, BUFFERING, ...) → slog.Info so it is visible
// at the default level without drowning the logs.
func (cp *CompositorPipeline) startBusDrain(log *slog.Logger) context.CancelFunc {
	if log == nil {
		log = slog.Default()
	}
	bus := cp.pipeline.As().GetBus()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for msg := range bus.Messages(ctx) {
			cp.logBusMessage(log, msg)
		}
	}()
	return cancel
}

// logBusMessage routes one bus message to the log. It is a pure translation
// (no GStreamer state), safe to call from the drain goroutine.
func (cp *CompositorPipeline) logBusMessage(log *slog.Logger, msg *gst.Message) {
	src := ""
	if o := msg.Source(); o != nil {
		src = o.GetName()
	}
	text := msg.String()
	// QoS messages (encoder over-load) fire at high rate; keep them noisy
	// only when the log level allows chatter.
	switch msg.Type() {
	case gst.MessageError:
		log.Error("gstreamer bus", "kind", "error", "src", src, "msg", text)
	case gst.MessageWarning:
		log.Warn("gstreamer bus", "kind", "warning", "src", src, "msg", text)
	case gst.MessageEOS, gst.MessageAsyncDone, gst.MessageElement,
		gst.MessageStateChanged, gst.MessageStateDirty,
		gst.MessageStreamStatus, gst.MessageStructureChange,
		gst.MessageDurationChanged, gst.MessageLatency,
		gst.MessageStepDone, gst.MessageClockProvide, gst.MessageClockLost,
		gst.MessageNewClock, gst.MessageRequestState, gst.MessageSegmentStart,
		gst.MessageSegmentDone, gst.MessageStreamStart, gst.MessageResetTime,
		gst.MessageHaveContext, gst.MessageNeedContext, gst.MessageStepStart:
		// Chatter: only when the operator asks for detail.
		if log.Enabled(context.Background(), slog.LevelDebug) {
			log.Debug("gstreamer bus", "kind", strings.ToLower(msg.Type().String()), "src", src, "msg", text)
		}
	case gst.MessageQos, gst.MessageInfo:
		log.Info("gstreamer bus", "kind", strings.ToLower(msg.Type().String()), "src", src, "msg", text)
	default:
		log.Info("gstreamer bus", "kind", strings.ToLower(msg.Type().String()), "src", src, "msg", text)
	}
}
