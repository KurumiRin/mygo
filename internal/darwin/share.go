//go:build darwin

package darwin

import (
	"errors"

	"github.com/egoist/mygo/internal/platform"
)

// nsMinYEdge is the bottom edge of a rectangle in a view that is not
// flipped (NSRectEdge): the picker shows below the anchor.
const nsMinYEdge = 1

// Share shows an NSSharingServicePicker of the items, from the anchor in
// the content view, which is not flipped. The picker lists the services
// that take all of the items: text as an NSString, the link and the files
// as NSURLs. AppKit holds the picker only while it shows, so the window
// keeps the last one.
func (w *window) Share(items *platform.ShareItems, anchor platform.Rect, done func(error)) {
	if w.closed || w.view == 0 {
		done(errDestroyed)
		return
	}
	if win := send(w.view, "window"); win == 0 || !sendBool(win, "isVisible") {
		done(errors.New("mygo: Window.Share needs a window that shows"))
		return
	}
	withPool(func() {
		var objs []id
		if items.Text != "" {
			objs = append(objs, nsString(items.Text))
		}
		if items.URL != "" {
			if u := nsURL(items.URL); u != 0 {
				objs = append(objs, u)
			}
		}
		for _, f := range items.Files {
			objs = append(objs, fileURL(f))
		}
		picker := send(send(class("NSSharingServicePicker"), "alloc"), "initWithItems:", uintptr(nsArray(objs...)))
		release(w.sharePicker)
		w.sharePicker = picker
		width, height := float64(max(anchor.Width, 1)), float64(max(anchor.Height, 1))
		h := msgRect(w.view, sel("bounds")).Size.Height
		r := NSRect{Origin: NSPoint{float64(anchor.X), h - float64(anchor.Y) - height}, Size: NSSize{width, height}}
		msgRectIDUint(picker, sel("showRelativeToRect:ofView:preferredEdge:"), r, w.view, nsMinYEdge)
	})
	done(nil)
}
