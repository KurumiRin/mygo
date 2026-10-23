package analyzer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	_ "embed"

	"github.com/ebitengine/purego"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/internal/download"
)

//go:generate go run ./libbuild

// natives names the builds of the shim, which the CLI puts into apps.
//
//go:embed mygo-plugin.json
var natives []byte

// The events of a recognition, as the shim sends them.
const (
	EventReady       = 0 // the microphone listens
	EventText        = 1 // the transcript so far
	EventFinal       = 2 // the last transcript, after Stop: the end
	EventFailed      = 3 // the end, with what failed
	EventCanceled    = 4 // the end, after Cancel
	EventUnavailable = 5 // the end, before ready: no transcriber here
	EventLanguage    = 6 // the end, before ready: the language is not supported
)

var (
	loadOnce  sync.Once
	available bool

	shimStart  func(id uint64, locale *byte, callback uintptr)
	shimStop   func(id uint64)
	shimCancel func(id uint64)
	callback   uintptr

	handlersMu sync.Mutex
	handlers   = map[uint64]func(event int32, text string){}
	lastID     uint64
)

// Available loads the shim, once, and reports whether SpeechAnalyzer's
// transcriber runs on this system.
func Available() bool {
	loadOnce.Do(func() {
		path, ok := libraryPath()
		if !ok {
			return
		}
		lib, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			return
		}
		var avail func() int32
		for name, fn := range map[string]any{
			"mygo_speech_available": &avail, "mygo_speech_start": &shimStart,
			"mygo_speech_stop": &shimStop, "mygo_speech_cancel": &shimCancel,
		} {
			sym, err := purego.Dlsym(lib, name)
			if err != nil {
				return
			}
			purego.RegisterFunc(fn, sym)
		}
		// One callback for every recognition, routed by id.
		callback = purego.NewCallback(func(id uint64, event int32, text *byte) {
			s := goString(text)
			handlersMu.Lock()
			h := handlers[id]
			if event >= EventFinal {
				delete(handlers, id)
			}
			handlersMu.Unlock()
			if h != nil {
				h(event, s)
			}
		})
		available = avail() == 1
	})
	return available
}

func goString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

// Start starts a recognition in the language of the BCP 47 tag locale, the
// user's when empty, and calls handle with its events, on the shim's
// threads, until one ends it (EventFinal and after). It returns the
// recognition's id.
func Start(locale string, handle func(event int32, text string)) uint64 {
	handlersMu.Lock()
	lastID++
	id := lastID
	handlers[id] = handle
	handlersMu.Unlock()
	tag := append([]byte(locale), 0)
	shimStart(id, &tag[0], callback)
	runtime.KeepAlive(tag)
	return id
}

// Stop stops listening: the final transcript follows.
func Stop(id uint64) { shimStop(id) }

// Cancel ends the recognition at once.
func Cancel(id uint64) { shimCancel(id) }

type nativeFile struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// libraryPath finds the shim: the file $MYGO_SPEECH_ANALYZER names, the
// app's resources, the executable's directory, then the CLI's cache,
// where programs that are not packaged apps download it. Without a build
// for this platform, or when it can't be had, the plugin does without.
func libraryPath() (string, bool) {
	if p := os.Getenv("MYGO_SPEECH_ANALYZER"); p != "" {
		return p, true
	}
	var m struct {
		Libraries []struct {
			Name  string                `json:"name"`
			Files map[string]nativeFile `json:"files"`
		} `json:"libraries"`
	}
	if json.Unmarshal(natives, &m) != nil {
		return "", false
	}
	var file nativeFile
	for _, lib := range m.Libraries {
		if lib.Name == "mygo-speech" {
			file = lib.Files[runtime.GOOS+"-"+runtime.GOARCH]
		}
	}
	sum, err := hex.DecodeString(file.SHA256)
	if err != nil || len(sum) != sha256.Size || file.URL == "" || file.Name == "" ||
		filepath.Base(file.Name) != file.Name || strings.ContainsAny(file.Name, `/\`) || file.Name == ".." {
		return "", false
	}
	var dirs []string
	if dir, err := mygo.App.Path(mygo.PathResources); err == nil {
		dirs = append(dirs, dir)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, dir := range dirs {
		p := filepath.Join(dir, file.Name)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p, true
		}
	}
	if mygo.App.IsPackaged() {
		return "", false
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", false
	}
	p := filepath.Join(cache, "mygo", "natives", file.SHA256, file.Name)
	if download.Check(p, file.SHA256) {
		return p, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if download.Cached(ctx, download.File{URL: file.URL, SHA256: file.SHA256}, p) != nil {
		return "", false
	}
	return p, true
}
