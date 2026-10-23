//go:build linux && (amd64 || arm64)

package screencapture

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ebitengine/purego"
)

// The portals answer the methods that may ask the user with a Response
// signal of a request object, later. A portal session runs on a thread of
// its own, with a GLib main context of its own, which it turns while it
// waits, so that it works with or without the app's GTK main loop.

const (
	portalName = "org.freedesktop.portal.Desktop"
	portalPath = "/org/freedesktop/portal/desktop"
)

// The response codes of the portals' requests.
const (
	responseSuccess   = 0
	responseCancelled = 1
)

var (
	signalOnce sync.Once
	signalCB   ptr
	portalsMu  sync.Mutex
	portals    = map[ptr]*portal{}
	portalIDs  ptr
	tokens     atomic.Uint64
)

// A portal is the connection to the desktop portal of one capture, on the
// thread it was made on, which it must stay on.
type portal struct {
	id      ptr
	conn    ptr
	mainCtx ptr
	sender  string

	// The request the portal waits for.
	waiting string
	code    uint32
	results ptr
	got     bool
}

// newPortal connects to the portal. The caller has locked its goroutine
// to its thread, and closes the portal on it.
func newPortal() (*portal, error) {
	if err := loadGLib(); err != nil {
		return nil, err
	}
	signalOnce.Do(func() {
		signalCB = purego.NewCallback(func(conn, sender, path, iface, signal, params, data ptr) {
			portalsMu.Lock()
			p := portals[data]
			portalsMu.Unlock()
			if p != nil {
				p.signal(goStr(path), params)
			}
		})
	})
	var gerr ptr
	conn := gBusGetSync(2, 0, &gerr) // G_BUS_TYPE_SESSION
	if conn == 0 {
		return nil, fmt.Errorf("%w: %w", gErr(gerr), errors.ErrUnsupported)
	}
	p := &portal{conn: conn, mainCtx: gMainContextNew()}
	gMainContextPushThreadDefault(p.mainCtx)
	p.sender = strings.ReplaceAll(strings.TrimPrefix(goStr(gDBusConnectionGetUniqueName(conn)), ":"), ".", "_")
	portalsMu.Lock()
	portalIDs++
	p.id = portalIDs
	portals[p.id] = p
	portalsMu.Unlock()
	return p, nil
}

func (p *portal) close() {
	portalsMu.Lock()
	delete(portals, p.id)
	portalsMu.Unlock()
	if p.results != 0 {
		gVariantUnref(p.results)
		p.results = 0
	}
	gMainContextPopThreadDefault(p.mainCtx)
	gMainContextUnref(p.mainCtx)
	gObjectUnref(p.conn)
}

func (p *portal) signal(path string, params ptr) {
	if path != p.waiting || p.got {
		return
	}
	code := gVariantGetChildValue(params, 0)
	p.code = gVariantGetUint32(code)
	gVariantUnref(code)
	p.results = gVariantGetChildValue(params, 1)
	p.got = true
}

// call calls a method of the portal, and returns its reply, which the
// caller unrefs.
func (p *portal) call(path, iface, method string, params ptr, replyType string) (ptr, error) {
	var gerr ptr
	res := gDBusConnectionCallSync(p.conn, cs(portalName), cs(path), cs(iface), cs(method), params, gVariantTypeNew(cs(replyType)), 0, -1, 0, &gerr)
	if res == 0 {
		return 0, gErr(gerr)
	}
	return res, nil
}

// property reads a property of type u of a portal interface, if the
// desktop offers it.
func (p *portal) property(iface, name string) (uint32, bool) {
	res, err := p.call(portalPath, "org.freedesktop.DBus.Properties", "Get",
		tuple(gVariantNewString(cs(iface)), gVariantNewString(cs(name))), "(v)")
	if err != nil {
		return 0, false
	}
	defer gVariantUnref(res)
	v := gVariantGetChildValue(res, 0)
	inner := gVariantGetVariant(v)
	gVariantUnref(v)
	defer gVariantUnref(inner)
	if goStr(gVariantGetTypeString(inner)) != "u" {
		return 0, false
	}
	return gVariantGetUint32(inner), true
}

// request calls a portal method whose last argument is a vardict of
// options, and waits for its Response, turning the portal's main context.
// It returns the results, which stay the portal's until the next
// request, and ErrDenied when the user cancelled.
func (p *portal) request(ctx context.Context, iface, method string, args []ptr, options ...ptr) (ptr, error) {
	if p.results != 0 {
		gVariantUnref(p.results)
		p.results = 0
	}
	token := "mygo_screencapture" + strconv.FormatUint(tokens.Add(1), 10)
	p.waiting = portalPath + "/request/" + p.sender + "/" + token
	p.got = false
	sub := gDBusConnectionSignalSubscribe(p.conn, cs(portalName), cs("org.freedesktop.portal.Request"), cs("Response"), nil, nil, 0, signalCB, p.id, 0)
	defer gDBusConnectionSignalUnsubscribe(p.conn, sub)
	options = append(options, vardictEntry("handle_token", gVariantNewString(cs(token))))
	res, err := p.call(portalPath, iface, method, tuple(append(args, vardict(options...))...), "(o)")
	if err != nil {
		return 0, err
	}
	handle := gVariantGetChildValue(res, 0)
	p.waiting = goStr(gVariantGetString(handle, nil)) // portals before 0.9 pick the path
	gVariantUnref(handle)
	gVariantUnref(res)

	stop := context.AfterFunc(ctx, func() { gMainContextWakeup(p.mainCtx) })
	defer stop()
	for !p.got {
		if ctx.Err() != nil {
			if r, err := p.call(p.waiting, "org.freedesktop.portal.Request", "Close", tuple(), "()"); err == nil {
				gVariantUnref(r)
			}
			return 0, ctx.Err()
		}
		gMainContextIteration(p.mainCtx, true)
	}
	switch p.code {
	case responseSuccess:
		return p.results, nil
	case responseCancelled:
		return 0, ErrDenied
	}
	return 0, fmt.Errorf("screencapture: the portal failed %s.%s: %w", iface, method, ErrDenied)
}
