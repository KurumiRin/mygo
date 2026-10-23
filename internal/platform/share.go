package platform

// ShareItems is what a share sheet offers: at least one of Text, URL and
// Files, with a Title.
type ShareItems struct {
	// Title names what is shared, as Windows's share window shows it.
	Title string
	Text  string
	// URL is absolute.
	URL string
	// Files are absolute paths of files and folders that exist.
	Files []string
}

// Sharer is implemented by the windows of backends with a system share
// sheet: NSSharingServicePicker on macOS, DataTransferManager on Windows.
// The core asserts it only in Window.Share, so apps that never share do
// not link it. Others fail with ErrUnsupported.
type Sharer interface {
	// Share shows the share sheet for items, pointing at anchor, in DIPs
	// of the content area, where the system places it against one. done
	// runs on the main thread exactly once, when the sheet shows or with
	// why it could not.
	Share(items *ShareItems, anchor Rect, done func(error))
}
