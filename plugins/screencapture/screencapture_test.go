package screencapture

import (
	"errors"
	"testing"
	"time"
)

func newRecording(rate int) *Recording {
	return &Recording{
		frames:   make(chan Frame, frameBuffer),
		done:     make(chan struct{}),
		start:    time.Now(),
		interval: time.Second / time.Duration(Options{FrameRate: rate}.frameRate()),
	}
}

func TestToRGBA(t *testing.T) {
	// 2×2 pixels in rows of 12 bytes, with padding.
	bgra := []byte{
		1, 2, 3, 0, 4, 5, 6, 0, 9, 9, 9, 9,
		7, 8, 9, 0, 10, 11, 12, 0,
	}
	img := toRGBA(bgra, 2, 2, 12, orderBGRA)
	want := []byte{3, 2, 1, 255, 6, 5, 4, 255, 9, 8, 7, 255, 12, 11, 10, 255}
	if string(img.Pix) != string(want) {
		t.Errorf("BGRA: got %v, want %v", img.Pix, want)
	}
	img = toRGBA(bgra, 2, 2, 12, orderRGBA)
	want = []byte{1, 2, 3, 255, 4, 5, 6, 255, 7, 8, 9, 255, 10, 11, 12, 255}
	if string(img.Pix) != string(want) {
		t.Errorf("RGBA: got %v, want %v", img.Pix, want)
	}
}

func TestDeliverDropsTheOldest(t *testing.T) {
	r := newRecording(240)
	for i := range 5 {
		r.deliver([]byte{byte(i), 0, 0, 0}, 1, 1, 4, orderRGBA)
		r.mu.Lock()
		r.delivered = false // not rate limited
		r.mu.Unlock()
	}
	var got []byte
	for range frameBuffer {
		got = append(got, (<-r.Frames()).Image.Pix[0])
	}
	if string(got) != string([]byte{3, 4}) {
		t.Errorf("frames %v, want the newest, 3 and 4", got)
	}
}

func TestDeliverKeepsTheFrameRate(t *testing.T) {
	r := newRecording(10)
	if !r.wants() {
		t.Fatal("a new recording wants a frame")
	}
	r.deliver([]byte{1, 2, 3, 4}, 1, 1, 4, orderRGBA)
	if r.wants() {
		t.Error("wants a frame right after one")
	}
	r.deliver([]byte{1, 2, 3, 4}, 1, 1, 4, orderRGBA)
	if n := len(r.frames); n != 1 {
		t.Errorf("%d frames, want 1", n)
	}
	r.mu.Lock()
	r.last -= 100 * time.Millisecond
	r.mu.Unlock()
	r.deliver([]byte{1, 2, 3, 4}, 1, 1, 4, orderRGBA)
	if n := len(r.frames); n != 2 {
		t.Errorf("%d frames a tenth of a second later, want 2", n)
	}
}

func TestDeliverChecksTheSize(t *testing.T) {
	r := newRecording(0)
	r.deliver(make([]byte, 7), 1, 2, 4, orderBGRA)
	r.deliver(make([]byte, 8), 2, 1, 4, orderBGRA)
	if len(r.frames) != 0 {
		t.Error("delivered pixels shorter than their size")
	}
}

func TestStop(t *testing.T) {
	r := newRecording(0)
	stopped := 0
	r.stopNative = func() { stopped++ }
	if err := r.Stop(); err != nil || stopped != 1 {
		t.Fatalf("Stop: %v, stopped %d times", err, stopped)
	}
	if _, ok := <-r.Frames(); ok {
		t.Error("Frames is open after Stop")
	}
	<-r.Done()
	r.deliver([]byte{1, 2, 3, 4}, 1, 1, 4, orderRGBA) // no panic
	r.Stop()
	if stopped != 1 {
		t.Errorf("stopped %d times", stopped)
	}
}

func TestEndKeepsTheError(t *testing.T) {
	r := newRecording(0)
	r.stopNative = func() {}
	gone := errors.New("gone")
	r.end(gone)
	r.end(errors.New("later"))
	if err := r.Stop(); err != gone {
		t.Errorf("Stop: %v, want %v", err, gone)
	}
}

func TestSource(t *testing.T) {
	for _, c := range []struct {
		src    Source
		window bool
		id     uint64
		str    string
	}{
		{Source{}, false, 0, "main display"},
		{DisplaySource(69734208), false, 69734208, "display 69734208"},
		{WindowSource(42), true, 42, "window 42"},
		{Window{ID: 7}.Source(), true, 7, "window 7"},
	} {
		if c.src.IsWindow() != c.window || c.src.ID() != c.id || c.src.String() != c.str {
			t.Errorf("%#v: %v %d %q", c.src, c.src.IsWindow(), c.src.ID(), c.src.String())
		}
	}
}
