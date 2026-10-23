package mygo

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"

	"github.com/egoist/mygo/internal/platform"
)

// ShareOptions is what Window.Share offers: text, a link, files, or any of
// them together. At least one is required.
type ShareOptions struct {
	// Text is text to share, such as a message or a quote.
	Text string
	// URL is a link to share. It must be absolute, as
	// "https://example.com/post/1".
	URL string
	// Files are the files and folders to share, by path; a relative path
	// is resolved against the working directory. Each must exist.
	Files []string
	// Title names what is shared, as Windows's share window shows it at
	// its top. Empty is the name of the first file, else the URL, else the
	// text. macOS's picker shows no title.
	Title string
	// Anchor is the area of the window's content that macOS's picker
	// points at, such as the button that shares, in DIPs from the top left
	// of the content. The zero Anchor is the middle of the content.
	// Windows places its share window itself.
	Anchor Rectangle
}

// Share shows the system's share sheet for the window, which offers the
// apps and services that can take what opts holds: Mail, Messages,
// AirDrop and Notes on macOS (NSSharingServicePicker), and Nearby Share,
// Mail and the apps that are share targets on Windows (the share window
// of DataTransferManager). It returns once the sheet shows, or with why it
// could not; the user then picks where to share or dismisses it, which
// the system does not tell the app on Windows, so neither does Share.
//
//	err := win.Share(mygo.ShareOptions{
//		URL:    "https://example.com/post/1",
//		Anchor: shareButtonBounds,
//	})
//	if errors.Is(err, errors.ErrUnsupported) {
//		// Linux has no share sheet: copy the link instead.
//	}
//
// Linux desktops and browsers have no share sheet: there Share fails with
// an error that errors.Is(err, errors.ErrUnsupported) holds for. The window
// must be showing. Share may be called from any goroutine.
func (w *Window) Share(opts ShareOptions) error {
	items, err := shareItems(opts)
	if err != nil {
		return err
	}
	ch := make(chan error, 1)
	shown := false
	w.do(func(n platform.Window) {
		shown = true
		s, ok := n.(platform.Sharer)
		if !ok {
			deliver(ch, platform.ErrUnsupported)
			return
		}
		anchor := platform.Rect(opts.Anchor)
		if anchor == (platform.Rect{}) {
			c := n.ContentBounds()
			anchor = platform.Rect{X: c.Width / 2, Y: c.Height / 2, Width: 1, Height: 1}
		}
		s.Share(items, anchor, func(err error) { deliver(ch, err) })
	})
	if !shown {
		return errDestroyed
	}
	return await(ch)
}

// shareItems validates opts and returns what the backend shares, with
// absolute paths and the default title.
func shareItems(opts ShareOptions) (*platform.ShareItems, error) {
	if opts.Text == "" && opts.URL == "" && len(opts.Files) == 0 {
		return nil, errors.New("mygo: Window.Share needs text, a URL or files")
	}
	items := &platform.ShareItems{Title: opts.Title, Text: opts.Text, URL: opts.URL}
	if opts.URL != "" {
		if u, err := url.Parse(opts.URL); err != nil || !u.IsAbs() {
			return nil, errors.New("mygo: Window.Share needs an absolute URL, got " + opts.URL)
		}
	}
	for _, f := range opts.Files {
		abs, err := filepath.Abs(f)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, err
		}
		items.Files = append(items.Files, abs)
	}
	if items.Title == "" {
		switch {
		case len(items.Files) > 0:
			items.Title = filepath.Base(items.Files[0])
		case items.URL != "":
			items.Title = items.URL
		default:
			items.Title = items.Text
		}
	}
	return items, nil
}
