# screencapture

Screenshots of displays and windows, and recordings of them as frames,
for [MyGo](https://github.com/egoist/mygo) apps: ScreenCaptureKit on
macOS, Windows.Graphics.Capture on Windows, and xdg-desktop-portal with
PipeWire on Linux. It is all Go.

```go
import "github.com/egoist/mygo/plugins/screencapture"

img, err := screencapture.Capture(ctx, screencapture.Source{}, screencapture.Options{})

rec, err := screencapture.Record(ctx, screencapture.DisplaySource(display.ID), screencapture.Options{FrameRate: 30})
for f := range rec.Frames() {
	encode(f.Image, f.Time)
}
```

`go test ./plugins/screencapture` with `MYGO_SCREENCAPTURE=1` captures
the real screen, and writes what it took to `MYGO_SCREENCAPTURE_OUT`.

See [the documentation](../../docs/plugins/screencapture.md).
