# Screen capture

The screen capture plugin (`plugins/screencapture`) takes screenshots of
displays and windows, and records them as frames, with what each system
has for it: ScreenCaptureKit on macOS, Windows.Graphics.Capture on
Windows, and the portals of xdg-desktop-portal with PipeWire on Linux.
It is all Go: no JavaScript package, and nothing to `mygo.Use`. Pages
that only need to show or send the screen, as a video call does, can use
`navigator.mediaDevices.getDisplayMedia` instead.

```go
import "github.com/egoist/mygo/plugins/screencapture"

img, err := screencapture.Capture(ctx, screencapture.Source{}, screencapture.Options{})
```

`img` is an `*image.RGBA` of the main display, at its pixels: a Retina
display gives twice its size in points.

## Sources

The zero `Source` is the main display. `DisplaySource` takes the ID of
one of `mygo.Screen.Displays`, and `WindowSource` the ID of one of
`screencapture.Windows`, the windows of every app on the screen, front
first, with their titles, their apps and where they are:

```go
for _, d := range mygo.Screen.Displays() {
	img, err := screencapture.Capture(ctx, screencapture.DisplaySource(d.ID), screencapture.Options{})
	// ...
}

wins, err := screencapture.Windows(ctx)
for _, w := range wins {
	fmt.Println(w.App, w.Title, w.Bounds)
}
img, err := screencapture.Capture(ctx, wins[0].Source(), screencapture.Options{HideCursor: true})
```

A window is captured as it is, even behind others, but not while it is
minimized.

Linux knows no displays or windows by ID: its portal asks the user for
one, in a dialog of the desktop, each time. `Windows` returns
`errors.ErrUnsupported` there; a display source lets the user pick a
display, and a window source, of any ID, a window.

## Recordings

`Record` starts a recording, whose frames come on a channel until it
stops:

```go
rec, err := screencapture.Record(ctx, src, screencapture.Options{FrameRate: 30})
if err != nil {
	return err
}
time.AfterFunc(10*time.Second, func() { rec.Stop() })
for f := range rec.Frames() {
	encoder.Write(f.Image, f.Time) // f.Time is since the recording started
}
if err := rec.Err(); err != nil {
	return err
}
```

A recording stops when `Stop` is called, when its context is done, or on
its own: when the window closes, the display goes, or the user stops
sharing from the system's controls. `Err` then tells why, nil but for a
failure. `Stop` returns once the system has stopped capturing.

Frames are `*image.RGBA` images in sRGB, the reader's to keep. Systems
make a frame when the screen changes, at most `FrameRate` a second (30 by
default). A recording keeps up with the screen, not with its reader:
when the reader falls behind, the oldest frames waiting are dropped,
rather than the recording falling behind the screen.

The plugin encodes no video: pass the frames to the encoder you choose,
such as FFmpeg, through a pipe, or write them as PNGs.

## Permission

macOS asks the user once for the app, the first time it captures, and
the user allows it in System Settings (Privacy & Security, Screen &
System Audio Recording); the app sees the change when it starts again.
`screencapture.Access` reports whether it may capture, and
`screencapture.RequestAccess` asks, so that an app can explain first,
and show the way to the setting after:

```go
if !screencapture.Access() && !screencapture.RequestAccess() {
	showHowToAllowScreenRecording()
}
```

Before the user allows it, capturing fails with `screencapture.ErrDenied`.
macOS gives the permission to an app by its signature: in development,
the program run from a terminal asks for, and gets, the terminal's.

Windows asks nothing, and draws a border around what is captured while
it is.

Linux asks with each capture, in the portal's dialog: cancelling it
fails with `screencapture.ErrDenied`. `Capture` goes through the
Screenshot portal, which takes the whole desktop, or for a window source
lets the user pick what to take, and writes a file that the plugin reads
and removes. `Record` goes through the ScreenCast portal, whose frames
come through PipeWire.

## Platforms

| | macOS | Windows | Linux |
| --- | --- | --- | --- |
| API | ScreenCaptureKit | Windows.Graphics.Capture | xdg-desktop-portal, PipeWire |
| Since | macOS 12.3 | Windows 10, 1903 | the ScreenCast portal |
| Displays | by `mygo.Display.ID` | by `mygo.Display.ID` | the user picks |
| Windows | `Windows` | `Windows` | the user picks |
| Asks | once for the app | never | each time |
| `HideCursor` | yes | Windows 10, 2004 | if the portal can |

Browsers and other systems return `errors.ErrUnsupported`.
