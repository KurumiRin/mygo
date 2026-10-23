//go:build windows && (amd64 || arm64)

package screencapture

import (
	"context"
	"errors"
	"fmt"
	"image"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/internal/gpu/d3d11/device"
)

// Windows captures with Windows.Graphics.Capture, through its COM
// interfaces: a GraphicsCaptureItem of the monitor or the window, a
// free-threaded frame pool of Direct3D 11 textures, and a session. The
// capture's own thread takes the newest frame of the pool, copies it to a
// texture the CPU reads, and passes it on.

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	iidGraphicsCaptureItemInterop         = guid{0x3628e81b, 0x3cac, 0x4c60, [8]byte{0xb7, 0xf4, 0x23, 0xce, 0x0e, 0x0c, 0x33, 0x56}}
	iidGraphicsCaptureItem                = guid{0x79c3f95b, 0x31f7, 0x4ec2, [8]byte{0xa4, 0x64, 0x63, 0x2e, 0xf5, 0xd3, 0x07, 0x60}}
	iidDirect3D11CaptureFramePoolStatics2 = guid{0x589b103f, 0x6bbc, 0x5df5, [8]byte{0xa9, 0x91, 0x02, 0xe2, 0x8b, 0x3b, 0x66, 0xd5}}
	iidGraphicsCaptureSessionStatics      = guid{0x2224a540, 0x5974, 0x49aa, [8]byte{0xb2, 0x32, 0x08, 0x82, 0x53, 0x6f, 0x4c, 0xb5}}
	iidGraphicsCaptureSession2            = guid{0x2c39ae40, 0x7d2e, 0x5044, [8]byte{0x80, 0x4e, 0x8b, 0x67, 0x99, 0xd4, 0xcf, 0x9e}}
	iidClosable                           = guid{0x30d5a829, 0x7fa4, 0x4026, [8]byte{0x83, 0xbb, 0xd7, 0x5b, 0xae, 0x4e, 0xa9, 0x9e}}
	iidDirect3DDxgiInterfaceAccess        = guid{0xa9b3d012, 0x3df2, 0x4ee3, [8]byte{0xb8, 0xd1, 0x86, 0x95, 0xf4, 0x57, 0xd3, 0xc1}}
	iidDirect3DDevice                     = guid{0xa37624ab, 0x8d5f, 0x4650, [8]byte{0x9d, 0x3e, 0x9e, 0xae, 0x3d, 0x9b, 0xc6, 0x70}}
	iidDXGIDevice                         = guid{0x54ec77fa, 0x1377, 0x44e6, [8]byte{0x8c, 0x32, 0x88, 0xfd, 0x5f, 0x44, 0xc8, 0x4c}}
	iidD3D11Texture2D                     = guid{0x6f15aaf2, 0xd208, 0x4e89, [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
	iidD3D11Multithread                   = guid{0x9b7e4e00, 0x342c, 0x4106, [8]byte{0xa1, 0x9f, 0x4f, 0x27, 0x04, 0xf6, 0x89, 0xf0}}
)

// The methods' indexes in their vtables: IUnknown has 3, IInspectable 6.
const (
	vtQueryInterface = 0
	vtRelease        = 2

	vtInteropCreateForWindow  = 3
	vtInteropCreateForMonitor = 4
	vtItemGetSize             = 7
	vtStaticsCreateFreeThread = 6
	vtPoolRecreate            = 6
	vtPoolTryGetNextFrame     = 7
	vtPoolCreateCaptureSess   = 10
	vtFrameGetSurface         = 6
	vtFrameGetContentSize     = 8
	vtSessionStartCapture     = 6
	vtSession2PutCursor       = 7
	vtSessionStaticsSupported = 6
	vtClosableClose           = 6
	vtDxgiAccessGetInterface  = 3
	vtMultithreadSetProtected = 5

	vtDeviceCreateTexture2D = 5
	vtContextMap            = 14
	vtContextUnmap          = 15
	vtContextCopyResource   = 47
	vtTexture2DGetDesc      = 10
)

const (
	pixelFormatB8G8R8A8      = 87 // DirectXPixelFormat and DXGI_FORMAT_B8G8R8A8_UNORM
	framePoolBuffers         = 2
	usageStaging             = 3
	cpuAccessRead            = 0x20000
	mapRead                  = 1
	roInitMultithreaded      = 1
	rpcEChangedMode          = 0x80010106
	monitorDefaultPrimary    = 1
	dwmwaExtendedFrameBounds = 9
	dwmwaCloaked             = 14
	gwlExStyle               = -20
	wsExToolWindow           = 0x80
	processQueryLimitedInfo  = 0x1000
)

var (
	combase  = syscall.NewLazyDLL(device.SystemDir() + `\combase.dll`)
	d3d11    = syscall.NewLazyDLL(device.SystemDir() + `\d3d11.dll`)
	user32   = syscall.NewLazyDLL(device.SystemDir() + `\user32.dll`)
	dwmapi   = syscall.NewLazyDLL(device.SystemDir() + `\dwmapi.dll`)
	kernel32 = syscall.NewLazyDLL(device.SystemDir() + `\kernel32.dll`)

	procRoInitialize                         = combase.NewProc("RoInitialize")
	procRoGetActivationFactory               = combase.NewProc("RoGetActivationFactory")
	procWindowsCreateString                  = combase.NewProc("WindowsCreateString")
	procWindowsDeleteString                  = combase.NewProc("WindowsDeleteString")
	procCreateDirect3D11DeviceFromDXGIDevice = d3d11.NewProc("CreateDirect3D11DeviceFromDXGIDevice")
	procMonitorFromPoint                     = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW                      = user32.NewProc("GetMonitorInfoW")
	procIsWindow                             = user32.NewProc("IsWindow")
	procIsWindowVisible                      = user32.NewProc("IsWindowVisible")
	procEnumWindows                          = user32.NewProc("EnumWindows")
	procGetWindowTextW                       = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW                 = user32.NewProc("GetWindowTextLengthW")
	procGetWindowLongPtrW                    = user32.NewProc("GetWindowLongPtrW")
	procGetWindowThreadProcessID             = user32.NewProc("GetWindowThreadProcessId")
	procGetDpiForWindow                      = user32.NewProc("GetDpiForWindow")
	procGetShellWindow                       = user32.NewProc("GetShellWindow")
	procDwmGetWindowAttribute                = dwmapi.NewProc("DwmGetWindowAttribute")
	procOpenProcess                          = kernel32.NewProc("OpenProcess")
	procCloseHandle                          = kernel32.NewProc("CloseHandle")
	procQueryFullProcessImageNameW           = kernel32.NewProc("QueryFullProcessImageNameW")
)

// word reads the i-th pointer-sized word at p, memory COM owns.
func word(p uintptr, i int) uintptr {
	return (*[1 << 16]uintptr)(*(*unsafe.Pointer)(unsafe.Pointer(&p)))[i]
}

// call calls the i-th method of a COM object.
func call(obj uintptr, i int, args ...uintptr) uintptr {
	fn := word(word(obj, 0), i)
	var a [16]uintptr
	a[0] = obj
	n := copy(a[1:], args)
	r, _, _ := syscall.SyscallN(fn, a[:n+1]...)
	return r
}

func failed(hr uintptr) bool { return int32(uint32(hr)) < 0 }

func release(obj *uintptr) {
	if *obj != 0 {
		call(*obj, vtRelease)
		*obj = 0
	}
}

func queryInterface(obj uintptr, iid *guid) (uintptr, error) {
	var out uintptr
	if hr := call(obj, vtQueryInterface, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out))); failed(hr) {
		return 0, fmt.Errorf("screencapture: no interface: %#x", uint32(hr))
	}
	return out, nil
}

// closeWinRT closes a WinRT object that is IClosable, and releases it.
func closeWinRT(obj *uintptr) {
	if *obj == 0 {
		return
	}
	if c, err := queryInterface(*obj, &iidClosable); err == nil {
		call(c, vtClosableClose)
		release(&c)
	}
	release(obj)
}

// activationFactory returns the factory of a runtime class, as iid.
func activationFactory(class string, iid *guid) (uintptr, error) {
	name, _ := syscall.UTF16FromString(class)
	var hs, factory uintptr
	if hr, _, _ := procWindowsCreateString.Call(uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)-1), uintptr(unsafe.Pointer(&hs))); failed(hr) {
		return 0, fmt.Errorf("screencapture: %#x", uint32(hr))
	}
	defer procWindowsDeleteString.Call(hs)
	if hr, _, _ := procRoGetActivationFactory.Call(hs, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&factory))); failed(hr) {
		return 0, fmt.Errorf("screencapture: no %s: %#x: %w", class, uint32(hr), errors.ErrUnsupported)
	}
	return factory, nil
}

func access() bool        { return true }
func requestAccess() bool { return true }

func screenshot(context.Context, Source, Options) (*image.RGBA, bool, error) { return nil, false, nil }

// sizeInt32 packs a SizeInt32, which is passed in a register.
func sizeInt32(w, h int32) uintptr { return uintptr(uint32(w)) | uintptr(uint32(h))<<32 }

type texture2DDesc struct {
	Width, Height, MipLevels, ArraySize, Format uint32
	SampleCount, SampleQuality                  uint32
	Usage, BindFlags, CPUAccessFlags, MiscFlags uint32
}

type mapped struct {
	Data                 uintptr
	RowPitch, DepthPitch uint32
}

// winCapture is the state of a capture, on its thread.
type winCapture struct {
	rec      *Recording
	window   uintptr // the HWND of a window
	device   uintptr // ID3D11Device
	context  uintptr // ID3D11DeviceContext
	rtDevice uintptr // IDirect3DDevice
	item     uintptr
	pool     uintptr
	session  uintptr
	poolW    int32
	poolH    int32
	staging  uintptr
	stagingW uint32
	stagingH uint32
}

func start(ctx context.Context, src Source, opts Options, r *Recording) (func(), error) {
	ready := make(chan error, 1)
	stop := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if hr, _, _ := procRoInitialize.Call(roInitMultithreaded); failed(hr) && uint32(hr) != rpcEChangedMode {
			ready <- fmt.Errorf("screencapture: cannot initialize WinRT: %#x", uint32(hr))
			return
		}
		c := &winCapture{rec: r}
		defer c.close()
		if err := c.open(src, opts); err != nil {
			ready <- err
			return
		}
		ready <- nil
		c.run(stop, r.interval)
	}()
	if err := <-ready; err != nil {
		<-exited
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-exited
	}, nil
}

func (c *winCapture) open(src Source, opts Options) error {
	if err := procRoGetActivationFactory.Find(); err != nil {
		return fmt.Errorf("screencapture: %w: %w", err, errors.ErrUnsupported)
	}
	statics, err := activationFactory("Windows.Graphics.Capture.GraphicsCaptureSession", &iidGraphicsCaptureSessionStatics)
	if err != nil {
		return err
	}
	var supported byte
	call(statics, vtSessionStaticsSupported, uintptr(unsafe.Pointer(&supported)))
	release(&statics)
	if supported == 0 {
		return fmt.Errorf("screencapture: Windows.Graphics.Capture needs Windows 10, 1903: %w", errors.ErrUnsupported)
	}

	interop, err := activationFactory("Windows.Graphics.Capture.GraphicsCaptureItem", &iidGraphicsCaptureItemInterop)
	if err != nil {
		return err
	}
	defer release(&interop)
	var hr uintptr
	if src.window {
		c.window = uintptr(src.id)
		if ok, _, _ := procIsWindow.Call(c.window); ok == 0 {
			return ErrNotFound
		}
		hr = call(interop, vtInteropCreateForWindow, c.window, uintptr(unsafe.Pointer(&iidGraphicsCaptureItem)), uintptr(unsafe.Pointer(&c.item)))
	} else {
		mon := uintptr(src.id)
		if mon == 0 {
			mon, _, _ = procMonitorFromPoint.Call(0, monitorDefaultPrimary)
		}
		info := make([]byte, 40) // MONITORINFO
		*(*uint32)(unsafe.Pointer(&info[0])) = uint32(len(info))
		if ok, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&info[0]))); ok == 0 {
			return ErrNotFound
		}
		hr = call(interop, vtInteropCreateForMonitor, mon, uintptr(unsafe.Pointer(&iidGraphicsCaptureItem)), uintptr(unsafe.Pointer(&c.item)))
	}
	if failed(hr) || c.item == 0 {
		return fmt.Errorf("screencapture: cannot capture %v: %#x", src, uint32(hr))
	}

	c.device, c.context, _, err = device.New()
	if err != nil {
		return err
	}
	if mt, err := queryInterface(c.device, &iidD3D11Multithread); err == nil {
		call(mt, vtMultithreadSetProtected, 1)
		release(&mt)
	}
	dxgi, err := queryInterface(c.device, &iidDXGIDevice)
	if err != nil {
		return err
	}
	var inspectable uintptr
	hr, _, _ = procCreateDirect3D11DeviceFromDXGIDevice.Call(dxgi, uintptr(unsafe.Pointer(&inspectable)))
	release(&dxgi)
	if failed(hr) {
		return fmt.Errorf("screencapture: cannot wrap the Direct3D device: %#x", uint32(hr))
	}
	c.rtDevice, err = queryInterface(inspectable, &iidDirect3DDevice)
	release(&inspectable)
	if err != nil {
		return err
	}

	var size [2]int32
	call(c.item, vtItemGetSize, uintptr(unsafe.Pointer(&size)))
	c.poolW, c.poolH = max(size[0], 1), max(size[1], 1)
	pools, err := activationFactory("Windows.Graphics.Capture.Direct3D11CaptureFramePool", &iidDirect3D11CaptureFramePoolStatics2)
	if err != nil {
		return err
	}
	hr = call(pools, vtStaticsCreateFreeThread, c.rtDevice, pixelFormatB8G8R8A8, framePoolBuffers, sizeInt32(c.poolW, c.poolH), uintptr(unsafe.Pointer(&c.pool)))
	release(&pools)
	if failed(hr) {
		return fmt.Errorf("screencapture: cannot create a frame pool: %#x", uint32(hr))
	}
	if hr := call(c.pool, vtPoolCreateCaptureSess, c.item, uintptr(unsafe.Pointer(&c.session))); failed(hr) {
		return fmt.Errorf("screencapture: cannot create a capture session: %#x", uint32(hr))
	}
	// Windows 10, 2004 can leave the cursor out.
	if s2, err := queryInterface(c.session, &iidGraphicsCaptureSession2); err == nil {
		show := uintptr(1)
		if opts.HideCursor {
			show = 0
		}
		call(s2, vtSession2PutCursor, show)
		release(&s2)
	}
	if hr := call(c.session, vtSessionStartCapture); failed(hr) {
		return fmt.Errorf("screencapture: cannot start capturing: %#x", uint32(hr))
	}
	return nil
}

// run takes frames until stop is closed or the window is gone.
func (c *winCapture) run(stop <-chan struct{}, interval time.Duration) {
	wait := min(max(interval/4, time.Millisecond), 8*time.Millisecond)
	for {
		select {
		case <-stop:
			return
		default:
		}
		if c.window != 0 {
			if ok, _, _ := procIsWindow.Call(c.window); ok == 0 {
				c.rec.end(nil)
				return
			}
		}
		var frame uintptr
		if hr := call(c.pool, vtPoolTryGetNextFrame, uintptr(unsafe.Pointer(&frame))); failed(hr) {
			c.rec.end(fmt.Errorf("screencapture: cannot take a frame: %#x", uint32(hr)))
			return
		}
		if frame == 0 {
			time.Sleep(wait)
			continue
		}
		c.frame(frame)
		closeWinRT(&frame)
	}
}

// frame passes on a frame of the pool, and makes the pool anew when the
// size of what is captured changed.
func (c *winCapture) frame(frame uintptr) {
	var size [2]int32
	call(frame, vtFrameGetContentSize, uintptr(unsafe.Pointer(&size)))
	if size[0] > 0 && size[1] > 0 && (size[0] != c.poolW || size[1] != c.poolH) {
		c.poolW, c.poolH = size[0], size[1]
		call(c.pool, vtPoolRecreate, c.rtDevice, pixelFormatB8G8R8A8, framePoolBuffers, sizeInt32(c.poolW, c.poolH))
		return // the next frame has the new size
	}
	if !c.rec.wants() {
		return
	}
	var surface, tex uintptr
	if hr := call(frame, vtFrameGetSurface, uintptr(unsafe.Pointer(&surface))); failed(hr) || surface == 0 {
		return
	}
	defer release(&surface)
	dxgiAccess, err := queryInterface(surface, &iidDirect3DDxgiInterfaceAccess)
	if err != nil {
		return
	}
	defer release(&dxgiAccess)
	if hr := call(dxgiAccess, vtDxgiAccessGetInterface, uintptr(unsafe.Pointer(&iidD3D11Texture2D)), uintptr(unsafe.Pointer(&tex))); failed(hr) || tex == 0 {
		return
	}
	defer release(&tex)
	var desc texture2DDesc
	call(tex, vtTexture2DGetDesc, uintptr(unsafe.Pointer(&desc)))
	if c.staging == 0 || c.stagingW != desc.Width || c.stagingH != desc.Height {
		release(&c.staging)
		sd := texture2DDesc{Width: desc.Width, Height: desc.Height, MipLevels: 1, ArraySize: 1, Format: desc.Format,
			SampleCount: 1, Usage: usageStaging, CPUAccessFlags: cpuAccessRead}
		if hr := call(c.device, vtDeviceCreateTexture2D, uintptr(unsafe.Pointer(&sd)), 0, uintptr(unsafe.Pointer(&c.staging))); failed(hr) {
			c.staging = 0
			return
		}
		c.stagingW, c.stagingH = desc.Width, desc.Height
	}
	call(c.context, vtContextCopyResource, c.staging, tex)
	var m mapped
	if hr := call(c.context, vtContextMap, c.staging, 0, mapRead, 0, uintptr(unsafe.Pointer(&m))); failed(hr) {
		return
	}
	defer call(c.context, vtContextUnmap, c.staging, 0)
	w := min(int(size[0]), int(desc.Width))
	h := min(int(size[1]), int(desc.Height))
	stride := int(m.RowPitch)
	data := *(*unsafe.Pointer)(unsafe.Pointer(&m.Data))
	if data == nil || w <= 0 || h <= 0 {
		return
	}
	c.rec.deliver(unsafe.Slice((*byte)(data), (h-1)*stride+w*4), w, h, stride, orderBGRA)
}

func (c *winCapture) close() {
	closeWinRT(&c.session)
	closeWinRT(&c.pool)
	closeWinRT(&c.item)
	release(&c.staging)
	closeWinRT(&c.rtDevice)
	release(&c.context)
	release(&c.device)
}

var (
	enumOnce     sync.Once
	enumCallback uintptr
	enumMu       sync.Mutex
	enumFound    []uintptr
)

func windows(context.Context) ([]Window, error) {
	enumOnce.Do(func() {
		enumCallback = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
			enumFound = append(enumFound, hwnd)
			return 1
		})
	})
	enumMu.Lock()
	enumFound = nil
	procEnumWindows.Call(enumCallback, 0)
	found := enumFound
	enumFound = nil
	enumMu.Unlock()

	shell, _, _ := procGetShellWindow.Call()
	var out []Window
	for _, hwnd := range found { // in z-order, front first
		if hwnd == shell {
			continue
		}
		if v, _, _ := procIsWindowVisible.Call(hwnd); v == 0 {
			continue
		}
		var cloaked uint32
		if hr, _, _ := procDwmGetWindowAttribute.Call(hwnd, dwmwaCloaked, uintptr(unsafe.Pointer(&cloaked)), 4); !failed(hr) && cloaked != 0 {
			continue
		}
		ex, _, _ := procGetWindowLongPtrW.Call(hwnd, ^uintptr(-gwlExStyle-1)) // GWL_EXSTYLE, negative
		if ex&wsExToolWindow != 0 {
			continue
		}
		title := windowText(hwnd)
		if title == "" {
			continue
		}
		var r [4]int32
		if hr, _, _ := procDwmGetWindowAttribute.Call(hwnd, dwmwaExtendedFrameBounds, uintptr(unsafe.Pointer(&r)), 16); failed(hr) {
			continue
		}
		if r[2] <= r[0] || r[3] <= r[1] {
			continue
		}
		dpi := 96
		if procGetDpiForWindow.Find() == nil {
			if d, _, _ := procGetDpiForWindow.Call(hwnd); d != 0 {
				dpi = int(d)
			}
		}
		dip := func(v int32) int { return (int(v)*96 + dpi/2) / dpi }
		out = append(out, Window{
			ID:     uint64(hwnd),
			Title:  title,
			App:    windowApp(hwnd),
			Bounds: mygo.Rectangle{X: dip(r[0]), Y: dip(r[1]), Width: dip(r[2] - r[0]), Height: dip(r[3] - r[1])},
		})
	}
	return out, nil
}

func windowText(hwnd uintptr) string {
	n, _, _ := procGetWindowTextLengthW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	n, _, _ = procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

// windowApp returns the name of the program of a window's process.
func windowApp(hwnd uintptr) string {
	var pid uint32
	procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	h, _, _ := procOpenProcess.Call(processQueryLimitedInfo, 0, uintptr(pid))
	if h == 0 {
		return ""
	}
	defer procCloseHandle.Call(h)
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	if ok, _, _ := procQueryFullProcessImageNameW.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); ok == 0 {
		return ""
	}
	name := filepath.Base(syscall.UTF16ToString(buf[:n]))
	return strings.TrimSuffix(name, filepath.Ext(name))
}
