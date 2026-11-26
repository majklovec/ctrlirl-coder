package browser

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// pulseVirtualSink creates a Pulse/PipeWire null sink via pactl and returns a
// closer that unloads it. The sink's monitor (<name>.monitor) is what the
// audio chain reads. A missing pactl (no user audio session) is an error: the
// browser layer then falls back to its test-tone audio.
//
// pactl load-module prints the module ID ("N: module-null-sink"); the closer
// must unload by that ID. Unloading by sink name fails ("Access denied" on
// PipeWire) and every failed attempt left the module registered, so repeated
// rebuilds accumulated duplicate same-named sinks — Pulse then resolved
// `device=<name>.monitor` and Chrome's sink binding to arbitrary duplicates,
// cross-leaking both layers' audio into each other's audio chain.
func pulseVirtualSink(name string) (cleanup func(), err error) {
	out, err := exec.Command("pactl", "load-module", "module-null-sink",
		"sink_name="+name, "sink_property=pulse.driver=ctrlirl").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("layer: pactl load-module module-null-sink: %v: %s", err, strings.TrimSpace(string(out)))
	}
	modID := moduleIDFromLoadOutput(string(out))
	cleanup = func() {
		_ = exec.Command("pactl", "unload-module", modID).Run()
	}
	// The module loads asynchronously; wait for the monitor source to be visible.
	if err := waitForPulseMonitorSource(name+".monitor", 5*time.Second); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

// moduleIDFromLoadOutput parses the module ID from `pactl load-module`
// output, which is the bare numeric ID on a single line ("536871142").
// Returns "0" on parse failure; unloading a non-existent module is a no-op,
// which is the right fallback.
func moduleIDFromLoadOutput(out string) string {
	id := strings.TrimSpace(out)
	if _, err := strconv.Atoi(id); err == nil {
		return id
	}
	return "0"
}

// waitForPulseMonitorSource polls `pactl list sources` for the sink's monitor
// source. A Pulse/PipeWire null sink's monitor (<name>.monitor) is a SOURCE,
// not a sink, so it is only visible in the sources list (a SUSPENDED monitor
// does not appear in `pactl list short sinks`).
func waitForPulseMonitorSource(device string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("pactl", "list", "sources").Output()
		if err == nil && strings.Contains(string(out), device) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("layer: pulse monitor source %q not found", device)
}

// findSinkInputID finds the Pulse sink-input id of a process group whose
// comm (or cmdline) contains `needle`, on sink `sink`. Returns "" when absent.
// (Chrome ignores --audio-sink when it starts on the default card, so the
// layer re-routes its stream here on PipeWire-Pulse.)
func findSinkInputID(needle, sink string) string {
	out, err := exec.Command("pactl", "list", "short", "sink-inputs").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if fields[3] != sink {
			continue
		}
		rest := strings.Join(fields[4:], " ")
		if strings.Contains(rest, needle) {
			return fields[0]
		}
	}
	return ""
}

// routeSinkInput moves a sink-input onto a sink (pactl move-sink-input).
func routeSinkInput(inputID, sink string) error {
	if inputID == "" {
		return fmt.Errorf("layer: no sink-input to route")
	}
	out, err := exec.Command("pactl", "move-sink-input", inputID, sink).CombinedOutput()
	if err != nil {
		return fmt.Errorf("layer: pactl move-sink-input: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
