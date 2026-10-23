//go:build darwin

package screencapture

import (
	"context"
	"errors"
	"fmt"
	"image"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"github.com/egoist/mygo"
)

// macOS captures with ScreenCaptureKit: SCShareableContent finds the
// display or the window, and an SCStream delivers its frames, as BGRA
// pixel buffers in sRGB, to an output object on a dispatch queue of its
// own. Its completion handlers run on queues of the system's.

type cgPoint struct{ X, Y float64 }
type cgSize struct{ Width, Height float64 }
type cgRect struct {
	Origin cgPoint
	Size   cgSize
}

type cmTime struct {
	Value     int64
	Timescale int32
	Flags     uint32
	Epoch     int64
}

const (
	kCVPixelFormatType32BGRA    = 0x42475241 // 'BGRA'
	kCVPixelBufferLockReadOnly  = 1
	kCMTimeFlagsValid           = 1
	scStreamOutputTypeScreen    = 0
	scFrameStatusComplete       = 0
	scStreamErrorUserDeclined   = -3801
	scStreamErrorUserStopped    = -3817
	kCGWindowListOptionOnScreen = 1
	kCGNullWindowID             = 0
	scWindowLayerNormal         = 0
	maxCaptureSide              = 16384
	shareableContentSelector    = "getShareableContentExcludingDesktopWindows:onScreenWindowsOnly:completionHandler:"
	streamOutputSelector        = "stream:didOutputSampleBuffer:ofType:"
	streamStopSelector          = "stream:didStopWithError:"
	outputClassName             = "MyGoScreenCaptureOutput"
	screenCaptureKitPath        = "/System/Library/Frameworks/ScreenCaptureKit.framework/ScreenCaptureKit"
	coreGraphicsPath            = "/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics"
	coreMediaPath               = "/System/Library/Frameworks/CoreMedia.framework/CoreMedia"
	coreVideoPath               = "/System/Library/Frameworks/CoreVideo.framework/CoreVideo"
	coreFoundationPath          = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
	libSystemPath               = "/usr/lib/libSystem.B.dylib"
	libObjCPath                 = "/usr/lib/libobjc.A.dylib"
)

var (
	sckOnce sync.Once
	sckErr  error

	cgPreflightScreenCaptureAccess          func() bool
	cgRequestScreenCaptureAccess            func() bool
	cgMainDisplayID                         func() uint32
	cgDisplayCopyDisplayMode                func(display uint32) uintptr
	cgDisplayModeGetPixelWidth              func(mode uintptr) uintptr
	cgDisplayModeGetPixelHeight             func(mode uintptr) uintptr
	cgDisplayModeGetWidth                   func(mode uintptr) uintptr
	cgDisplayModeRelease                    func(mode uintptr)
	cgWindowListCreate                      func(option uint32, relativeTo uint32) uintptr
	cfArrayGetCount                         func(arr uintptr) int
	cfArrayGetValueAtIndex                  func(arr uintptr, i int) uintptr
	cfRelease                               func(obj uintptr)
	cmSampleBufferGetImageBuffer            func(buf uintptr) uintptr
	cmSampleBufferGetSampleAttachmentsArray func(buf uintptr, create bool) objc.ID
	cvPixelBufferLockBaseAddress            func(pb uintptr, flags uint64) int32
	cvPixelBufferUnlockBaseAddress          func(pb uintptr, flags uint64) int32
	cvPixelBufferGetBaseAddress             func(pb uintptr) unsafe.Pointer
	cvPixelBufferGetBytesPerRow             func(pb uintptr) uintptr
	cvPixelBufferGetWidth                   func(pb uintptr) uintptr
	cvPixelBufferGetHeight                  func(pb uintptr) uintptr
	dispatchQueueCreate                     func(label *byte, attr uintptr) uintptr

	msgRect        func(obj objc.ID, sel objc.SEL) cgRect
	msgFloat       func(obj objc.ID, sel objc.SEL) float64
	msgSetCMTime   func(obj objc.ID, sel objc.SEL, t cmTime)
	poolPush       uintptr
	poolPop        uintptr
	colorSpaceSRGB objc.ID // kCGColorSpaceSRGB
	frameStatusKey objc.ID // SCStreamFrameInfoStatus

	outputs     sync.Map // output object → *macStream
	sampleQueue uintptr
)

func sel(name string) objc.SEL { return objc.RegisterName(name) }

func loadSCK() error {
	sckOnce.Do(func() {
		sck, err := purego.Dlopen(screenCaptureKitPath, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			sckErr = fmt.Errorf("screencapture: ScreenCaptureKit needs macOS 12.3: %w", errors.ErrUnsupported)
			return
		}
		libs := map[string]uintptr{screenCaptureKitPath: sck}
		for _, path := range []string{coreGraphicsPath, coreMediaPath, coreVideoPath, coreFoundationPath, libSystemPath, libObjCPath} {
			if libs[path], err = purego.Dlopen(path, purego.RTLD_LAZY|purego.RTLD_GLOBAL); err != nil {
				sckErr = fmt.Errorf("screencapture: %w", err)
				return
			}
		}
		sym := func(lib, name string) uintptr {
			p, err := purego.Dlsym(libs[lib], name)
			if err != nil && sckErr == nil {
				sckErr = fmt.Errorf("screencapture: missing %s: %w", name, errors.ErrUnsupported)
			}
			return p
		}
		for _, f := range []struct {
			fn        any
			lib, name string
		}{
			{&cgPreflightScreenCaptureAccess, coreGraphicsPath, "CGPreflightScreenCaptureAccess"},
			{&cgRequestScreenCaptureAccess, coreGraphicsPath, "CGRequestScreenCaptureAccess"},
			{&cgMainDisplayID, coreGraphicsPath, "CGMainDisplayID"},
			{&cgDisplayCopyDisplayMode, coreGraphicsPath, "CGDisplayCopyDisplayMode"},
			{&cgDisplayModeGetPixelWidth, coreGraphicsPath, "CGDisplayModeGetPixelWidth"},
			{&cgDisplayModeGetPixelHeight, coreGraphicsPath, "CGDisplayModeGetPixelHeight"},
			{&cgDisplayModeGetWidth, coreGraphicsPath, "CGDisplayModeGetWidth"},
			{&cgDisplayModeRelease, coreGraphicsPath, "CGDisplayModeRelease"},
			{&cgWindowListCreate, coreGraphicsPath, "CGWindowListCreate"},
			{&cfArrayGetCount, coreFoundationPath, "CFArrayGetCount"},
			{&cfArrayGetValueAtIndex, coreFoundationPath, "CFArrayGetValueAtIndex"},
			{&cfRelease, coreFoundationPath, "CFRelease"},
			{&cmSampleBufferGetImageBuffer, coreMediaPath, "CMSampleBufferGetImageBuffer"},
			{&cmSampleBufferGetSampleAttachmentsArray, coreMediaPath, "CMSampleBufferGetSampleAttachmentsArray"},
			{&cvPixelBufferLockBaseAddress, coreVideoPath, "CVPixelBufferLockBaseAddress"},
			{&cvPixelBufferUnlockBaseAddress, coreVideoPath, "CVPixelBufferUnlockBaseAddress"},
			{&cvPixelBufferGetBaseAddress, coreVideoPath, "CVPixelBufferGetBaseAddress"},
			{&cvPixelBufferGetBytesPerRow, coreVideoPath, "CVPixelBufferGetBytesPerRow"},
			{&cvPixelBufferGetWidth, coreVideoPath, "CVPixelBufferGetWidth"},
			{&cvPixelBufferGetHeight, coreVideoPath, "CVPixelBufferGetHeight"},
			{&dispatchQueueCreate, libSystemPath, "dispatch_queue_create"},
		} {
			if p := sym(f.lib, f.name); p != 0 {
				purego.RegisterFunc(f.fn, p)
			}
		}
		msgSend := sym(libObjCPath, "objc_msgSend")
		// Methods returning structs larger than 16 bytes use another entry
		// point on amd64.
		stret := msgSend
		if runtime.GOARCH == "amd64" {
			stret = sym(libObjCPath, "objc_msgSend_stret")
		}
		poolPush = sym(libObjCPath, "objc_autoreleasePoolPush")
		poolPop = sym(libObjCPath, "objc_autoreleasePoolPop")
		srgb := sym(coreGraphicsPath, "kCGColorSpaceSRGB")
		status := sym(screenCaptureKitPath, "SCStreamFrameInfoStatus")
		if sckErr != nil {
			return
		}
		purego.RegisterFunc(&msgRect, stret)
		purego.RegisterFunc(&msgFloat, msgSend)
		purego.RegisterFunc(&msgSetCMTime, msgSend)
		colorSpaceSRGB = **(**objc.ID)(unsafe.Pointer(&srgb))
		frameStatusKey = **(**objc.ID)(unsafe.Pointer(&status))

		var protocols []*objc.Protocol
		for _, name := range []string{"SCStreamOutput", "SCStreamDelegate"} {
			if p := objc.GetProtocol(name); p != nil {
				protocols = append(protocols, p)
			}
		}
		_, err = objc.RegisterClass(outputClassName, objc.GetClass("NSObject"), protocols, nil, []objc.MethodDef{
			{Cmd: sel(streamOutputSelector), Fn: didOutputSampleBuffer},
			{Cmd: sel(streamStopSelector), Fn: didStop},
		})
		if err != nil {
			sckErr = fmt.Errorf("screencapture: %w", err)
			return
		}
		sampleQueue = dispatchQueueCreate(cstr("dev.mygo.screencapture"), 0)
	})
	return sckErr
}

func cstr(s string) *byte {
	b := append([]byte(s), 0)
	return &b[0]
}

// withPool runs fn in an autorelease pool, on one thread.
func withPool(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	p, _, _ := purego.SyscallN(poolPush)
	defer purego.SyscallN(poolPop, p)
	fn()
}

func goString(s objc.ID) string {
	if s == 0 {
		return ""
	}
	p := objc.Send[unsafe.Pointer](s, sel("UTF8String"))
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(p), n))
}

func items(arr objc.ID) []objc.ID {
	if arr == 0 {
		return nil
	}
	n := objc.Send[uint](arr, sel("count"))
	out := make([]objc.ID, n)
	for i := range out {
		out[i] = arr.Send(sel("objectAtIndex:"), uint(i))
	}
	return out
}

// nsError converts an NSError of ScreenCaptureKit.
func nsError(err objc.ID) error {
	if err == 0 {
		return nil
	}
	if objc.Send[int](err, sel("code")) == scStreamErrorUserDeclined {
		return ErrDenied
	}
	return fmt.Errorf("screencapture: %s", goString(err.Send(sel("localizedDescription"))))
}

func access() bool {
	if loadSCK() != nil {
		return false
	}
	return cgPreflightScreenCaptureAccess()
}

func requestAccess() bool {
	if loadSCK() != nil {
		return false
	}
	return cgRequestScreenCaptureAccess()
}

// shareableContent passes the displays and windows on the screen to fn,
// on a queue of the system's, in an autorelease pool.
func shareableContent(ctx context.Context, fn func(content objc.ID) error) error {
	if err := loadSCK(); err != nil {
		return err
	}
	done := make(chan error, 1)
	var once sync.Once
	blk := objc.NewBlock(func(_ objc.Block, content, nsErr objc.ID) {
		once.Do(func() {
			if nsErr != 0 || content == 0 {
				if nsErr == 0 {
					done <- ErrDenied
				} else {
					done <- nsError(nsErr)
				}
				return
			}
			var err error
			withPool(func() { err = fn(content) })
			done <- err
		})
	})
	defer blk.Release()
	withPool(func() {
		objc.ID(objc.GetClass("SCShareableContent")).Send(sel(shareableContentSelector), true, true, blk)
	})
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// fn must not run once this has returned: if it has, its result
		// stands.
		ran := true
		once.Do(func() { ran = false })
		if ran {
			return <-done
		}
		return ctx.Err()
	}
}

func windows(ctx context.Context) ([]Window, error) {
	var out []Window
	err := shareableContent(ctx, func(content objc.ID) error {
		byID := map[uint32]Window{}
		for _, w := range items(content.Send(sel("windows"))) {
			if objc.Send[int](w, sel("windowLayer")) != scWindowLayerNormal || !objc.Send[bool](w, sel("isOnScreen")) {
				continue
			}
			app := w.Send(sel("owningApplication"))
			r := msgRect(w, sel("frame"))
			if app == 0 || r.Size.Width < 1 || r.Size.Height < 1 {
				continue
			}
			id := objc.Send[uint32](w, sel("windowID"))
			byID[id] = Window{
				ID:     uint64(id),
				Title:  goString(w.Send(sel("title"))),
				App:    goString(app.Send(sel("applicationName"))),
				Bounds: rectangle(r),
			}
		}
		// In the order of the window server, front to back.
		list := cgWindowListCreate(kCGWindowListOptionOnScreen, kCGNullWindowID)
		if list != 0 {
			for i := range cfArrayGetCount(list) {
				id := uint32(cfArrayGetValueAtIndex(list, i))
				if w, ok := byID[id]; ok {
					out = append(out, w)
					delete(byID, id)
				}
			}
			cfRelease(list)
		}
		for _, w := range byID {
			out = append(out, w)
		}
		return nil
	})
	return out, err
}

func rectangle(r cgRect) mygo.Rectangle {
	return mygo.Rectangle{X: int(r.Origin.X), Y: int(r.Origin.Y), Width: int(r.Size.Width), Height: int(r.Size.Height)}
}

func screenshot(context.Context, Source, Options) (*image.RGBA, bool, error) { return nil, false, nil }

// macStream is a recording's SCStream, and the output object that takes
// its frames.
type macStream struct {
	rec    *Recording
	stream objc.ID
	output objc.ID
}

func start(ctx context.Context, src Source, opts Options, r *Recording) (func(), error) {
	var filter objc.ID
	var width, height int
	err := shareableContent(ctx, func(content objc.ID) error {
		var err error
		filter, width, height, err = newFilter(content, src)
		return err
	})
	if err != nil {
		return nil, err
	}
	s := &macStream{rec: r}
	var startErr error
	withPool(func() {
		defer filter.Send(sel("release"))
		config := objc.ID(objc.GetClass("SCStreamConfiguration")).Send(sel("new"))
		defer config.Send(sel("release"))
		config.Send(sel("setWidth:"), uint(width))
		config.Send(sel("setHeight:"), uint(height))
		config.Send(sel("setPixelFormat:"), uint32(kCVPixelFormatType32BGRA))
		config.Send(sel("setShowsCursor:"), !opts.HideCursor)
		config.Send(sel("setQueueDepth:"), 3)
		msgSetCMTime(config, sel("setMinimumFrameInterval:"), cmTime{Value: 1, Timescale: int32(opts.frameRate()), Flags: kCMTimeFlagsValid})
		if colorSpaceSRGB != 0 {
			config.Send(sel("setColorSpaceName:"), colorSpaceSRGB)
		}
		s.output = objc.ID(objc.GetClass(outputClassName)).Send(sel("new"))
		outputs.Store(s.output, s)
		s.stream = objc.ID(objc.GetClass("SCStream")).Send(sel("alloc")).
			Send(sel("initWithFilter:configuration:delegate:"), filter, config, s.output)
		if s.stream == 0 {
			startErr = errors.New("screencapture: cannot create a stream")
			return
		}
		var nsErr objc.ID
		if !objc.Send[bool](s.stream, sel("addStreamOutput:type:sampleHandlerQueue:error:"), s.output, scStreamOutputTypeScreen, sampleQueue, unsafe.Pointer(&nsErr)) {
			startErr = nsError(nsErr)
			if startErr == nil {
				startErr = errors.New("screencapture: cannot add the stream's output")
			}
		}
	})
	if startErr == nil {
		startErr = s.call("startCaptureWithCompletionHandler:")
	}
	if startErr != nil {
		s.release()
		return nil, startErr
	}
	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			s.call("stopCaptureWithCompletionHandler:")
			s.release()
		})
	}, nil
}

// call sends the stream a message whose argument is a completion handler
// with an NSError, and waits for it.
func (s *macStream) call(selector string) error {
	done := make(chan error, 1)
	blk := objc.NewBlock(func(_ objc.Block, nsErr objc.ID) { done <- nsError(nsErr) })
	defer blk.Release()
	withPool(func() { s.stream.Send(sel(selector), blk) })
	return <-done
}

func (s *macStream) release() {
	outputs.Delete(s.output)
	withPool(func() {
		if s.stream != 0 {
			s.stream.Send(sel("release"))
		}
		s.output.Send(sel("release"))
	})
}

// newFilter returns a content filter for src, retained, and the size of
// its frames in pixels.
func newFilter(content objc.ID, src Source) (objc.ID, int, int, error) {
	var filter objc.ID
	var points cgSize
	if src.window {
		for _, w := range items(content.Send(sel("windows"))) {
			if uint64(objc.Send[uint32](w, sel("windowID"))) == src.id {
				filter = objc.ID(objc.GetClass("SCContentFilter")).Send(sel("alloc")).Send(sel("initWithDesktopIndependentWindow:"), w)
				points = msgRect(w, sel("frame")).Size
				break
			}
		}
	} else {
		id := uint32(src.id)
		if src.id == 0 {
			id = cgMainDisplayID()
		}
		for _, d := range items(content.Send(sel("displays"))) {
			if objc.Send[uint32](d, sel("displayID")) == id {
				empty := objc.ID(objc.GetClass("NSArray")).Send(sel("array"))
				filter = objc.ID(objc.GetClass("SCContentFilter")).Send(sel("alloc")).Send(sel("initWithDisplay:excludingWindows:"), d, empty)
				points = msgRect(d, sel("frame")).Size
				break
			}
		}
	}
	if filter == 0 {
		return 0, 0, 0, ErrNotFound
	}
	scale := 0.0
	// macOS 14 tells the filter's scale.
	if objc.Send[bool](filter, sel("respondsToSelector:"), sel("pointPixelScale")) {
		scale = float64(objc.Send[float32](filter, sel("pointPixelScale")))
	}
	if scale <= 0 {
		scale = displayScale(cgMainDisplayID())
		if !src.window {
			scale = displayScale(uint32(src.id))
		}
	}
	w := min(int(points.Width*scale+0.5), maxCaptureSide)
	h := min(int(points.Height*scale+0.5), maxCaptureSide)
	if w < 1 || h < 1 {
		filter.Send(sel("release"))
		return 0, 0, 0, ErrNotFound
	}
	return filter, w, h, nil
}

// displayScale returns the pixels a point of the display has.
func displayScale(display uint32) float64 {
	if display == 0 {
		display = cgMainDisplayID()
	}
	mode := cgDisplayCopyDisplayMode(display)
	if mode == 0 {
		return 1
	}
	defer cgDisplayModeRelease(mode)
	if w := cgDisplayModeGetWidth(mode); w > 0 {
		return float64(cgDisplayModeGetPixelWidth(mode)) / float64(w)
	}
	return 1
}

func didOutputSampleBuffer(self objc.ID, _ objc.SEL, _ objc.ID, sample uintptr, typ int) {
	if typ != scStreamOutputTypeScreen || sample == 0 {
		return
	}
	v, ok := outputs.Load(self)
	if !ok {
		return
	}
	s := v.(*macStream)
	p, _, _ := purego.SyscallN(poolPush)
	defer purego.SyscallN(poolPop, p)
	if !frameComplete(sample) {
		return
	}
	pb := cmSampleBufferGetImageBuffer(sample)
	if pb == 0 || cvPixelBufferLockBaseAddress(pb, kCVPixelBufferLockReadOnly) != 0 {
		return
	}
	defer cvPixelBufferUnlockBaseAddress(pb, kCVPixelBufferLockReadOnly)
	base := cvPixelBufferGetBaseAddress(pb)
	w, h := int(cvPixelBufferGetWidth(pb)), int(cvPixelBufferGetHeight(pb))
	stride := int(cvPixelBufferGetBytesPerRow(pb))
	if base == nil || w <= 0 || h <= 0 {
		return
	}
	s.rec.deliver(unsafe.Slice((*byte)(base), stride*h), w, h, stride, orderBGRA)
}

// frameComplete reports whether a sample has a new frame: ScreenCaptureKit
// also sends samples when nothing changed, without one.
func frameComplete(sample uintptr) bool {
	if frameStatusKey == 0 {
		return true
	}
	attachments := items(cmSampleBufferGetSampleAttachmentsArray(sample, false))
	if len(attachments) == 0 {
		return false
	}
	status := attachments[0].Send(sel("objectForKey:"), frameStatusKey)
	return status != 0 && objc.Send[int](status, sel("integerValue")) == scFrameStatusComplete
}

func didStop(self objc.ID, _ objc.SEL, _ objc.ID, nsErr objc.ID) {
	v, ok := outputs.Load(self)
	if !ok {
		return
	}
	var err error
	// The user stopping it from the menu bar ends it without an error.
	if nsErr != 0 && objc.Send[int](nsErr, sel("code")) != scStreamErrorUserStopped {
		err = nsError(nsErr)
	}
	v.(*macStream).rec.end(err)
}
