// Package screencapture takes screenshots of displays and windows, and
// records them as a stream of frames:
//
//	img, err := screencapture.Capture(ctx, screencapture.Source{}, screencapture.Options{})
//
//	rec, err := screencapture.Record(ctx, screencapture.DisplaySource(d.ID), screencapture.Options{FrameRate: 30})
//	for f := range rec.Frames() {
//		encode(f.Image, f.Time)
//	}
//
// It captures with ScreenCaptureKit on macOS (12.3 and later), with
// Windows.Graphics.Capture on Windows (10, 1903 and later), and with the
// ScreenCast and Screenshot portals of xdg-desktop-portal on Linux, whose
// frames come through PipeWire. Browsers and other systems return
// errors.ErrUnsupported.
//
// The system asks the user. macOS asks once for the app, in System
// Settings (see Access and RequestAccess), and Linux asks each time,
// in a dialog of the desktop where the user picks the display or the
// window: the IDs of sources are what macOS and Windows go by. Windows
// asks nothing, and draws a border around what is captured.
//
// Frames are RGBA images in sRGB, at the pixels of the display, and a
// recording keeps up with the screen, not with its reader: when the
// reader falls behind, the oldest frames waiting are dropped. Every
// function and method is safe to call from any goroutine.
package screencapture

import (
	"context"
	"errors"
	"fmt"
	"image"
	"sync"
	"time"

	"github.com/egoist/mygo"
)

// Errors.
var (
	// ErrDenied is returned when the user, or the system, did not let the
	// app capture the screen, or when the user cancelled the portal's
	// dialog on Linux.
	ErrDenied = errors.New("screencapture: denied")
	// ErrNotFound is returned when the display or the window is gone.
	ErrNotFound = errors.New("screencapture: no such display or window")
)

// A Source is what to capture: a display, the main one when it is the
// zero Source, or a window.
type Source struct {
	window bool
	id     uint64
}

// DisplaySource returns the display whose ID is id, as mygo.Display has
// it (mygo.Screen.Displays). Linux lets the user pick a display instead.
func DisplaySource(id int64) Source { return Source{id: uint64(id)} }

// WindowSource returns the window whose ID is id, as Window has it. Linux
// lets the user pick a window instead, and takes any ID, 0 included.
func WindowSource(id uint64) Source { return Source{window: true, id: id} }

// IsWindow reports whether s is a window.
func (s Source) IsWindow() bool { return s.window }

// ID returns the ID of the display or the window, 0 for the main display.
func (s Source) ID() uint64 { return s.id }

func (s Source) String() string {
	switch {
	case s.window:
		return fmt.Sprintf("window %d", s.id)
	case s.id == 0:
		return "main display"
	}
	return fmt.Sprintf("display %d", s.id)
}

// A Window is a window on the screen, of any app.
type Window struct {
	// ID identifies the window while it is open: its CGWindowID on macOS,
	// its HWND on Windows.
	ID uint64
	// Title is the window's title, which may be empty.
	Title string
	// App is the name of the app the window belongs to.
	App string
	// Bounds is where the window is, in screen coordinates as
	// mygo.Display has them.
	Bounds mygo.Rectangle
}

// Source returns the window as a Source.
func (w Window) Source() Source { return WindowSource(w.ID) }

// Windows returns the windows on the screen that can be captured, those
// in front first, without the desktop's own, such as the menu bar and the
// Dock. Linux has no such list, and returns errors.ErrUnsupported: its
// portal lets the user pick a window (see WindowSource).
func Windows(ctx context.Context) ([]Window, error) { return windows(ctx) }

// Access reports whether the app may capture the screen. Only macOS asks
// for the app as a whole: elsewhere it reports true, and Linux asks with
// each capture.
func Access() bool { return access() }

// RequestAccess asks the user to let the app capture the screen, on macOS,
// and reports whether it may. macOS asks once: after the user decides, it
// only reports, and the user changes their mind in System Settings
// (Privacy & Security, Screen & System Audio Recording), which takes
// effect when the app starts again. Capturing asks too.
func RequestAccess() bool { return requestAccess() }

// Options configures a capture.
type Options struct {
	// FrameRate is how many frames a second a recording makes at most:
	// 30 when 0. Systems make a frame only when the screen changes.
	FrameRate int
	// HideCursor leaves the mouse pointer out.
	HideCursor bool
}

func (o Options) frameRate() int {
	if o.FrameRate <= 0 {
		return 30
	}
	return min(o.FrameRate, 240)
}

// A Frame is an image of a recording.
type Frame struct {
	// Image is the frame, which the reader owns.
	Image *image.RGBA
	// Time is when the frame came, since the recording started.
	Time time.Duration
}

// Capture takes a screenshot of src. On Linux, it takes one of the whole
// desktop, or the window the user picks, through the Screenshot portal,
// which may ask the user first.
func Capture(ctx context.Context, src Source, opts Options) (*image.RGBA, error) {
	if img, ok, err := screenshot(ctx, src, opts); ok {
		return img, err
	}
	rec, err := Record(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	defer rec.Stop()
	select {
	case f, ok := <-rec.Frames():
		if !ok {
			if err := rec.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("screencapture: the capture stopped")
		}
		return f.Image, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Record starts recording src, until Stop is called, ctx is done, or the
// capture ends, such as when the window closes or the user stops it from
// the system's controls. The context also bounds the start, which on
// Linux waits for the user to pick what to share.
func Record(ctx context.Context, src Source, opts Options) (*Recording, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := &Recording{
		frames:   make(chan Frame, frameBuffer),
		done:     make(chan struct{}),
		start:    time.Now(),
		interval: time.Second / time.Duration(opts.frameRate()),
	}
	stop, err := start(ctx, src, opts, r)
	if err != nil {
		return nil, err
	}
	r.stopNative = stop
	go func() {
		// Stop releases what the system holds, after the capture ended on
		// its own too.
		select {
		case <-ctx.Done():
		case <-r.done:
		}
		r.Stop()
	}()
	return r, nil
}

// frameBuffer is how many frames wait for the reader at most.
const frameBuffer = 2

// A Recording is a capture running, from Record.
type Recording struct {
	frames     chan Frame
	done       chan struct{}
	start      time.Time
	interval   time.Duration
	stopNative func()
	stopOnce   sync.Once

	mu        sync.Mutex
	ended     bool
	err       error
	last      time.Duration
	delivered bool // a frame was passed on
}

// Frames returns the frames, a channel closed once the recording has
// stopped.
func (r *Recording) Frames() <-chan Frame { return r.frames }

// Done returns a channel closed once the recording has stopped.
func (r *Recording) Done() <-chan struct{} { return r.done }

// Err returns the error that stopped the recording: nil while it runs,
// and when Stop, its context, the user or the window closing stopped it.
func (r *Recording) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Stop stops the recording, and returns once the system has stopped
// capturing and Frames is closed. It returns the error of Err.
func (r *Recording) Stop() error {
	r.stopOnce.Do(func() {
		if r.stopNative != nil {
			r.stopNative()
		}
		r.end(nil)
	})
	return r.Err()
}

// end ends the recording, with the error that ended it, if it has not
// ended yet. The platforms call it when the capture ends on its own;
// they must not deliver frames after.
func (r *Recording) end(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		return
	}
	r.ended = true
	r.err = err
	close(r.frames)
	close(r.done)
}

// pixel orders of the frames the platforms deliver.
const (
	orderBGRA = iota
	orderRGBA
)

// wants reports whether deliver would pass on a frame now, for platforms
// to skip the work of one it would drop.
func (r *Recording) wants() bool {
	t := time.Since(r.start)
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.ended && !r.early(t)
}

// early reports whether t is too soon after the last frame.
func (r *Recording) early(t time.Duration) bool {
	return r.delivered && t-r.last < r.interval-r.interval/8
}

// deliver converts the pixels of a frame, w×h of 4 bytes each in rows of
// stride bytes, to an image, and passes it to the reader, at most
// interval apart. It runs on the platform's thread and never blocks.
func (r *Recording) deliver(pix []byte, w, h, stride, order int) {
	if w <= 0 || h <= 0 || stride < w*4 || len(pix) < (h-1)*stride+w*4 {
		return
	}
	t := time.Since(r.start)
	r.mu.Lock()
	if r.ended || r.early(t) {
		r.mu.Unlock()
		return
	}
	r.delivered, r.last = true, t
	r.mu.Unlock()
	// The platform delivers one frame at a time: convert it unlocked.
	f := Frame{Image: toRGBA(pix, w, h, stride, order), Time: t}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		return
	}
	for {
		select {
		case r.frames <- f:
			return
		default:
		}
		// The reader is behind: drop the oldest frame.
		select {
		case <-r.frames:
		default:
		}
	}
}

// toRGBA copies pixels into an opaque image.
func toRGBA(pix []byte, w, h, stride, order int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		src := pix[y*stride : y*stride+w*4]
		dst := img.Pix[y*img.Stride : y*img.Stride+w*4]
		if order == orderRGBA {
			copy(dst, src)
			for i := 3; i < len(dst); i += 4 {
				dst[i] = 0xff
			}
			continue
		}
		for i := 0; i < len(dst); i += 4 {
			dst[i], dst[i+1], dst[i+2], dst[i+3] = src[i+2], src[i+1], src[i], 0xff
		}
	}
	return img
}
