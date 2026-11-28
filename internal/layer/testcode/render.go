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

	"ctrlirl-coder/internal/layer"
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
	colQRLight    = [3]byte{255, 255, 255}
	colQRDark     = [3]byte{0, 0, 0}
)

// testcodeRenderer renders frames for one testcode layer.
type testcodeRenderer struct {
	w, h, fps int

	frame []byte // reused between renders (one layer, one render goroutine)
}

// newTestcodeRenderer reads the surface size/fps from the scene properties
// (width/height/framerate), defaulting to 1280x720@30.
func newTestcodeRenderer(l scene.Layer) *testcodeRenderer {
	return &testcodeRenderer{
		w:   layer.IntProp(l, "width", defaultSurfaceW),
		h:   layer.IntProp(l, "height", defaultSurfaceH),
		fps: layer.IntProp(l, "framerate", defaultFPS),
	}
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
// reading. The QR matrix is recomputed each frame because the payload
// includes the frame number.
//
// The returned slice is the renderer's own buffer, valid until the next
// render: the caller pushes it into the appsrc, which copies it.
func (r *testcodeRenderer) render(now time.Time, frame uint64) []byte {
	if len(r.frame) != r.w*r.h*4 {
		r.frame = make([]byte, r.w*r.h*4)
	} else {
		clear(r.frame) // transparent wherever nothing is drawn
	}
	buf := r.frame

	// Label + timestamp + frame counter, top-left.
	fillRect(buf, r.w, 24, 20, 740, 180, colBackground)
	drawText(buf, r.w, 40, 36, 3, colText, "CTRL IRL TESTCODE")
	drawText(buf, r.w, 40, 84, 5, colText, now.UTC().Format("2006-01-02 15:04:05.000"))
	drawText(buf, r.w, 40, 150, 3, colText, fmt.Sprintf("frame %06d", frame))

	// 1 Hz blink square, top-right (lit for the first half of each second).
	const blinkSize = 40
	if now.Nanosecond()/1e6 < 500 {
		fillRect(buf, r.w, r.w-40-blinkSize, 36, blinkSize, blinkSize, colBlink)
	}

	// QR code, bottom-right: white field + quiet zone, black modules.
	size, mod := qrEncode([]byte(qrPayload(now, frame)))
	if size > 0 {
		field := (size + 2*qrQuietZone) * qrModuleScale
		x0 := r.w - qrMargin - field
		y0 := r.h - qrMargin - field
		fillRect(buf, r.w, x0, y0, field, field, colQRLight)
		off := qrQuietZone * qrModuleScale
		for my := 0; my < size; my++ {
			for mx := 0; mx < size; mx++ {
				if mod[my*size+mx] {
					fillRect(buf, r.w, x0+off+mx*qrModuleScale, y0+off+my*qrModuleScale,
						qrModuleScale, qrModuleScale, colQRDark)
				}
			}
		}
	}
	return buf
}

// fillRect paints an opaque rectangle into the BGRA frame (row width w),
// clipped to the frame.
func fillRect(frame []byte, w, x, y, width, height int, color [3]byte) {
	if w <= 0 {
		return
	}
	x0, x1 := max(x, 0), min(x+width, w)
	y0, y1 := max(y, 0), min(y+height, len(frame)/(4*w))
	if x0 >= x1 || y0 >= y1 {
		return
	}
	// Paint the first row pixel by pixel, then copy it down.
	first := frame[(y0*w+x0)*4 : (y0*w+x1)*4]
	for i := 0; i < len(first); i += 4 {
		first[i], first[i+1], first[i+2], first[i+3] = color[0], color[1], color[2], 0xFF
	}
	for row := y0 + 1; row < y1; row++ {
		copy(frame[(row*w+x0)*4:], first)
	}
}
