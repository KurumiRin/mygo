package speech

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
	"github.com/egoist/mygo/plugins/speech/internal/analyzer"
)

// macOS 26 and later recognize with SpeechAnalyzer, through the shim of
// package analyzer, which is on the device and more accurate; older
// systems, and those without the shim or the language's model, with
// SFSpeechRecognizer: an audio buffer request fed
// by a tap on AVAudioEngine's input node, the microphone, with partial
// results. Its results arrive on an operation queue of the recognition's
// own, not on the main thread, so that nothing waits for the main run
// loop. Asking for the speech recognition and microphone permissions
// without their usage descriptions in Info.plist would end the app, so
// both are checked first.

var (
	sfOnce sync.Once
	sfOK   bool

	poolPush, poolPop uintptr
	mediaTypeAudio    objc.ID // AVMediaTypeAudio

	selAlloc, selInit, selRelease, selRetain, selAutorelease, selRespondsTo, selMainBundle,
	selInfoObject, selStringWithBytes, selUTF8, selLength, selLocaleWithID,
	selInitWithLocale, selAuthStatus, selRequestAuth, selAVAuthStatus,
	selAVRequestAccess, selIsAvailable, selSupportsOnDevice, selSetQueue,
	selSetMaxOps, selSetTaskHint, selSetPartial, selSetOnDevice, selSetPunctuation, selInputNode,
	selOutputFormat, selSampleRate, selInstallTap, selRemoveTap, selAppendBuffer,
	selPrepare, selStartEngine, selStopEngine, selEndAudio, selTaskWithRequest,
	selCancel, selBestTranscription, selFormattedString, selIsFinal, selDomain,
	selCode, selLocalizedDescription objc.SEL
)

const (
	authNotDetermined = 0
	authAuthorized    = 3
	utf8Encoding      = 4 // NSUTF8StringEncoding
)

func loadSpeech() bool {
	sfOnce.Do(func() {
		if _, err := purego.Dlopen("/System/Library/Frameworks/Speech.framework/Speech", purego.RTLD_LAZY|purego.RTLD_GLOBAL); err != nil {
			return
		}
		av, err := purego.Dlopen("/System/Library/Frameworks/AVFoundation.framework/AVFoundation", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			return
		}
		libobjc, err := purego.Dlopen("/usr/lib/libobjc.A.dylib", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			return
		}
		if poolPush, err = purego.Dlsym(libobjc, "objc_autoreleasePoolPush"); err != nil {
			return
		}
		if poolPop, err = purego.Dlsym(libobjc, "objc_autoreleasePoolPop"); err != nil {
			return
		}
		p, err := purego.Dlsym(av, "AVMediaTypeAudio")
		if err != nil || p == 0 {
			return
		}
		mediaTypeAudio = **(**objc.ID)(unsafe.Pointer(&p))
		sel := objc.RegisterName
		selAlloc, selInit, selRelease, selRetain, selAutorelease = sel("alloc"), sel("init"), sel("release"), sel("retain"), sel("autorelease")
		selRespondsTo, selMainBundle, selInfoObject = sel("respondsToSelector:"), sel("mainBundle"), sel("objectForInfoDictionaryKey:")
		selStringWithBytes, selUTF8, selLength = sel("initWithBytes:length:encoding:"), sel("UTF8String"), sel("lengthOfBytesUsingEncoding:")
		selLocaleWithID, selInitWithLocale = sel("localeWithLocaleIdentifier:"), sel("initWithLocale:")
		selAuthStatus, selRequestAuth = sel("authorizationStatus"), sel("requestAuthorization:")
		selAVAuthStatus, selAVRequestAccess = sel("authorizationStatusForMediaType:"), sel("requestAccessForMediaType:completionHandler:")
		selIsAvailable, selSupportsOnDevice, selSetQueue = sel("isAvailable"), sel("supportsOnDeviceRecognition"), sel("setQueue:")
		selSetMaxOps, selSetTaskHint = sel("setMaxConcurrentOperationCount:"), sel("setTaskHint:")
		selSetPartial, selSetOnDevice, selSetPunctuation = sel("setShouldReportPartialResults:"), sel("setRequiresOnDeviceRecognition:"), sel("setAddsPunctuation:")
		selInputNode, selOutputFormat, selSampleRate = sel("inputNode"), sel("outputFormatForBus:"), sel("sampleRate")
		selInstallTap, selRemoveTap, selAppendBuffer = sel("installTapOnBus:bufferSize:format:block:"), sel("removeTapOnBus:"), sel("appendAudioPCMBuffer:")
		selPrepare, selStartEngine, selStopEngine, selEndAudio = sel("prepare"), sel("startAndReturnError:"), sel("stop"), sel("endAudio")
		selTaskWithRequest, selCancel = sel("recognitionTaskWithRequest:resultHandler:"), sel("cancel")
		selBestTranscription, selFormattedString, selIsFinal = sel("bestTranscription"), sel("formattedString"), sel("isFinal")
		selDomain, selCode, selLocalizedDescription = sel("domain"), sel("code"), sel("localizedDescription")
		sfOK = objc.GetClass("SFSpeechRecognizer") != 0 && objc.GetClass("AVAudioEngine") != 0
	})
	return sfOK
}

// withPool runs fn in an autorelease pool, on one thread until it is
// popped.
func withPool(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	p, _, _ := purego.SyscallN(poolPush)
	defer purego.SyscallN(poolPop, p)
	fn()
}

func class(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

func newObject(name string) objc.ID { return class(name).Send(selAlloc).Send(selInit) }

// nsString returns an autoreleased NSString of s.
func nsString(s string) objc.ID {
	b := []byte(s)
	var p unsafe.Pointer
	if len(b) > 0 {
		p = unsafe.Pointer(&b[0])
	}
	str := class("NSString").Send(selAlloc).Send(selStringWithBytes, p, uint(len(b)), uint(utf8Encoding))
	runtime.KeepAlive(b)
	return str.Send(selAutorelease)
}

func goString(str objc.ID) string {
	if str == 0 {
		return ""
	}
	n := objc.Send[uint](str, selLength, uint(utf8Encoding))
	p := objc.Send[unsafe.Pointer](str, selUTF8)
	if p == nil || n == 0 {
		return ""
	}
	return string(unsafe.Slice((*byte)(p), n))
}

func platformSupported() bool { return loadSpeech() }

// usageDescriptions are the keys of Info.plist without which macOS ends an
// app that asks for the permissions.
var usageDescriptions = []string{"NSSpeechRecognitionUsageDescription", "NSMicrophoneUsageDescription"}

func platformAuthorize(ctx context.Context) error {
	var missing []string
	withPool(func() {
		bundle := class("NSBundle").Send(selMainBundle)
		for _, key := range usageDescriptions {
			if bundle == 0 || bundle.Send(selInfoObject, nsString(key)) == 0 {
				missing = append(missing, key)
			}
		}
	})
	if len(missing) > 0 {
		return fmt.Errorf("speech: the app's Info.plist has no %s (macos.infoPlist of mygo.config.ts), without which macOS ends the app when it asks for speech recognition", strings.Join(missing, " and "))
	}
	if err := authorizeSpeech(ctx); err != nil {
		return err
	}
	return authorizeMicrophone(ctx)
}

func authorizeSpeech(ctx context.Context) error {
	recognizer := class("SFSpeechRecognizer")
	status := objc.Send[int](recognizer, selAuthStatus)
	if status == authNotDetermined {
		answer := make(chan int, 1)
		withPool(func() {
			blk := objc.NewBlock(func(_ objc.Block, status int) { answer <- status })
			recognizer.Send(selRequestAuth, blk)
			blk.Release()
		})
		select {
		case status = <-answer:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if status != authAuthorized {
		return ErrDenied
	}
	return nil
}

func authorizeMicrophone(ctx context.Context) error {
	device := class("AVCaptureDevice")
	status := objc.Send[int](device, selAVAuthStatus, mediaTypeAudio)
	if status == authNotDetermined {
		answer := make(chan bool, 1)
		withPool(func() {
			blk := objc.NewBlock(func(_ objc.Block, granted bool) { answer <- granted })
			device.Send(selAVRequestAccess, mediaTypeAudio, blk)
			blk.Release()
		})
		select {
		case granted := <-answer:
			if granted {
				status = authAuthorized
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if status != authAuthorized {
		return ErrDenied
	}
	return nil
}

// sfSession is a recognition: the objects are owned, and released once it
// ended.
type sfSession struct {
	emit func(string, bool)
	end  func(error)

	// ops serializes Stop with the release of the objects.
	ops                                      sync.Mutex
	recognizer, queue, request, engine, task objc.ID
	listening, released                      bool

	mu                 sync.Mutex
	canceled, finished bool
	last               string
}

func platformStart(ctx context.Context, opts Options, emit func(string, bool), end func(error)) (session, error) {
	if analyzer.Available() {
		if s, ok, err := startAnalyzer(ctx, opts, emit, end); ok {
			return s, err
		}
	}
	s := &sfSession{emit: emit, end: end}
	var err error
	withPool(func() { err = s.start(opts) })
	if err != nil {
		s.release()
		return nil, err
	}
	return s, nil
}

func (s *sfSession) start(opts Options) error {
	if opts.Locale != "" {
		// NSLocale's identifiers separate with underscores.
		locale := class("NSLocale").Send(selLocaleWithID, nsString(strings.ReplaceAll(opts.Locale, "-", "_")))
		s.recognizer = class("SFSpeechRecognizer").Send(selAlloc).Send(selInitWithLocale, locale)
	} else {
		s.recognizer = newObject("SFSpeechRecognizer")
	}
	if s.recognizer == 0 {
		return ErrLanguage
	}
	if !objc.Send[bool](s.recognizer, selIsAvailable) {
		return errors.New("speech: the recognizer is not available now")
	}
	if opts.OnDevice && (!objc.Send[bool](s.recognizer, selRespondsTo, selSupportsOnDevice) || !objc.Send[bool](s.recognizer, selSupportsOnDevice)) {
		return fmt.Errorf("%w: on-device recognition of the language", ErrUnsupported)
	}
	s.queue = newObject("NSOperationQueue")
	s.queue.Send(selSetMaxOps, 1) // the results in order
	s.recognizer.Send(selSetQueue, s.queue)

	s.request = newObject("SFSpeechAudioBufferRecognitionRequest")
	s.request.Send(selSetPartial, true)
	s.request.Send(selSetTaskHint, 1) // SFSpeechRecognitionTaskHintDictation
	if opts.OnDevice {
		s.request.Send(selSetOnDevice, true)
	}
	if objc.Send[bool](s.request, selRespondsTo, selSetPunctuation) { // macOS 13
		s.request.Send(selSetPunctuation, true)
	}

	s.engine = newObject("AVAudioEngine")
	input := s.engine.Send(selInputNode)
	format := input.Send(selOutputFormat, uint(0))
	if input == 0 || format == 0 || objc.Send[float64](format, selSampleRate) == 0 {
		return errors.New("speech: there is no microphone")
	}
	request := s.request
	tap := objc.NewBlock(func(_ objc.Block, buffer, when objc.ID) {
		request.Send(selAppendBuffer, buffer)
	})
	input.Send(selInstallTap, uint(0), uint32(1024), format, tap)
	tap.Release()
	s.listening = true
	s.engine.Send(selPrepare)
	var nsErr objc.ID
	if !objc.Send[bool](s.engine, selStartEngine, unsafe.Pointer(&nsErr)) {
		s.stopAudio()
		return fmt.Errorf("speech: the microphone did not start: %s", goString(nsErr.Send(selLocalizedDescription)))
	}

	handler := objc.NewBlock(func(_ objc.Block, result, nsErr objc.ID) { s.handle(result, nsErr) })
	s.ops.Lock()
	task := s.recognizer.Send(selTaskWithRequest, s.request, handler)
	if task != 0 {
		s.task = task.Send(selRetain)
	}
	s.ops.Unlock()
	handler.Release()
	if task == 0 {
		s.stopAudio()
		return errors.New("speech: the recognition did not start")
	}
	return nil
}

// handle takes a result or an error of the task, on the recognition's
// queue.
func (s *sfSession) handle(result, nsErr objc.ID) {
	if result != 0 {
		text := goString(result.Send(selBestTranscription).Send(selFormattedString))
		final := objc.Send[bool](result, selIsFinal)
		s.mu.Lock()
		done := s.finished || s.canceled
		s.last = text
		s.mu.Unlock()
		if done {
			return
		}
		s.emit(text, final)
		if final {
			s.finish(nil)
			return
		}
	}
	if nsErr == 0 {
		return
	}
	s.mu.Lock()
	canceled, last := s.canceled, s.last
	s.mu.Unlock()
	switch {
	case canceled:
		s.finish(context.Canceled)
	case objc.Send[int](nsErr, selCode) == 1110: // no speech detected
		s.emit(last, true)
		s.finish(nil)
	default:
		s.finish(fmt.Errorf("speech: %s", goString(nsErr.Send(selLocalizedDescription))))
	}
}

// stopAudio stops the microphone, once.
func (s *sfSession) stopAudio() {
	if !s.listening {
		return
	}
	s.listening = false
	s.engine.Send(selStopEngine)
	s.engine.Send(selInputNode).Send(selRemoveTap, uint(0))
}

func (s *sfSession) stop() {
	s.ops.Lock()
	defer s.ops.Unlock()
	if s.released {
		return
	}
	withPool(func() {
		s.stopAudio()
		s.request.Send(selEndAudio)
	})
}

func (s *sfSession) cancel() {
	s.mu.Lock()
	s.canceled = true
	s.mu.Unlock()
	// The task may not call its handler once canceled.
	s.finish(context.Canceled)
}

// finish ends the recognition once: the microphone stops, the task is
// canceled if it still runs, and the objects are released.
func (s *sfSession) finish(err error) {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.mu.Unlock()
	// Not on the queue that called handle: releasing the task there would
	// free what is running.
	go func() {
		s.ops.Lock()
		withPool(func() {
			s.stopAudio()
			if s.task != 0 {
				s.task.Send(selCancel)
			}
		})
		s.release()
		s.ops.Unlock()
		s.end(err)
	}()
}

// release releases the objects, with ops held or before the recognition
// started.
func (s *sfSession) release() {
	s.released = true
	withPool(func() {
		for _, obj := range []*objc.ID{&s.task, &s.request, &s.engine, &s.recognizer, &s.queue} {
			if *obj != 0 {
				obj.Send(selRelease)
				*obj = 0
			}
		}
	})
}

// analyzerSession is a recognition of SpeechAnalyzer's.
type analyzerSession struct{ id uint64 }

func (a analyzerSession) stop()   { analyzer.Stop(a.id) }
func (a analyzerSession) cancel() { analyzer.Cancel(a.id) }

// startAnalyzer starts recognizing with SpeechAnalyzer and waits until it
// listens. It reports false when SFSpeechRecognizer should recognize
// instead: SpeechAnalyzer has no model of the language, or failed before
// it listened.
func startAnalyzer(ctx context.Context, opts Options, emit func(string, bool), end func(error)) (session, bool, error) {
	type outcome struct {
		event int32
		text  string
	}
	var (
		mu      sync.Mutex
		running bool
	)
	started := make(chan outcome, 2)
	id := analyzer.Start(opts.Locale, func(event int32, text string) {
		mu.Lock()
		if !running {
			running = event == analyzer.EventReady
			mu.Unlock()
			started <- outcome{event, text}
			return
		}
		mu.Unlock()
		switch event {
		case analyzer.EventText:
			emit(text, false)
		case analyzer.EventFinal:
			emit(text, true)
			end(nil)
		case analyzer.EventCanceled:
			end(context.Canceled)
		default:
			end(errors.New(text))
		}
	})
	select {
	case o := <-started:
		switch o.event {
		case analyzer.EventReady:
			return analyzerSession{id}, true, nil
		case analyzer.EventCanceled:
			return nil, true, context.Canceled
		}
		return nil, false, nil
	case <-ctx.Done():
		// The first use downloads the language's model, which may take
		// long.
		analyzer.Cancel(id)
		return nil, true, ctx.Err()
	}
}
