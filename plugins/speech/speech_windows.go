//go:build windows && (amd64 || arm64)

package speech

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// Windows recognizes with the continuous dictation of
// Windows.Media.SpeechRecognition, a WinRT API, called through its vtables:
// a SpeechRecognizer without constraints, which dictates, and its
// continuous recognition session. Hypotheses come as the user speaks, a
// result after each phrase, and the session completes on Stop, a silence
// or a failure. WinRT objects live in the multithreaded apartment, which a
// thread of the package's keeps; their events arrive on its threads.
//
// Dictation needs online speech recognition, which the user turns on in
// Settings > Privacy > Speech, and the microphone allowed for desktop
// apps. Windows doesn't prompt unpackaged apps for either: they fail.

type guid struct {
	data1        uint32
	data2, data3 uint16
	data4        [8]byte
}

func mustGUID(s string) guid {
	s = strings.ReplaceAll(s, "-", "")
	var b [16]byte
	for i := range b {
		var v byte
		for _, c := range s[2*i : 2*i+2] {
			switch {
			case c >= '0' && c <= '9':
				v = v<<4 | byte(c-'0')
			case c >= 'a' && c <= 'f':
				v = v<<4 | byte(c-'a'+10)
			default:
				panic("speech: invalid GUID " + s)
			}
		}
		b[i] = v
	}
	g := guid{
		data1: uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]),
		data2: uint16(b[4])<<8 | uint16(b[5]),
		data3: uint16(b[6])<<8 | uint16(b[7]),
	}
	copy(g.data4[:], b[8:])
	return g
}

var (
	iidIUnknown                 = mustGUID("00000000-0000-0000-c000-000000000046")
	iidIAgileObject             = mustGUID("94ea2b94-e9cc-49e0-c0ff-ee64ca8f5b90")
	iidIAsyncInfo               = mustGUID("00000036-0000-0000-c000-000000000046")
	iidIClosable                = mustGUID("30d5a829-7fa4-4026-83bb-d75bae4ea99e")
	iidILanguageFactory         = mustGUID("9b0252ac-0c27-44f8-b792-9793fb66c63e")
	iidISpeechRecognizer        = mustGUID("0bc3c9cb-c26a-40f2-aeb5-8096b2e48073")
	iidISpeechRecognizer2       = mustGUID("63c9baf1-91e3-4ea4-86a1-7c3867d084a6")
	iidISpeechRecognizerFactory = mustGUID("60c488dd-7fb8-4033-ac70-d046f64818e1")

	// The delegates'. Those of generic delegates are derived from their
	// type signatures (TestDelegateIIDs).
	iidAsyncActionCompleted = mustGUID("a4ed5c81-76c9-40bd-8be6-b1d90fb20ae7")
	// AsyncOperationCompletedHandler<SpeechRecognitionCompilationResult>
	iidCompileCompleted = mustGUID("78c859bd-14d4-5c40-abff-490616d5e92d")
	// TypedEventHandler<SpeechRecognizer, SpeechRecognitionHypothesisGeneratedEventArgs>
	iidHypothesisHandler = mustGUID("4cb45aba-7573-545a-b29a-e9be35bd4682")
	// TypedEventHandler<SpeechContinuousRecognitionSession, SpeechContinuousRecognitionResultGeneratedEventArgs>
	iidResultHandler = mustGUID("26192073-a2c9-527d-9bd3-911c05e0011e")
	// TypedEventHandler<SpeechContinuousRecognitionSession, SpeechContinuousRecognitionCompletedEventArgs>
	iidCompletedHandler = mustGUID("8103c018-7952-59f9-9f41-23b17d6e452d")
)

// Vtable slots, after IInspectable's six.
const (
	slotQueryInterface = 0
	slotRelease        = 2

	slotLanguageTag    = 6 // ILanguage
	slotCreateLanguage = 6 // ILanguageFactory
	slotCreate         = 6 // ISpeechRecognizerFactory
	slotClose          = 6 // IClosable

	slotCurrentLanguage  = 6 // ISpeechRecognizer
	slotCompile          = 10
	slotContinuous       = 6 // ISpeechRecognizer2
	slotAddHypothesis    = 9
	slotRemoveHypothesis = 10

	slotStartAsync      = 8 // ISpeechContinuousRecognitionSession
	slotStopAsync       = 10
	slotCancelAsync     = 11
	slotAddCompleted    = 14
	slotRemoveCompleted = 15
	slotAddResult       = 16
	slotRemoveResult    = 17

	slotArgsValue = 6 // the event args' Hypothesis, Result and Status
	slotText      = 6 // ISpeechRecognitionHypothesis
	slotStatus    = 6 // ISpeechRecognitionResult, ISpeechRecognitionCompilationResult
	slotResText   = 7 // ISpeechRecognitionResult

	slotPutCompleted = 6 // IAsyncAction, IAsyncOperation
	slotGetResults   = 8
	slotErrorCode    = 8 // IAsyncInfo
)

// SpeechRecognitionResultStatus
const (
	statusSuccess               = 0
	statusTopicLanguage         = 1
	statusAudioQuality          = 4
	statusUserCanceled          = 5
	statusTimeoutExceeded       = 7
	statusPauseLimitExceeded    = 8
	statusNetworkFailure        = 9
	statusMicrophoneUnavailable = 10
)

const (
	asyncCompleted = 1
	asyncError     = 3

	eNoInterface        = 0x80004002
	eAccessDenied       = 0x80070005
	ePrivacyNotAccepted = 0x80045509 // SPERR_SPEECH_PRIVACY_POLICY_NOT_ACCEPTED
)

var (
	combase = syscall.NewLazyDLL("combase.dll")

	roInitialize           = combase.NewProc("RoInitialize")
	roActivateInstance     = combase.NewProc("RoActivateInstance")
	roGetActivationFactory = combase.NewProc("RoGetActivationFactory")
	createString           = combase.NewProc("WindowsCreateString")
	deleteString           = combase.NewProc("WindowsDeleteString")
	stringBuffer           = combase.NewProc("WindowsGetStringRawBuffer")
)

func native(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }

// comCall calls method i of the COM interface obj. Pointers converted to
// uintptr in the call stay valid until it returns (go:uintptrescapes).
//
//go:uintptrescapes
func comCall(obj uintptr, i int, args ...uintptr) uintptr {
	vtbl := *(*uintptr)(native(obj))
	fn := *(*uintptr)(unsafe.Add(native(vtbl), i*int(unsafe.Sizeof(uintptr(0)))))
	var a [8]uintptr
	a[0] = obj
	n := copy(a[1:], args)
	r, _, _ := syscall.SyscallN(fn, a[:n+1]...)
	return r
}

func failed(hr uintptr) bool { return int32(uint32(hr)) < 0 }

func release(obj uintptr) {
	if obj != 0 {
		comCall(obj, slotRelease)
	}
}

func queryInterface(obj uintptr, iid *guid) uintptr {
	var out uintptr
	if obj == 0 || failed(comCall(obj, slotQueryInterface, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))) {
		return 0
	}
	return out
}

// getObject calls a method that returns an object.
func getObject(obj uintptr, slot int) uintptr {
	var out uintptr
	if failed(comCall(obj, slot, uintptr(unsafe.Pointer(&out)))) {
		return 0
	}
	return out
}

// callAsync calls a method that starts an async action or operation, and
// returns it or what failed.
func callAsync(obj uintptr, slot int) (uintptr, error) {
	var op uintptr
	if hr := comCall(obj, slot, uintptr(unsafe.Pointer(&op))); failed(hr) {
		return 0, hresultError(hr)
	}
	if op == 0 {
		return 0, errors.New("speech: speech recognition did not start")
	}
	return op, nil
}

func getInt32(obj uintptr, slot int) int32 {
	var v int32
	if failed(comCall(obj, slot, uintptr(unsafe.Pointer(&v)))) {
		return -1
	}
	return v
}

func hstring(s string) (uintptr, error) {
	u := utf16.Encode([]rune(s))
	var p *uint16
	if len(u) > 0 {
		p = &u[0]
	}
	var h uintptr
	if hr, _, _ := createString.Call(uintptr(unsafe.Pointer(p)), uintptr(len(u)), uintptr(unsafe.Pointer(&h))); failed(hr) {
		return 0, hresultError(hr)
	}
	return h, nil
}

func freeHString(h uintptr) {
	if h != 0 {
		deleteString.Call(h)
	}
}

// getString calls a method that returns an HSTRING, and frees it.
func getString(obj uintptr, slot int) string {
	var h uintptr
	if failed(comCall(obj, slot, uintptr(unsafe.Pointer(&h)))) || h == 0 {
		return ""
	}
	defer freeHString(h)
	var n uint32
	p, _, _ := stringBuffer.Call(h, uintptr(unsafe.Pointer(&n)))
	if p == 0 {
		return ""
	}
	return string(utf16.Decode(unsafe.Slice((*uint16)(native(p)), n)))
}

func hresultError(hr uintptr) error {
	switch uint32(hr) {
	case ePrivacyNotAccepted:
		return fmt.Errorf("%w: turn on online speech recognition in Settings > Privacy > Speech", ErrDenied)
	case eAccessDenied:
		return fmt.Errorf("%w: let desktop apps use the microphone in Settings > Privacy > Microphone", ErrDenied)
	}
	return fmt.Errorf("speech: speech recognition failed (HRESULT %#08x)", uint32(hr))
}

func activationFactory(class string, iid *guid) (uintptr, error) {
	h, err := hstring(class)
	if err != nil {
		return 0, err
	}
	defer freeHString(h)
	var f uintptr
	if hr, _, _ := roGetActivationFactory.Call(h, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&f))); failed(hr) {
		return 0, hresultError(hr)
	}
	return f, nil
}

// The thread of the package's: it keeps the multithreaded apartment, and
// makes the calls that create, start and end recognitions.
var (
	mtaOnce sync.Once
	mtaOK   bool
	mtaWork chan func()
)

func onMTA(fn func()) bool {
	mtaOnce.Do(func() {
		if combase.Load() != nil {
			return
		}
		mtaWork = make(chan func())
		ready := make(chan bool)
		go func() {
			runtime.LockOSThread()
			// RO_INIT_MULTITHREADED
			hr, _, _ := roInitialize.Call(1)
			ready <- !failed(hr)
			for fn := range mtaWork {
				fn()
			}
		}()
		mtaOK = <-ready
	})
	if !mtaOK {
		return false
	}
	done := make(chan struct{})
	mtaWork <- func() {
		defer close(done)
		fn()
	}
	<-done
	return true
}

func platformSupported() bool {
	ok := false
	onMTA(func() {
		if f, err := activationFactory("Windows.Media.SpeechRecognition.SpeechRecognizer", &iidISpeechRecognizerFactory); err == nil {
			release(f)
			ok = true
		}
	})
	return ok
}

// platformAuthorize has nothing to ask: Windows has no prompt for
// unpackaged apps, and what Settings refuse shows only once a recognition
// starts, which platformStart reports as ErrDenied.
func platformAuthorize(context.Context) error { return nil }

// A delegate is a WinRT delegate backed by a Go function: an event
// handler or a completion handler, called on WinRT's threads. All share
// one vtable, whose callbacks are made once.
type delegate struct {
	vtbl *[4]uintptr
	refs int32
	iid  guid
	fn   func(a, b uintptr)
}

var (
	delegateOnce sync.Once
	delegateVtbl [4]uintptr
	delegatesMu  sync.Mutex
	delegates    = map[uintptr]*delegate{}
)

func newDelegate(iid guid, fn func(a, b uintptr)) uintptr {
	delegateOnce.Do(func() {
		delegateVtbl = [4]uintptr{
			syscall.NewCallback(func(this, riid, out uintptr) uintptr {
				iid := *(*guid)(native(riid))
				delegatesMu.Lock()
				d := delegates[this]
				delegatesMu.Unlock()
				if d == nil || iid != d.iid && iid != iidIUnknown && iid != iidIAgileObject {
					*(*uintptr)(native(out)) = 0
					return eNoInterface
				}
				*(*uintptr)(native(out)) = this
				delegateRef(this, 1)
				return 0
			}),
			syscall.NewCallback(func(this uintptr) uintptr { return uintptr(delegateRef(this, 1)) }),
			syscall.NewCallback(func(this uintptr) uintptr { return uintptr(delegateRef(this, -1)) }),
			syscall.NewCallback(func(this, a, b uintptr) uintptr {
				delegatesMu.Lock()
				d := delegates[this]
				delegatesMu.Unlock()
				if d != nil {
					d.fn(a, b)
				}
				return 0
			}),
		}
	})
	d := &delegate{vtbl: &delegateVtbl, refs: 1, iid: iid, fn: fn}
	p := uintptr(unsafe.Pointer(d))
	delegatesMu.Lock()
	delegates[p] = d
	delegatesMu.Unlock()
	return p
}

func delegateRef(this uintptr, delta int32) int32 {
	delegatesMu.Lock()
	defer delegatesMu.Unlock()
	d := delegates[this]
	if d == nil {
		return 0
	}
	d.refs += delta
	if d.refs <= 0 {
		delete(delegates, this)
		return 0
	}
	return d.refs
}

// await waits for the IAsyncAction or IAsyncOperation op to complete, and
// returns its result object, if it has one, or what failed.
func await(op uintptr, iid guid, hasResult bool) (uintptr, error) {
	status := make(chan uint32, 1)
	h := newDelegate(iid, func(_, s uintptr) { status <- uint32(s) })
	hr := comCall(op, slotPutCompleted, h)
	release(h)
	if failed(hr) {
		return 0, hresultError(hr)
	}
	switch <-status {
	case asyncCompleted:
	case asyncError:
		var code int32 = -1
		if info := queryInterface(op, &iidIAsyncInfo); info != 0 {
			comCall(info, slotErrorCode, uintptr(unsafe.Pointer(&code)))
			release(info)
		}
		return 0, hresultError(uintptr(uint32(code)))
	default:
		return 0, context.Canceled
	}
	if !hasResult {
		return 0, nil
	}
	var out uintptr
	if hr := comCall(op, slotGetResults, uintptr(unsafe.Pointer(&out))); failed(hr) {
		return 0, hresultError(hr)
	}
	return out, nil
}

// winSession is a recognition: the objects are owned, and released once
// it ended.
type winSession struct {
	emit func(string, bool)
	end  func(error)

	recognizer, recognizer2, session      uintptr
	hypothesisToken, resultToken, doneTok int64
	spaced                                bool // whether phrases are separated by spaces

	mu                 sync.Mutex
	committed          string // the text of the phrases recognized
	canceled, finished bool
}

func platformStart(ctx context.Context, opts Options, emit func(string, bool), end func(error)) (session, error) {
	s := &winSession{emit: emit, end: end}
	var err error
	if !onMTA(func() {
		if err = s.start(opts); err != nil {
			// The handlers must not end what never started.
			s.mu.Lock()
			s.finished = true
			s.mu.Unlock()
			s.release()
		}
	}) {
		return nil, ErrUnsupported
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *winSession) start(opts Options) error {
	if opts.OnDevice {
		return fmt.Errorf("%w: on-device recognition", ErrUnsupported)
	}
	if opts.Locale != "" {
		lf, err := activationFactory("Windows.Globalization.Language", &iidILanguageFactory)
		if err != nil {
			return err
		}
		tag, err := hstring(opts.Locale)
		if err != nil {
			release(lf)
			return err
		}
		var lang uintptr
		hr := comCall(lf, slotCreateLanguage, tag, uintptr(unsafe.Pointer(&lang)))
		freeHString(tag)
		release(lf)
		if failed(hr) {
			return ErrLanguage
		}
		rf, err := activationFactory("Windows.Media.SpeechRecognition.SpeechRecognizer", &iidISpeechRecognizerFactory)
		if err != nil {
			release(lang)
			return err
		}
		hr = comCall(rf, slotCreate, lang, uintptr(unsafe.Pointer(&s.recognizer)))
		release(rf)
		release(lang)
		if failed(hr) {
			return fmt.Errorf("%w (HRESULT %#08x)", ErrLanguage, uint32(hr))
		}
	} else {
		class, err := hstring("Windows.Media.SpeechRecognition.SpeechRecognizer")
		if err != nil {
			return err
		}
		var insp uintptr
		hr, _, _ := roActivateInstance.Call(class, uintptr(unsafe.Pointer(&insp)))
		freeHString(class)
		if failed(hr) {
			return hresultError(hr)
		}
		s.recognizer = queryInterface(insp, &iidISpeechRecognizer)
		release(insp)
	}
	if s.recognizer == 0 {
		return ErrUnsupported
	}
	if lang := getObject(s.recognizer, slotCurrentLanguage); lang != 0 {
		tag := strings.ToLower(getString(lang, slotLanguageTag))
		release(lang)
		s.spaced = !strings.HasPrefix(tag, "zh") && !strings.HasPrefix(tag, "ja") && !strings.HasPrefix(tag, "th")
	}
	if s.recognizer2 = queryInterface(s.recognizer, &iidISpeechRecognizer2); s.recognizer2 == 0 {
		return ErrUnsupported
	}

	// No constraints: the dictation of the language.
	op, err := callAsync(s.recognizer, slotCompile)
	if err != nil {
		return err
	}
	compiled, err := await(op, iidCompileCompleted, true)
	release(op)
	if err != nil {
		return err
	}
	status := getInt32(compiled, slotStatus)
	release(compiled)
	switch status {
	case statusSuccess:
	case statusTopicLanguage:
		return ErrLanguage
	default:
		return statusError(status)
	}

	if s.session = getObject(s.recognizer2, slotContinuous); s.session == 0 {
		return ErrUnsupported
	}
	hyp := newDelegate(iidHypothesisHandler, func(_, args uintptr) { s.hypothesis(args) })
	comCall(s.recognizer2, slotAddHypothesis, hyp, uintptr(unsafe.Pointer(&s.hypothesisToken)))
	release(hyp)
	res := newDelegate(iidResultHandler, func(_, args uintptr) { s.result(args) })
	comCall(s.session, slotAddResult, res, uintptr(unsafe.Pointer(&s.resultToken)))
	release(res)
	done := newDelegate(iidCompletedHandler, func(_, args uintptr) { s.completed(getInt32(args, slotArgsValue)) })
	comCall(s.session, slotAddCompleted, done, uintptr(unsafe.Pointer(&s.doneTok)))
	release(done)

	// StartAsync fails right away when Settings refuse.
	if op, err = callAsync(s.session, slotStartAsync); err != nil {
		return err
	}
	_, err = await(op, iidAsyncActionCompleted, false)
	release(op)
	return err
}

func (s *winSession) join(text string) string {
	switch {
	case s.committed == "":
		return text
	case text == "":
		return s.committed
	case s.spaced:
		return s.committed + " " + text
	}
	return s.committed + text
}

func (s *winSession) hypothesis(args uintptr) {
	h := getObject(args, slotArgsValue)
	if h == 0 {
		return
	}
	text := getString(h, slotText)
	release(h)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.finished {
		s.emit(s.join(text), false)
	}
}

func (s *winSession) result(args uintptr) {
	r := getObject(args, slotArgsValue)
	if r == 0 {
		return
	}
	status := getInt32(r, slotStatus)
	text := getString(r, slotResText)
	release(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished || status != statusSuccess || text == "" {
		return
	}
	s.committed = s.join(text)
	s.emit(s.committed, false)
}

func (s *winSession) completed(status int32) {
	s.mu.Lock()
	canceled, text := s.canceled, s.committed
	s.mu.Unlock()
	switch {
	case canceled:
		s.finish(context.Canceled)
	case status == statusSuccess || status == statusUserCanceled ||
		status == statusTimeoutExceeded || status == statusPauseLimitExceeded:
		// Stop, or a long silence.
		s.mu.Lock()
		if !s.finished {
			s.emit(text, true)
		}
		s.mu.Unlock()
		s.finish(nil)
	default:
		s.finish(statusError(status))
	}
}

func statusError(status int32) error {
	switch status {
	case statusAudioQuality:
		return errors.New("speech: the audio was too poor to recognize")
	case statusNetworkFailure:
		return errors.New("speech: the network failed")
	case statusMicrophoneUnavailable:
		return fmt.Errorf("%w: there is no microphone, or desktop apps may not use it (Settings > Privacy > Microphone)", ErrDenied)
	}
	return fmt.Errorf("speech: speech recognition failed (status %d)", status)
}

func (s *winSession) stop() {
	onMTA(func() {
		s.mu.Lock()
		finished := s.finished
		s.mu.Unlock()
		if !finished && s.session != 0 {
			release(getObject(s.session, slotStopAsync))
		}
	})
}

func (s *winSession) cancel() {
	s.mu.Lock()
	s.canceled = true
	s.mu.Unlock()
	onMTA(func() {
		if s.session != 0 {
			release(getObject(s.session, slotCancelAsync))
		}
	})
	s.finish(context.Canceled)
}

// finish ends the recognition once: the handlers are removed and the
// objects released, off the thread that runs the event.
func (s *winSession) finish(err error) {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.mu.Unlock()
	go func() {
		onMTA(s.release)
		s.end(err)
	}()
}

// release removes the handlers and releases the objects, on the thread of
// the package's.
func (s *winSession) release() {
	if s.session != 0 {
		comCall(s.session, slotRemoveResult, uintptr(s.resultToken))
		comCall(s.session, slotRemoveCompleted, uintptr(s.doneTok))
		release(s.session)
		s.session = 0
	}
	if s.recognizer2 != 0 {
		comCall(s.recognizer2, slotRemoveHypothesis, uintptr(s.hypothesisToken))
		release(s.recognizer2)
		s.recognizer2 = 0
	}
	if s.recognizer != 0 {
		// Closing gives the microphone back now.
		if c := queryInterface(s.recognizer, &iidIClosable); c != 0 {
			comCall(c, slotClose)
			release(c)
		}
		release(s.recognizer)
		s.recognizer = 0
	}
}
