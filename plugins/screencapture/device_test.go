package screencapture

import (
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDevice captures the real screen, where MYGO_SCREENCAPTURE=1: it
// lists the windows, takes a screenshot of the main display, records it
// for two seconds, and records the frontmost window for one, and writes
// the images to MYGO_SCREENCAPTURE_OUT, the temporary directory by
// default, for a look. On Linux it goes through the portals, which ask.
func TestDevice(t *testing.T) {
	if os.Getenv("MYGO_SCREENCAPTURE") != "1" {
		t.Skip("set MYGO_SCREENCAPTURE=1 to capture the screen")
	}
	out := os.Getenv("MYGO_SCREENCAPTURE_OUT")
	if out == "" {
		out = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	t.Logf("access: %v", Access())
	if !RequestAccess() {
		t.Fatal("no access to the screen")
	}

	wins, err := Windows(ctx)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Log("windows: unsupported")
	} else if err != nil {
		t.Fatalf("windows: %v", err)
	} else {
		for i, w := range wins {
			if i < 10 {
				t.Logf("window %d %q of %q at %+v", w.ID, w.Title, w.App, w.Bounds)
			}
		}
	}

	start := time.Now()
	img, err := Capture(ctx, Source{}, Options{})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	t.Logf("screenshot: %v in %v", img.Rect, time.Since(start))
	save(t, filepath.Join(out, "screenshot.png"), img)

	record(t, ctx, Source{}, Options{FrameRate: 30}, 2*time.Second, filepath.Join(out, "display-last.png"))
	if len(wins) > 0 {
		record(t, ctx, wins[0].Source(), Options{FrameRate: 10, HideCursor: true}, time.Second, filepath.Join(out, "window-last.png"))
	}
	t.Logf("images in %s", out)
}

func record(t *testing.T, ctx context.Context, src Source, opts Options, d time.Duration, path string) {
	t.Helper()
	rec, err := Record(ctx, src, opts)
	if err != nil {
		t.Fatalf("record %v: %v", src, err)
	}
	timer := time.AfterFunc(d, func() { rec.Stop() })
	defer timer.Stop()
	var n int
	var last Frame
	for f := range rec.Frames() {
		n++
		last = f
	}
	if err := rec.Err(); err != nil {
		t.Fatalf("record %v: %v", src, err)
	}
	t.Logf("record %v: %d frames in %v, the last %v at %v", src, n, d, last.Image.Bounds(), last.Time)
	if n == 0 {
		t.Fatalf("record %v: no frames", src)
	}
	save(t, path, last.Image)
}

func save(t *testing.T, path string, img *image.RGBA) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}
