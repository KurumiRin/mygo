// Package speech is the speech plugin of MyGo: speech to text from the
// microphone, with the recognizer of the system, for Go code and native UI
// and, through the plugin, for pages (see @mygo-plugins/speech):
//
//	r, err := speech.Start(ctx, speech.Options{Locale: "en-US"})
//	if err != nil {
//		return err
//	}
//	for res := range r.Results() {
//		field.SetText(res.Text) // the transcript so far
//	}
//	return r.Err()
//
// macOS 26 and later recognize with SpeechAnalyzer, older macOS with
// SFSpeechRecognizer, the microphone through AVAudioEngine, Windows with the continuous dictation of
// Windows.Media.SpeechRecognition. Linux and browsers have no recognizer
// of the system: Start fails there with ErrUnsupported. Every function and
// method is safe to call from any goroutine, the main one included: none
// waits for the main thread.
package speech

import (
	"context"
	"errors"
	"sync"
)

// Options configures a recognition.
type Options struct {
	// Locale is the language spoken, as a BCP 47 tag such as "en-US" or
	// "zh-CN": the user's language when empty. Start fails with
	// ErrLanguage when the system can't recognize it.
	Locale string
	// OnDevice requires recognition on the device, which sends no audio
	// to a server. macOS has it for some languages (supportsOnDevice-
	// Recognition); elsewhere Start fails with ErrUnsupported.
	OnDevice bool
}

// Result is what the recognizer heard so far.
type Result struct {
	// Text is the transcript of everything said since Start, which the
	// recognizer may still revise until the final result.
	Text string `json:"text"`
	// Final marks the last result, sent once the recognition ended
	// without being canceled.
	Final bool `json:"final"`
}

var (
	// ErrUnsupported is returned where the system has no recognizer, or
	// not the one Options ask for.
	ErrUnsupported = errors.New("speech: speech recognition is not supported")
	// ErrDenied is returned when the user or the system doesn't let the
	// app use speech recognition or the microphone.
	ErrDenied = errors.New("speech: speech recognition or the microphone is not allowed")
	// ErrLanguage is returned when the system can't recognize the locale
	// of Options.
	ErrLanguage = errors.New("speech: the language is not supported")
	// ErrBusy is returned by Start while another recognition runs: an
	// app recognizes one at a time.
	ErrBusy = errors.New("speech: another recognition is running")
)

// Supported reports whether the system has a speech recognizer: whether
// Start may succeed, for instance to show a dictation button.
func Supported() bool { return supported() }

// RequestPermission asks the user, the first time, to let the app
// recognize speech and use the microphone, and returns ErrDenied when they
// or the system don't. Start asks too; asking first lets an app explain
// before the system's prompt. It returns once the user answered or ctx is
// done.
//
// On macOS the app's Info.plist needs NSSpeechRecognitionUsageDescription
// and NSMicrophoneUsageDescription (macos.infoPlist of mygo.config.ts),
// without which macOS would end the app: it returns an error saying so
// instead. Windows has no prompt for desktop apps: the user allows the
// microphone and online speech recognition in Settings > Privacy, and
// RequestPermission returns nil, since Windows tells what Settings refuse
// only when a recognition starts, where Start returns ErrDenied.
func RequestPermission(ctx context.Context) error {
	if !supported() {
		return ErrUnsupported
	}
	return authorize(ctx)
}

// A Recognition listens to the microphone from Start until it ends: after
// Stop, Cancel, a long silence or the system's limit (about a minute for
// macOS's server recognition), or when it fails.
type Recognition struct {
	results chan Result
	wake    chan struct{}
	cancel  context.CancelFunc

	mu      sync.Mutex
	sess    session
	pending *Result // the latest result not yet sent
	ended   bool
	stopped bool // Stop or Cancel was called
	err     error
	done    chan struct{}
}

// Start asks for permission as RequestPermission does, then listens to the
// microphone and recognizes what is said. Canceling ctx cancels the
// recognition.
func Start(ctx context.Context, opts Options) (*Recognition, error) {
	if !supported() {
		return nil, ErrUnsupported
	}
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	if !running.TryLock() {
		return nil, ErrBusy
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &Recognition{
		results: make(chan Result),
		wake:    make(chan struct{}, 1),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	sess, err := start(ctx, opts, r.emit, r.end)
	if err != nil {
		cancel()
		running.Unlock()
		return nil, err
	}
	r.mu.Lock()
	r.sess = sess
	r.mu.Unlock()
	go r.forward()
	go func() {
		select {
		case <-ctx.Done():
			r.Cancel()
		case <-r.done:
		}
	}()
	return r, nil
}

// running is held while a recognition runs.
var running sync.Mutex

// Results returns the results, the transcript growing as the user speaks,
// then the final one; it closes once the recognition ended. A result the
// receiver was too slow to take gives way to the next, which has its text.
func (r *Recognition) Results() <-chan Result { return r.results }

// Stop stops listening: the recognizer finishes what it heard, sends the
// final result and ends.
func (r *Recognition) Stop() {
	if s := r.stopping(); s != nil {
		s.stop()
	}
}

// Cancel stops listening and recognizing at once, without a final result.
func (r *Recognition) Cancel() {
	if s := r.stopping(); s != nil {
		s.cancel()
	}
}

func (r *Recognition) stopping() session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended || r.stopped || r.sess == nil {
		return nil
	}
	r.stopped = true
	return r.sess
}

// Done is closed once the recognition ended.
func (r *Recognition) Done() <-chan struct{} { return r.done }

// Err returns why the recognition ended, once it did: nil after Stop and
// silence, context.Canceled after Cancel, or what failed.
func (r *Recognition) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// emit takes a result from the platform, on any thread: it never waits for
// the receiver.
func (r *Recognition) emit(text string, final bool) {
	r.mu.Lock()
	if r.ended || r.pending != nil && r.pending.Final {
		r.mu.Unlock()
		return
	}
	r.pending = &Result{Text: text, Final: final}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// end takes the end of the recognition from the platform, once, on any
// thread, after its last result.
func (r *Recognition) end(err error) {
	r.mu.Lock()
	if r.ended {
		r.mu.Unlock()
		return
	}
	r.ended = true
	if errors.Is(err, context.Canceled) && r.pending != nil && !r.pending.Final {
		r.pending = nil // a canceled recognition has no last word
	}
	r.err = err
	r.mu.Unlock()
	r.cancel()
	running.Unlock()
	close(r.done)
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// forward sends the results to the receiver, the latest each time: one
// it hasn't taken gives way to the next. It closes the channel once the
// recognition ended and the receiver took the last.
func (r *Recognition) forward() {
	var res *Result
	for {
		r.mu.Lock()
		if r.pending != nil {
			res, r.pending = r.pending, nil
		}
		ended := r.ended
		r.mu.Unlock()
		if res == nil {
			if ended {
				close(r.results)
				return
			}
			<-r.wake
			continue
		}
		select {
		case r.results <- *res:
			res = nil
		case <-r.wake:
		}
	}
}

// A session is a recognition as a platform runs it.
type session interface {
	// stop stops listening; the platform sends the final result, then
	// ends.
	stop()
	// cancel stops at once; the platform ends with context.Canceled.
	cancel()
}

// The platform's functions; tests replace them.
var (
	supported = platformSupported
	authorize = platformAuthorize
	// start starts listening and recognizing, then calls emit with each
	// transcript, from any thread, and end once, with nil after stop or
	// silence, context.Canceled after cancel, or what failed.
	start = platformStart
)
