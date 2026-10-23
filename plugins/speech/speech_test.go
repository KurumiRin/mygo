package speech

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/egoist/mygo"
)

// fakeSession is a platform's recognition that the test drives: say emits
// a transcript, as the user speaks.
type fakeSession struct {
	emit func(string, bool)
	end  func(error)

	mu             sync.Mutex
	text           string
	stops, cancels int
}

func (f *fakeSession) say(text string) {
	f.mu.Lock()
	f.text = text
	f.mu.Unlock()
	f.emit(text, false)
}

func (f *fakeSession) stop() {
	f.mu.Lock()
	f.stops++
	text := f.text
	f.mu.Unlock()
	// The platform finishes on its own thread.
	go func() {
		f.emit(text, true)
		f.end(nil)
	}()
}

func (f *fakeSession) cancel() {
	f.mu.Lock()
	f.cancels++
	f.mu.Unlock()
	go f.end(context.Canceled)
}

// fakePlatform replaces the platform's functions for the test; sessions
// receives each recognition started.
func fakePlatform(t *testing.T, authErr, startErr error) <-chan *fakeSession {
	t.Helper()
	sessions := make(chan *fakeSession, 4)
	oldSupported, oldAuthorize, oldStart := supported, authorize, start
	t.Cleanup(func() { supported, authorize, start = oldSupported, oldAuthorize, oldStart })
	supported = func() bool { return true }
	authorize = func(context.Context) error { return authErr }
	start = func(ctx context.Context, opts Options, emit func(string, bool), end func(error)) (session, error) {
		if startErr != nil {
			return nil, startErr
		}
		f := &fakeSession{emit: emit, end: end}
		sessions <- f
		return f, nil
	}
	return sessions
}

func TestStopSendsTheFinalResult(t *testing.T) {
	sessions := fakePlatform(t, nil, nil)
	r, err := Start(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := <-sessions
	f.say("hello")
	if res := <-r.Results(); res != (Result{Text: "hello"}) {
		t.Fatalf("result %+v", res)
	}
	f.say("hello world")
	if res := <-r.Results(); res != (Result{Text: "hello world"}) {
		t.Fatalf("result %+v", res)
	}
	r.Stop()
	r.Stop()
	var last Result
	for res := range r.Results() {
		last = res
	}
	if last != (Result{Text: "hello world", Final: true}) {
		t.Fatalf("last result %+v", last)
	}
	<-r.Done()
	if r.Err() != nil || f.stops != 1 {
		t.Fatalf("err %v, stops %d", r.Err(), f.stops)
	}
	// The recognition is over: another may start.
	r2, err := Start(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	r2.Cancel()
	<-r2.Done()
}

func TestCancelHasNoFinalResult(t *testing.T) {
	sessions := fakePlatform(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	r, err := Start(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := <-sessions
	cancel()
	for res := range r.Results() {
		if res.Final {
			t.Fatalf("final result %+v after cancel", res)
		}
	}
	if !errors.Is(r.Err(), context.Canceled) || f.cancels != 1 {
		t.Fatalf("err %v, cancels %d", r.Err(), f.cancels)
	}
	r.Stop() // nothing to stop
	if f.stops != 0 {
		t.Fatal("stopped after the end")
	}
}

func TestSlowReceiverGetsTheLatest(t *testing.T) {
	sessions := fakePlatform(t, nil, nil)
	r, err := Start(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := <-sessions
	f.say("a")
	f.say("a b")
	f.say("a b c")
	// The platform never waits for the receiver; the results it took
	// too long for give way to the latest.
	f.emit("a b c d", true)
	f.end(nil)
	var got []Result
	for res := range r.Results() {
		got = append(got, res)
	}
	if len(got) == 0 || len(got) > 4 || got[len(got)-1] != (Result{Text: "a b c d", Final: true}) {
		t.Fatalf("results %+v", got)
	}
}

func TestOneAtATime(t *testing.T) {
	fakePlatform(t, nil, nil)
	r, err := Start(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), Options{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Start: %v", err)
	}
	r.Cancel()
	<-r.Done()
}

func TestFailures(t *testing.T) {
	fakePlatform(t, ErrDenied, nil)
	if _, err := Start(context.Background(), Options{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("Start: %v", err)
	}
	if err := RequestPermission(context.Background()); !errors.Is(err, ErrDenied) {
		t.Fatalf("RequestPermission: %v", err)
	}
	fakePlatform(t, nil, ErrLanguage)
	if _, err := Start(context.Background(), Options{Locale: "xx"}); !errors.Is(err, ErrLanguage) {
		t.Fatalf("Start: %v", err)
	}
	// A failed start leaves the way open.
	sessions := fakePlatform(t, nil, nil)
	r, err := Start(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := <-sessions
	f.say("partial")
	f.end(errors.New("speech: the network failed"))
	for range r.Results() {
	}
	if r.Err() == nil || r.Err().Error() != "speech: the network failed" {
		t.Fatalf("err %v", r.Err())
	}
	supported = func() bool { return false }
	if Supported() {
		t.Fatal("supported")
	}
	if _, err := Start(context.Background(), Options{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Start: %v", err)
	}
}

// TestPlatform runs on every platform: where there is no recognizer,
// everything says so.
func TestPlatform(t *testing.T) {
	if Supported() {
		t.Skip("the system has a recognizer, which would ask the user")
	}
	if _, err := Start(context.Background(), Options{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Start: %v", err)
	}
	if err := RequestPermission(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("RequestPermission: %v", err)
	}
}

func TestService(t *testing.T) {
	sessions := fakePlatform(t, nil, nil)
	s := newService()
	page := new(int)
	parts := make(chan part, 8)
	out := make(chan outcome, 1)
	go func() {
		out <- s.listen(context.Background(), page, request{ID: "1", Locale: "en-US"}, func(p part) error {
			parts <- p
			return nil
		})
	}()
	f := <-sessions
	if p := <-parts; p.Type != "ready" {
		t.Fatalf("first part %+v", p)
	}
	f.say("hi")
	if p := <-parts; p != (part{Type: "result", Text: "hi"}) {
		t.Fatalf("part %+v", p)
	}
	// Another page's id, or another id, stops nothing.
	s.stop(new(int), "1")
	s.stop(page, "2")
	if o := s.listen(context.Background(), page, request{ID: "1"}, func(part) error { return nil }); o.Code != "busy" {
		t.Fatalf("same id: %+v", o)
	}
	s.stop(page, "1")
	if p := <-parts; p != (part{Type: "result", Text: "hi", Final: true}) {
		t.Fatalf("part %+v", p)
	}
	if o := <-out; o != (outcome{}) {
		t.Fatalf("outcome %+v", o)
	}
	if f.stops != 1 {
		t.Fatalf("stops %d", f.stops)
	}
	if o := s.listen(context.Background(), page, request{}, nil); o.Code != "failed" {
		t.Fatalf("no id: %+v", o)
	}
}

func TestServiceCancelsWhenThePageCloses(t *testing.T) {
	sessions := fakePlatform(t, nil, nil)
	s := newService()
	out := make(chan outcome, 1)
	go func() {
		out <- s.listen(context.Background(), new(int), request{ID: "1"}, func(part) error { return mygo.ErrChannelClosed })
	}()
	f := <-sessions
	if o := <-out; o != (outcome{}) {
		t.Fatalf("outcome %+v", o)
	}
	if f.cancels != 1 {
		t.Fatalf("cancels %d", f.cancels)
	}
}

func TestFailureCodes(t *testing.T) {
	for err, code := range map[error]string{
		nil:                          "",
		context.Canceled:             "",
		ErrUnsupported:               "unsupported",
		ErrDenied:                    "denied",
		ErrLanguage:                  "language",
		ErrBusy:                      "busy",
		errors.New("speech: failed"): "failed",
	} {
		if o := failure(err); o.Code != code {
			t.Errorf("%v: code %q, want %q", err, o.Code, code)
		}
	}
}

// TestUse binds the service as mygo.Use does, which panics on methods
// pages can't call.
func TestUse(t *testing.T) {
	mygo.Use(Plugin)
}
