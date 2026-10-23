//go:build linux && (amd64 || arm64)

package screencapture

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/png" // the Screenshot portal's files
	"net/url"
	"os"
	"runtime"
	"strconv"
	"sync"
	"syscall"
)

// Linux captures through xdg-desktop-portal, which asks the user, and
// knows displays and windows only as the user picks them: screenshots
// through its Screenshot portal, recordings through its ScreenCast portal
// and PipeWire.

const (
	screenCastIface = "org.freedesktop.portal.ScreenCast"
	screenshotIface = "org.freedesktop.portal.Screenshot"

	sourceMonitor = 1
	sourceWindow  = 2

	cursorHidden   = 1
	cursorEmbedded = 2
)

func windows(context.Context) ([]Window, error) { return nil, errors.ErrUnsupported }

func access() bool        { return true }
func requestAccess() bool { return true }

func closeFd(fd int32) { syscall.Close(int(fd)) }

// onPortalThread runs fn on a thread of its own, with a portal, and
// returns its error.
func onPortalThread(fn func(p *portal) error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		p, err := newPortal()
		if err != nil {
			done <- err
			return
		}
		defer p.close()
		done <- fn(p)
	}()
	return <-done
}

// screenshot takes a screenshot with the Screenshot portal: of the whole
// desktop, or, for a window, of what the user picks in its dialog. The
// portal saves it as a file, which it reads and removes.
func screenshot(ctx context.Context, src Source, opts Options) (*image.RGBA, bool, error) {
	var path string
	err := onPortalThread(func(p *portal) error {
		res, err := p.request(ctx, screenshotIface, "Screenshot", []ptr{gVariantNewString(cs(""))},
			vardictEntry("interactive", gVariantNewBoolean(src.window)),
			vardictEntry("modal", gVariantNewBoolean(true)))
		if err != nil {
			return err
		}
		uri := lookupString(res, "uri")
		u, err := url.Parse(uri)
		if err != nil || u.Scheme != "file" {
			return fmt.Errorf("screencapture: the portal gave %q", uri)
		}
		path = u.Path
		return nil
	})
	if err != nil {
		return nil, true, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, true, err
	}
	img, _, err := image.Decode(f)
	f.Close()
	os.Remove(path)
	if err != nil {
		return nil, true, fmt.Errorf("screencapture: %w", err)
	}
	if rgba, ok := img.(*image.RGBA); ok && rgba.Rect.Min == (image.Point{}) {
		return rgba, true, nil
	}
	rgba := image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
	draw.Draw(rgba, rgba.Rect, img, img.Bounds().Min, draw.Src)
	return rgba, true, nil
}

// pwControl lets other goroutines stop a PipeWire main loop while it
// exists.
type pwControl struct {
	mu   sync.Mutex
	loop ptr
}

func (c *pwControl) set(loop ptr) {
	c.mu.Lock()
	c.loop = loop
	c.mu.Unlock()
}

func (c *pwControl) quit() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loop != 0 {
		pwMainLoopQuit(c.loop)
	}
}

func start(ctx context.Context, src Source, opts Options, r *Recording) (func(), error) {
	ready := make(chan error, 1)
	exited := make(chan struct{})
	ctl := &pwControl{}
	go func() {
		defer close(exited)
		started := false
		err := onPortalThread(func(p *portal) error {
			session, node, fd, err := p.screenCast(ctx, src, opts)
			if err != nil {
				return err
			}
			defer p.closeSession(session)
			return runPipeWire(fd, node, r, ctl, func() {
				started = true
				ready <- nil
			})
		})
		if !started {
			ready <- err
			return
		}
		r.end(err)
	}()
	if err := <-ready; err != nil {
		return nil, err
	}
	return func() {
		ctl.quit()
		<-exited
	}, nil
}

// screenCast asks the user what to share, with the ScreenCast portal, and
// returns the session, the PipeWire node to record and the remote to
// find it in.
func (p *portal) screenCast(ctx context.Context, src Source, opts Options) (session string, node uint32, fd int32, err error) {
	token := "mygo_screencapture" + strconv.FormatUint(tokens.Add(1), 10)
	res, err := p.request(ctx, screenCastIface, "CreateSession", nil,
		vardictEntry("session_handle_token", gVariantNewString(cs(token))))
	if err != nil {
		return "", 0, -1, err
	}
	session = lookupString(res, "session_handle")
	if session == "" {
		return "", 0, -1, errors.New("screencapture: the portal made no session")
	}
	defer func() {
		if err != nil {
			p.closeSession(session)
		}
	}()

	kind := uint32(sourceMonitor)
	if src.window {
		kind = sourceWindow
	}
	if avail, ok := p.property(screenCastIface, "AvailableSourceTypes"); ok && avail&kind == 0 {
		return "", 0, -1, fmt.Errorf("screencapture: the desktop cannot share a %s: %w", map[bool]string{false: "display", true: "window"}[src.window], errors.ErrUnsupported)
	}
	options := []ptr{
		vardictEntry("types", gVariantNewUint32(kind)),
		vardictEntry("multiple", gVariantNewBoolean(false)),
	}
	cursor := uint32(cursorEmbedded)
	if opts.HideCursor {
		cursor = cursorHidden
	}
	if modes, ok := p.property(screenCastIface, "AvailableCursorModes"); ok && modes&cursor != 0 {
		options = append(options, vardictEntry("cursor_mode", gVariantNewUint32(cursor)))
	}
	sessionArg := func() ptr { return gVariantNewObjectPath(cs(session)) }
	if _, err = p.request(ctx, screenCastIface, "SelectSources", []ptr{sessionArg()}, options...); err != nil {
		return "", 0, -1, err
	}
	res, err = p.request(ctx, screenCastIface, "Start", []ptr{sessionArg(), gVariantNewString(cs(""))})
	if err != nil {
		return "", 0, -1, err
	}
	streams := lookup(res, "streams", "a(ua{sv})")
	if streams == 0 || gVariantNChildren(streams) == 0 {
		if streams != 0 {
			gVariantUnref(streams)
		}
		return "", 0, -1, errors.New("screencapture: the portal shared nothing")
	}
	first := gVariantGetChildValue(streams, 0)
	id := gVariantGetChildValue(first, 0)
	node = gVariantGetUint32(id)
	gVariantUnref(id)
	gVariantUnref(first)
	gVariantUnref(streams)

	var gerr, fds ptr
	reply := gDBusConnectionCallWithUnixFDListSync(p.conn, cs(portalName), cs(portalPath), cs(screenCastIface), cs("OpenPipeWireRemote"),
		tuple(sessionArg(), vardict()), gVariantTypeNew(cs("(h)")), 0, -1, 0, &fds, 0, &gerr)
	if reply == 0 {
		return "", 0, -1, gErr(gerr)
	}
	h := gVariantGetChildValue(reply, 0)
	index := gVariantGetHandle(h)
	gVariantUnref(h)
	gVariantUnref(reply)
	if fds == 0 {
		return "", 0, -1, errors.New("screencapture: the portal opened no PipeWire remote")
	}
	fd = gUnixFDListGet(fds, index, &gerr)
	gObjectUnref(fds)
	if fd < 0 {
		return "", 0, -1, gErr(gerr)
	}
	return session, node, fd, nil
}

func (p *portal) closeSession(session string) {
	if res, err := p.call(session, "org.freedesktop.portal.Session", "Close", tuple(), "()"); err == nil {
		gVariantUnref(res)
	}
}
