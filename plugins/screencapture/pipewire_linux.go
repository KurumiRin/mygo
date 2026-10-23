//go:build linux && (amd64 || arm64)

package screencapture

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// PipeWire brings the frames of a screencast: libpipewire-0.3, loaded at
// run time, connects to the remote the portal opened, and a stream takes
// the node of what the user picked. The stream's events run on the
// thread that runs its main loop, the capture's own.

var (
	pwOnce sync.Once
	pwErr  error

	pwInit                func(argc, argv ptr)
	pwMainLoopNew         func(props ptr) ptr
	pwMainLoopGetLoop     func(loop ptr) ptr
	pwMainLoopRun         func(loop ptr) int32
	pwMainLoopQuit        func(loop ptr) int32
	pwMainLoopDestroy     func(loop ptr)
	pwContextNew          func(loop, props ptr, size uintptr) ptr
	pwContextConnectFd    func(ctx ptr, fd int32, props ptr, size uintptr) ptr
	pwContextDestroy      func(ctx ptr)
	pwCoreDisconnect      func(core ptr) int32
	pwPropertiesNewString func(s *byte) ptr
	pwStreamNew           func(core ptr, name *byte, props ptr) ptr
	pwStreamAddListener   func(stream, hook, events, data ptr)
	pwStreamConnect       func(stream ptr, direction uint32, target uint32, flags uint32, params ptr, n uint32) int32
	pwStreamUpdateParams  func(stream ptr, params ptr, n uint32) int32
	pwStreamDequeueBuffer func(stream ptr) unsafe.Pointer
	pwStreamQueueBuffer   func(stream, buffer ptr) int32
	pwStreamDisconnect    func(stream ptr) int32
	pwStreamDestroy       func(stream ptr)

	// The stream's events, made once and routed by their data.
	pwStateChanged, pwParamChanged, pwProcess ptr
	pwStreamsMu                               sync.Mutex
	pwStreams                                 = map[ptr]*pwStream{}
	pwStreamIDs                               ptr
)

const (
	pwDirectionInput          = 0
	pwStreamFlagAutoconnect   = 1 << 0
	pwStreamFlagMapBuffers    = 1 << 2
	pwStreamFlagDontReconnect = 1 << 7

	pwStreamStateError       = -1
	pwStreamStateUnconnected = 0
	pwStreamStateStreaming   = 3

	spaChunkFlagCorrupted = 1
)

func loadPipeWire() error {
	pwOnce.Do(func() {
		lib, err := dlopen("libpipewire-0.3.so.0")
		if err != nil {
			pwErr = err
			return
		}
		for _, f := range []struct {
			fn   any
			name string
		}{
			{&pwInit, "pw_init"},
			{&pwMainLoopNew, "pw_main_loop_new"},
			{&pwMainLoopGetLoop, "pw_main_loop_get_loop"},
			{&pwMainLoopRun, "pw_main_loop_run"},
			{&pwMainLoopQuit, "pw_main_loop_quit"},
			{&pwMainLoopDestroy, "pw_main_loop_destroy"},
			{&pwContextNew, "pw_context_new"},
			{&pwContextConnectFd, "pw_context_connect_fd"},
			{&pwContextDestroy, "pw_context_destroy"},
			{&pwCoreDisconnect, "pw_core_disconnect"},
			{&pwPropertiesNewString, "pw_properties_new_string"},
			{&pwStreamNew, "pw_stream_new"},
			{&pwStreamAddListener, "pw_stream_add_listener"},
			{&pwStreamConnect, "pw_stream_connect"},
			{&pwStreamUpdateParams, "pw_stream_update_params"},
			{&pwStreamDequeueBuffer, "pw_stream_dequeue_buffer"},
			{&pwStreamQueueBuffer, "pw_stream_queue_buffer"},
			{&pwStreamDisconnect, "pw_stream_disconnect"},
			{&pwStreamDestroy, "pw_stream_destroy"},
		} {
			sym, err := purego.Dlsym(lib, f.name)
			if err != nil || sym == 0 {
				pwErr = fmt.Errorf("screencapture: missing %s: %w", f.name, errors.ErrUnsupported)
				return
			}
			purego.RegisterFunc(f.fn, sym)
		}
		pwInit(0, 0)
		pwStateChanged = purego.NewCallback(func(data, old, state, msg ptr) {
			if s := lookupStream(data); s != nil {
				s.stateChanged(int32(state), goStr(msg))
			}
		})
		pwParamChanged = purego.NewCallback(func(data, id, param ptr) {
			if s := lookupStream(data); s != nil && uint32(id) == spaParamFormat && param != 0 {
				s.formatChanged(param)
			}
		})
		pwProcess = purego.NewCallback(func(data ptr) {
			if s := lookupStream(data); s != nil {
				s.process()
			}
		})
	})
	return pwErr
}

func lookupStream(id ptr) *pwStream {
	pwStreamsMu.Lock()
	defer pwStreamsMu.Unlock()
	return pwStreams[id]
}

// pwStreamEvents mirrors struct pw_stream_events of version 0.
type pwStreamEvents struct {
	version      uint32
	_            uint32
	destroy      ptr
	stateChanged ptr
	controlInfo  ptr
	ioChanged    ptr
	paramChanged ptr
	addBuffer    ptr
	removeBuffer ptr
	process      ptr
	drained      ptr
}

// pwStream is the PipeWire stream of a recording.
type pwStream struct {
	id     ptr
	rec    *Recording
	loop   ptr
	stream ptr
	format videoFormat
	err    error
	// streamed is set once the stream has streamed.
	streamed bool
	// The listener of the stream's events, which PipeWire links into its
	// list: it must stay where it is until the stream is destroyed.
	hook   *[6]ptr
	events *pwStreamEvents
}

// runPipeWire receives the frames of node from the PipeWire remote fd,
// which it takes, for rec, calling ready once the stream is connected. It
// runs the main loop on the calling thread, locked to its goroutine,
// until ctl quits it or the stream ends, and returns the error that ended
// the stream.
func runPipeWire(fd int32, node uint32, rec *Recording, ctl *pwControl, ready func()) error {
	if err := loadPipeWire(); err != nil {
		closeFd(fd)
		return err
	}
	s := &pwStream{rec: rec, hook: new([6]ptr), events: &pwStreamEvents{
		stateChanged: pwStateChanged,
		paramChanged: pwParamChanged,
		process:      pwProcess,
	}}
	s.loop = pwMainLoopNew(0)
	if s.loop == 0 {
		closeFd(fd)
		return errors.New("screencapture: cannot create a PipeWire loop")
	}
	defer pwMainLoopDestroy(s.loop)
	ctl.set(s.loop)
	defer ctl.set(0)
	ctx := pwContextNew(pwMainLoopGetLoop(s.loop), 0, 0)
	if ctx == 0 {
		closeFd(fd)
		return errors.New("screencapture: cannot create a PipeWire context")
	}
	defer pwContextDestroy(ctx)
	core := pwContextConnectFd(ctx, fd, 0, 0) // takes the fd
	if core == 0 {
		return errors.New("screencapture: cannot connect to PipeWire")
	}
	defer pwCoreDisconnect(core)
	props := pwPropertiesNewString(cs("media.type=Video media.category=Capture media.role=Screen"))
	s.stream = pwStreamNew(core, cs("mygo-screencapture"), props)
	if s.stream == 0 {
		return errors.New("screencapture: cannot create a PipeWire stream")
	}
	defer pwStreamDestroy(s.stream)

	pwStreamsMu.Lock()
	pwStreamIDs++
	s.id = pwStreamIDs
	pwStreams[s.id] = s
	pwStreamsMu.Unlock()
	defer func() {
		pwStreamsMu.Lock()
		delete(pwStreams, s.id)
		pwStreamsMu.Unlock()
	}()
	pwStreamAddListener(s.stream, ptr(unsafe.Pointer(s.hook)), ptr(unsafe.Pointer(s.events)), s.id)

	format := enumFormatPod()
	params := []ptr{ptr(unsafe.Pointer(&format[0]))}
	if res := pwStreamConnect(s.stream, pwDirectionInput, node, pwStreamFlagAutoconnect|pwStreamFlagMapBuffers|pwStreamFlagDontReconnect, ptr(unsafe.Pointer(&params[0])), 1); res < 0 {
		return fmt.Errorf("screencapture: cannot connect the PipeWire stream: %d", res)
	}
	runtime.KeepAlive(format)
	ready()
	pwMainLoopRun(s.loop)
	pwStreamDisconnect(s.stream)
	runtime.KeepAlive(s)
	return s.err
}

func (s *pwStream) stateChanged(state int32, msg string) {
	switch state {
	case pwStreamStateStreaming:
		s.streamed = true
	case pwStreamStateError:
		// Once streaming, the stream fails when what was shared is gone,
		// or the user stopped sharing it: it does not wait for it to come
		// back (pwStreamFlagDontReconnect).
		if !s.streamed {
			s.err = fmt.Errorf("screencapture: PipeWire: %s", msg)
		}
		pwMainLoopQuit(s.loop)
	case pwStreamStateUnconnected:
		pwMainLoopQuit(s.loop)
	}
}

func (s *pwStream) formatChanged(param ptr) {
	p := *(*unsafe.Pointer)(unsafe.Pointer(&param))
	b := unsafe.Slice((*byte)(p), 8+int(*(*uint32)(p)))
	f, err := parseFormat(b)
	if err != nil {
		s.err = err
		pwMainLoopQuit(s.loop)
		return
	}
	s.format = f
	buffers := buffersPod()
	params := []ptr{ptr(unsafe.Pointer(&buffers[0]))}
	pwStreamUpdateParams(s.stream, ptr(unsafe.Pointer(&params[0])), 1)
	runtime.KeepAlive(buffers)
}

// The layouts of struct pw_buffer, spa_buffer, spa_data and spa_chunk.
type (
	pwBuffer struct {
		buffer *spaBuffer
	}
	spaBuffer struct {
		nMetas, nDatas uint32
		metas          ptr
		datas          *spaData
	}
	spaData struct {
		typ, flags         uint32
		fd                 int64
		mapOffset, maxSize uint32
		data               ptr
		chunk              *spaChunk
	}
	spaChunk struct {
		offset, size  uint32
		stride, flags int32
	}
)

func (s *pwStream) process() {
	// Only the newest buffer matters.
	var buf *pwBuffer
	for {
		b := (*pwBuffer)(pwStreamDequeueBuffer(s.stream))
		if b == nil {
			break
		}
		if buf != nil {
			pwStreamQueueBuffer(s.stream, ptr(unsafe.Pointer(buf)))
		}
		buf = b
	}
	if buf == nil {
		return
	}
	defer pwStreamQueueBuffer(s.stream, ptr(unsafe.Pointer(buf)))
	if buf.buffer == nil || buf.buffer.nDatas == 0 {
		return
	}
	d := buf.buffer.datas
	if d.data == 0 || d.chunk == nil || d.chunk.size == 0 || d.chunk.flags&spaChunkFlagCorrupted != 0 {
		return
	}
	w, h := int(s.format.width), int(s.format.height)
	stride := int(d.chunk.stride)
	if stride <= 0 {
		stride = w * 4
	}
	n := int(d.chunk.size)
	if room := int(d.maxSize) - int(d.chunk.offset); n > room {
		n = room
	}
	if n < (h-1)*stride+w*4 {
		return
	}
	pix := unsafe.Slice((*byte)(unsafe.Add(*(*unsafe.Pointer)(unsafe.Pointer(&d.data)), d.chunk.offset)), n)
	order := orderBGRA
	if s.format.format == spaVideoFormatRGBx || s.format.format == spaVideoFormatRGBA {
		order = orderRGBA
	}
	s.rec.deliver(pix, w, h, stride, order)
}
