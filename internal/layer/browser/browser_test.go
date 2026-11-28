package browser

import (
	"bytes"
	"context"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-gst/go-gst/pkg/gst"

	"ctrlirl-coder/internal/scene"
)

func TestMain(m *testing.M) {
	gst.Init()
	os.Exit(m.Run())
}

func TestSanitizeSinkID(t *testing.T) {
	cases := map[string]string{
		"main":        "main",
		"my-layer":    "my_layer",
		"a.b/c":       "a_b_c",
		"":            "layer",
		"123":         "123",
		"weird!!id==": "weird__id__",
	}
	for in, want := range cases {
		if got := sanitizeSinkID(in); got != want {
			t.Errorf("sanitizeSinkID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewBrowserOverlay_Basics(t *testing.T) {
	l := scene.Layer{ID: "b1", Type: "browser", Source: "https://example.com"}
	b := NewBrowserOverlay(l)
	if b.ID() != "b1" {
		t.Errorf("ID() = %q, want b1", b.ID())
	}
	if b.Type() != "browser" {
		t.Errorf("Type() = %q, want browser", b.Type())
	}
	props := b.DefaultProperties()
	if _, ok := props["zorder"]; !ok {
		t.Error("DefaultProperties missing zorder")
	}
}

func TestCDPSessionCapturePaintedLocalPage(t *testing.T) {
	pageDir := t.TempDir()
	page := filepath.Join(pageDir, "page.html")
	// The canvas covers only the top 360px; the rest of the page must come
	// back transparent (alpha 0) after capture, which verifies the
	// transparent-background + PNG alpha path end to end.
	if err := os.WriteFile(page, []byte(`<!doctype html>
<html><head><style>
html,body{width:100%;height:100%;margin:0}
body{margin:0}canvas{display:block;width:1280px;height:360px}
</style></head><body><canvas id="page" width="1280" height="360"></canvas><script>
const ctx=document.getElementById("page").getContext("2d");
let seed=123456789;
for(let y=0;y<360;y+=8){
  for(let x=0;x<1280;x+=8){
    seed=(seed*1664525+1013904223)>>>0;
    ctx.fillStyle="rgb("+ (seed&255) +","+ ((seed>>>8)&255) +","+ ((seed>>>16)&255) +")";
    ctx.fillRect(x,y,8,8);
  }
}
</script></body></html>`), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := newCDPSession(ctx, "file://"+page, "", 1280, 720)
	if err != nil {
		t.Fatalf("newCDPSession(local) error: %v", err)
	}
	defer sess.stop()

	time.Sleep(2 * time.Second)
	frame, err := sess.capture(ctx)
	if err != nil {
		t.Fatalf("capture(local) error: %v", err)
	}
	if len(frame) <= 32768 {
		t.Fatalf("test PNG = %d bytes, want > CDP 32 KiB read limit", len(frame))
	}
	img, err := png.Decode(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("decode captured PNG: %v", err)
	}

	colors := make(map[[3]uint32]struct{})
	bounds := img.Bounds()
	for y := 0; y < bounds.Dy()/2; y += 24 {
		for x := 0; x < bounds.Dx(); x += 24 {
			r, g, b, _ := img.At(x, y).RGBA()
			colors[[3]uint32{r >> 8, g >> 8, b >> 8}] = struct{}{}
		}
	}
	if len(colors) < 2 {
		t.Fatalf("captured local page appears blank: only %d sampled colors", len(colors))
	}
	// Top half: painted canvas, fully opaque. Bottom half: page background,
	// must be transparent.
	if _, _, _, a := img.At(640, 40).RGBA(); a < 0xffff {
		t.Errorf("painted pixel alpha = %#x, want 0xffff (opaque)", a)
	}
	if _, _, _, a := img.At(640, bounds.Dy()-40).RGBA(); a != 0 {
		t.Errorf("background pixel alpha = %#x, want 0 (transparent)", a)
	}
}
