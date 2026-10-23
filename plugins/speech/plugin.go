package speech

import (
	"context"
	"errors"
	"sync"

	"github.com/egoist/mygo"
)

// Plugin gives the app's pages speech recognition, through
// @mygo-plugins/speech. Go code and native UI call Start directly.
var Plugin = mygo.Plugin{Name: "speech", Service: newService()}

type service struct {
	mu     sync.Mutex
	active map[key]*Recognition
}

// key names a page's recognition, by the id the page gave it.
type key struct {
	page any
	id   string
}

func newService() *service { return &service{active: make(map[key]*Recognition)} }

// request is a recognition a page asks for.
type request struct {
	ID       string `json:"id"`
	Locale   string `json:"locale"`
	OnDevice bool   `json:"onDevice"`
}

// part is what Go streams: "ready" once the microphone listens, then a
// "result" for each transcript.
type part struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitzero"`
	Final bool   `json:"final,omitzero"`
}

// outcome is how a call ended: the code of what failed, if anything.
type outcome struct {
	Code    string `json:"code,omitzero"`
	Message string `json:"message,omitzero"`
}

func failure(err error) outcome {
	if err == nil || errors.Is(err, context.Canceled) {
		return outcome{}
	}
	code := "failed"
	switch {
	case errors.Is(err, ErrUnsupported):
		code = "unsupported"
	case errors.Is(err, ErrDenied):
		code = "denied"
	case errors.Is(err, ErrLanguage):
		code = "language"
	case errors.Is(err, ErrBusy):
		code = "busy"
	}
	return outcome{Code: code, Message: err.Error()}
}

func (s *service) Supported() bool { return Supported() }

func (s *service) RequestPermission(ctx context.Context) outcome {
	return failure(RequestPermission(ctx))
}

// Listen recognizes until the recognition ends, the page closes parts or
// it navigates.
func (s *service) Listen(ctx context.Context, req request, parts *mygo.Channel[part]) outcome {
	page := mygo.CallerPage(ctx)
	if page == nil {
		return failure(ErrDenied)
	}
	return s.listen(ctx, page, req, parts.Send)
}

func (s *service) listen(ctx context.Context, page any, req request, send func(part) error) outcome {
	if req.ID == "" {
		return outcome{Code: "failed", Message: "speech: a recognition needs an id"}
	}
	k := key{page, req.ID}
	s.mu.Lock()
	_, taken := s.active[k]
	s.mu.Unlock()
	if taken {
		return failure(ErrBusy)
	}
	r, err := Start(ctx, Options{Locale: req.Locale, OnDevice: req.OnDevice})
	if err != nil {
		return failure(err)
	}
	s.mu.Lock()
	s.active[k] = r
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, k)
		s.mu.Unlock()
	}()
	if send(part{Type: "ready"}) != nil {
		r.Cancel()
	}
	for res := range r.Results() {
		if send(part{Type: "result", Text: res.Text, Final: res.Final}) != nil {
			r.Cancel()
		}
	}
	return failure(r.Err())
}

// Stop stops the page's recognition id: its final result follows.
func (s *service) Stop(ctx context.Context, id string) {
	page := mygo.CallerPage(ctx)
	if page == nil {
		return
	}
	s.stop(page, id)
}

func (s *service) stop(page any, id string) {
	s.mu.Lock()
	r := s.active[key{page, id}]
	s.mu.Unlock()
	if r != nil {
		r.Stop()
	}
}
