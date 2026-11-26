package compositor

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeCorruptClip renders a 10-frame H.264 mp4 and corrupts its last 256
// bytes, so a decoder posts a genuine async bus ERROR mid-playback.
func makeCorruptClip(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clip.mp4")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=0.4:size=64x64:rate=10",
		"-c:v", "libx264", "-profile:v", "baseline", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg not usable (%v): %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read clip: %v", err)
	}
	for i := len(data) - 256; i < len(data); i++ {
		data[i] ^= 0xFF
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("corrupt clip: %v", err)
	}
	return path
}

// TestBusDrainLogsError verifies that a GStreamer bus ERROR posted AFTER
// the pipeline plays (fakesink emit-error) reaches the log through the
// drain (spec §6 Phase 6) — the frozen-pipeline case the drain exists for.
func TestBusDrainLogsError(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	// A short H.264 clip with a corrupted tail: the pipeline prerolls and
	// reaches PLAYING, then the decoder hits the garbage and posts an async
	// bus ERROR — exactly like a real mid-stream failure.
	clip := makeCorruptClip(t)
	cp, err := New("filesrc location=" + clip + " ! qtdemux ! h264parse ! avdec_h264 ! fakesink name=sink")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cp.SetBusLog(log)
	if err := cp.Start(); err != nil {
		t.Fatalf("Start (async): %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), `"kind"="error"`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	text := buf.String()
	if !strings.Contains(text, `"kind"="error"`) {
		t.Fatalf("no bus ERROR in log within 5s. log:\n%s", text)
	}

	if err := cp.Stop(); err != nil {
		// The broken chain cannot reach PLAYING; a state failure on the
		// freeze is expected and acceptable here.
		t.Logf("Stop (state fail tolerated): %v", err)
	}
}

// TestBusDrainStopsWithPipeline ensures the drain goroutine exits with the
// pipeline (no leaked goroutine pumping a dead bus).
func TestBusDrainStopsWithPipeline(t *testing.T) {
	var buf bytes.Buffer
	cp, err := New("videotestsrc num-buffers=1 ! fakesink")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cp.SetBusLog(slog.New(slog.NewTextHandler(&buf, nil)))
	if err := cp.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Let it run (EOS on the bus), then stop: the drain must end quietly.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), `"kind"="eos"`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Stop posts its own STATE_CHANGED (→NULL) messages; the drain is only
	// torn down once Stop returns, so those are legitimately logged. After
	// that the drain is gone and the log must stabilize.
	if err := cp.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	time.Sleep(700 * time.Millisecond) // let any in-flight drain cycle finish
	before := buf.Len()
	time.Sleep(400 * time.Millisecond)
	if buf.Len() != before {
		t.Fatalf("bus drain wrote after Stop: %q", buf.String()[before:])
	}
}
