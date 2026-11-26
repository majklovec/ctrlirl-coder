// Frame renderer for the testcode layer: a human-readable timestamp, frame
// counter, 1 Hz blink square, and QR code encoding the same data.
//
// The layout is fixed (see render), so a test can crop the QR box and
// decode it with any standard reader (zbar, zxing, pyzbar):
//
//	1280x720 canvas
//	  top-left    "CTRL IRL TESTCODE" label (scale 3)
//	  below it    "YYYY-MM-DD HH:MM:SS.mmm"  (scale 5)
//	  below that  "frame 000123"             (scale 3)
//	  top-right   1 Hz blink square (40x40, lit for the first 500 ms)
//	  bottom-right QR code, white field, black modules (module scale 6,
//	                4-module quiet zone)
package testcode

import (
	"fmt"
	"time"

	"ctrlirl-coder/internal/scene"
)

// Layer surface defaults.
const (
	defaultSurfaceW = 1280
	defaultSurfaceH = 720
	defaultFPS      = 30
)

// QR geometry (canvas-relative, bottom-right anchored).
const (
	qrModuleScale = 6
	qrQuietZone   = 4 // modules
	qrMargin      = 40
)

// frame colors.
var (
	colBackground = [3]byte{12, 14, 20}
	colText       = [3]byte{255, 255, 255}
	colBlink      = [3]byte{255, 80, 60}
)

// testcodeRenderer renders frames for one testcode layer.
type testcodeRenderer struct {
	w, h, fps int
}

// newTestcodeRenderer reads the surface size/fps from the scene properties
// (width/height/framerate), defaulting to 1280x720@30.
func newTestcodeRenderer(l scene.Layer) *testcodeRenderer {
	r := &testcodeRenderer{w: defaultSurfaceW, h: defaultSurfaceH, fps: defaultFPS}
	if v, ok := l.Properties["width"]; ok && v > 0 {
		r.w = int(v)
	}
	if v, ok := l.Properties["height"]; ok && v > 0 {
		r.h = int(v)
	}
	if v, ok := l.Properties["framerate"]; ok && v > 0 {
		r.fps = int(v)
	}
	return r
}

// frameDuration is one tick of the layer's framerate.
func (r *testcodeRenderer) frameDuration() time.Duration {
	return time.Duration(uint64(time.Second) / uint64(r.fps))
}

// qrPayload is machine-readable content: UTC timestamp (ms precision) and
// frame counter, encoded as a single ASCII line.
func qrPayload(now time.Time, frame uint64) string {
	return fmt.Sprintf("CTRLIRL|%s|f=%06d", now.UTC().Format("2006-01-02T15:04:05.000Z"), frame)
}

// render produces one BGRA frame for the given frame number and clock
// reading. QR matrix is recomputed each frame because payload includes frame.
func (r *testcodeRenderer) render(now time.Time, frame uint64) []byte {
	frameBytes := make([]byte, r.w*r.h*4)

	// Label + timestamp + frame counter, top-left.
	drawRect(frameBytes, r.w, 24, 20, 740, 180, colBackground)
	drawText(frameBytes, r.w, 40, 36, 3, colText, "CTRL IRL TESTCODE")
	drawText(frameBytes, r.w, 40, 84, 5, colText, now.UTC().Format("2006-01-02 15:04:05.000"))
	drawText(frameBytes, r.w, 40, 150, 3, colText, fmt.Sprintf("frame %06d", frame))

	// 1 Hz blink square, top-right (lit for the first half of each second).
	sq := 40
	if now.Nanosecond()/1e6 < 500 {
		x0, y0 := r.w-40-sq, 36
		for y := 0; y < sq; y++ {
			for x := 0; x < sq; x++ {
				putPixel(frameBytes, r.w, x0+x, y0+y, colBlink)
			}
		}
	}

	// QR code, bottom-right: white field + quiet zone, black modules.
	size, mod := qrEncode([]byte(qrPayload(now, frame)))
	if size > 0 {
		field := size*qrModuleScale + 2*qrQuietZone*qrModuleScale
		x0 := r.w - qrMargin - field
		y0 := r.h - qrMargin - field
		for y := 0; y < field; y++ {
			for x := 0; x < field; x++ {
				putPixel(frameBytes, r.w, x0+x, y0+y, [3]byte{255, 255, 255})
			}
		}
		off := qrQuietZone * qrModuleScale
		for my := 0; my < size; my++ {
			for mx := 0; mx < size; mx++ {
				if mod[(my*size + mx)] {
					for dy := 0; dy < qrModuleScale; dy++ {
						for dx := 0; dx < qrModuleScale; dx++ {
							putPixel(frameBytes, r.w, x0+off+mx*qrModuleScale+dx, y0+off+my*qrModuleScale+dy, [3]byte{0, 0, 0})
						}
					}
				}
			}
		}
	}
	return frameBytes
}

func drawRect(frame []byte, w, x, y, width, height int, color [3]byte) {
	for py := y; py < y+height; py++ {
		for px := x; px < x+width; px++ {
			putPixel(frame, w, px, py, color)
		}
	}
}
