//go:build !js

package mygo

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/egoist/mygo/internal/fake"
	"github.com/egoist/mygo/internal/platform"
)

func TestShare(t *testing.T) {
	w, fw := testWindow(t, WindowOptions{Width: 400, Height: 300})
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("photo.png", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	photo := filepath.Join(dir, "photo.png")

	// Paths become absolute, the title defaults to the first file's name,
	// and the zero anchor is the middle of the content.
	if err := w.Share(ShareOptions{Text: "Look", Files: []string{"photo.png", dir}}); err != nil {
		t.Fatal(err)
	}
	want := fake.Share{
		Items:  platform.ShareItems{Title: "photo.png", Text: "Look", Files: []string{photo, dir}},
		Anchor: platform.Rect{X: 200, Y: 150, Width: 1, Height: 1},
	}
	if len(fw.Shares) != 1 || !reflect.DeepEqual(fw.Shares[0], want) {
		t.Fatalf("Shares = %+v, want [%+v]", fw.Shares, want)
	}

	// Without files the title is the URL, else the text; an anchor is
	// passed as it is.
	anchor := Rectangle{X: 10, Y: 20, Width: 30, Height: 24}
	if err := w.Share(ShareOptions{URL: "https://example.com/a", Text: "Read", Anchor: anchor}); err != nil {
		t.Fatal(err)
	}
	if err := w.Share(ShareOptions{Text: "Hello"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Share(ShareOptions{Text: "Hello", Title: "Greeting"}); err != nil {
		t.Fatal(err)
	}
	if got := fw.Shares[1]; got.Items.Title != "https://example.com/a" || got.Anchor != platform.Rect(anchor) {
		t.Errorf("share of a URL = %+v", got)
	}
	if got := fw.Shares[2].Items.Title; got != "Hello" {
		t.Errorf("title of a text = %q, want the text", got)
	}
	if got := fw.Shares[3].Items.Title; got != "Greeting" {
		t.Errorf("title = %q, want Greeting", got)
	}

	// What cannot be shared fails before the backend sees it.
	for _, opts := range []ShareOptions{
		{},
		{Title: "Nothing"},
		{URL: "/relative"},
		{URL: "://"},
		{Files: []string{"missing.txt"}},
	} {
		if err := w.Share(opts); err == nil {
			t.Errorf("Share(%+v) did not fail", opts)
		}
	}
	if len(fw.Shares) != 4 {
		t.Errorf("the backend showed %d share sheets, want 4", len(fw.Shares))
	}

	// The backend's failure is the caller's.
	boom := errors.New("boom")
	onMain(func() { fw.ShareError = boom })
	if err := w.Share(ShareOptions{Text: "Hello"}); err != boom {
		t.Errorf("Share = %v, want %v", err, boom)
	}

	// A destroyed window shares nothing.
	w.Destroy()
	if err := w.Share(ShareOptions{Text: "Hello"}); err != errDestroyed {
		t.Errorf("Share of a destroyed window = %v, want %v", err, errDestroyed)
	}
}
