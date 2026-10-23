// Share shows the system's share sheet for text, a link and a file: the
// picker of macOS's sharing services, or Windows's share window. Linux has
// none, which the window says.
//
//	go run ./examples/share
package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

type app struct {
	win    *mygo.Window
	text   string
	file   string // a file of the text, to share as a file
	status string
}

func (a *app) view(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Fill().Center().Gap(12).Padding(20).Children(func() {
		ui.TextInput(c.Key("text"), &a.text).Width(280)
		ui.Row(c).Gap(8).Children(func() {
			if b := ui.Button(c.Key("text-button"), "Share text"); b.Clicked() {
				a.share(b.Bounds(), mygo.ShareOptions{Text: a.text})
			}
			if b := ui.Button(c.Key("link"), "Share link"); b.Clicked() {
				a.share(b.Bounds(), mygo.ShareOptions{Title: "MyGo", URL: "https://github.com/egoist/mygo"})
			}
			if b := ui.PrimaryButton(c.Key("file"), "Share file"); b.Clicked() {
				if err := os.WriteFile(a.file, []byte(a.text), 0o644); err != nil {
					a.status = err.Error()
				} else {
					a.share(b.Bounds(), mygo.ShareOptions{Files: []string{a.file}})
				}
			}
		})
		ui.Text(c, a.status).FontSize(12).TextColor(t.TextMuted)
	})
}

// share shows the share sheet from the button, on a goroutine: Share waits
// until the sheet shows.
func (a *app) share(button ui.Rect, opts mygo.ShareOptions) {
	opts.Anchor = mygo.Rectangle{X: int(button.X), Y: int(button.Y), Width: int(button.W), Height: int(button.H)}
	go func() {
		err := a.win.Share(opts)
		a.win.Update(func() {
			switch {
			case errors.Is(err, errors.ErrUnsupported):
				a.status = "This system has no share sheet"
			case err != nil:
				a.status = err.Error()
			default:
				a.status = "Shown"
			}
		})
	}()
}

func main() {
	a := &app{text: "Hello from MyGo", file: filepath.Join(os.TempDir(), "mygo-share.txt")}
	mygo.App.WhenReady(func() {
		a.win = mygo.NewWindow(mygo.WindowOptions{
			Title:   "Share",
			Width:   400,
			Height:  200,
			Content: ui.View(a.view),
		})
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
