//go:build !js

package e2e

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo"
)

// TestContentRules loads a page whose script the rules block and one of
// whose elements they hide, then loads it again without the rules.
func TestContentRules(t *testing.T) {
	var adLoads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ads/ad.js":
			adLoads.Add(1)
			w.Header().Set("Content-Type", "text/javascript")
			fmt.Fprint(w, "window.adLoaded = true")
			return
		case "/app.js":
			w.Header().Set("Content-Type", "text/javascript")
			fmt.Fprint(w, "window.appLoaded = true")
			return
		}
		fmt.Fprint(w, `<!doctype html><title>page</title>
<div class="banner" id="banner">ad</div><div id="text">text</div>
<script src="/app.js"></script><script src="/ads/ad.js"></script>
<script>document.title = "loaded"</script>`)
	}))
	defer server.Close()

	// Rules of their own per run, so that they compile rather than load,
	// and replace the list of the run before.
	const id = "e2e"
	source := `[
		{"trigger":{"url-filter":"/run-` + fmt.Sprint(time.Now().UnixNano()) + `/"},"action":{"type":"block"}},
		{"trigger":{"url-filter":"/ads/","resource-type":["script"]},"action":{"type":"block"}},
		{"trigger":{"url-filter":".*"},"action":{"type":"css-display-none","selector":".banner"}}]`
	_, err := mygo.CompileContentRules(id, source)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	// The same rules again load the list compiled.
	rules, err := mygo.CompileContentRules(id, source)
	if err != nil {
		t.Fatal(err)
	}
	// WebKit takes no alternatives, nor a list without a rule; it skips a
	// rule whose selector it cannot parse.
	for _, bad := range []string{
		`[{"trigger":{"url-filter":"a|b"},"action":{"type":"block"}}]`,
		`[]`,
	} {
		if _, err := mygo.CompileContentRules(id+"-bad", bad); err == nil {
			t.Errorf("%s compiled", bad)
		}
	}
	if _, err := mygo.CompileContentRules(id+"-bad", `[
		{"trigger":{"url-filter":".*"},"action":{"type":"css-display-none","selector":"a[[["}},
		{"trigger":{"url-filter":"x"},"action":{"type":"block"}}]`); err != nil {
		t.Errorf("a rule of a selector the engine cannot parse: %v", err)
	}

	w := newWindow(t, mygo.WindowOptions{Title: "Content rules", Width: 300, Height: 200, Page: mygo.PageOptions{ContentRules: []*mygo.ContentRules{rules}}})
	if err := w.Page().LoadURL(server.URL + "/"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, "document.title === 'loaded'")
	state := func() string {
		s, err := mygo.EvalAs[string](w.Page(), `[!!window.appLoaded, !!window.adLoaded,
			getComputedStyle(document.getElementById("banner")).display,
			getComputedStyle(document.getElementById("text")).display].join(" ")`)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := state(); got != "true false none block" {
		t.Errorf("with the rules: app, ad, banner and text are %q, want the ad blocked and the banner hidden", got)
	}
	if n := adLoads.Load(); n != 0 {
		t.Errorf("the blocked script was requested %d times", n)
	}

	w.Page().SetContentRules()
	w.Page().Reload()
	waitFor(t, w, "document.title === 'loaded' && window.adLoaded === true")
	if got := state(); got != "true true block block" {
		t.Errorf("without the rules: %q", got)
	}
}
