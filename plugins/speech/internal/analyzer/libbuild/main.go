// Builds the speech plugin's SpeechAnalyzer shim with Xcode 26 or later,
// on macOS, for arm64 and amd64:
//
//	go generate ./plugins/speech/internal/analyzer
//
// It writes the libraries into build/ (-out), to publish as the assets of
// the release that -release names, and mygo-plugin.json with their
// SHA-256, and puts them into the CLI's cache, so that mygo dev and build
// work before the release is published.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// version numbers the builds of the shim: raise it with every change of
// native/analyzer.swift.
const version = "1"

const (
	name = "mygo-speech"
	file = "libmygo-speech.dylib"
	// The oldest macOS MyGo runs on: there the shim loads and reports
	// SpeechAnalyzer unavailable.
	minimumMacOS = "12.0"
)

var targets = []struct{ name, arch string }{
	{"darwin-arm64", "arm64"},
	{"darwin-amd64", "x86_64"},
}

type nativeFile struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type library struct {
	Name   string                `json:"name"`
	Source string                `json:"source"`
	Files  map[string]nativeFile `json:"files"`
}

func main() {
	out := flag.String("out", "build", "output directory")
	release := flag.String("release", "https://github.com/egoist/mygo/releases/download/speech-analyzer-"+version, "release asset base URL")
	flag.Parse()
	*out, _ = filepath.Abs(*out)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	src, err := filepath.Abs(filepath.Join("native", "analyzer.swift"))
	if err != nil {
		log.Fatal(err)
	}
	lib := library{Name: name, Source: "plugins/speech/internal/analyzer/native/analyzer.swift", Files: map[string]nativeFile{}}
	for _, t := range targets {
		asset := name + "-" + t.name + ".dylib"
		path := filepath.Join(*out, asset)
		log.Printf("building %s", asset)
		cmd := exec.Command("xcrun", "swiftc", "-emit-library", "-O", "-swift-version", "5",
			"-module-name", "MygoSpeech", "-target", t.arch+"-apple-macos"+minimumMacOS,
			"-Xlinker", "-install_name", "-Xlinker", "@rpath/"+file,
			"-o", path, src)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			log.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			log.Fatal(err)
		}
		sum := sha256.Sum256(data)
		digest := hex.EncodeToString(sum[:])
		cache, err := os.UserCacheDir()
		if err != nil {
			log.Fatal(err)
		}
		dir := filepath.Join(cache, "mygo", "natives", digest)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), data, 0o755); err != nil {
			log.Fatal(err)
		}
		lib.Files[t.name] = nativeFile{file, strings.TrimRight(*release, "/") + "/" + asset, digest}
	}
	data, err := json.MarshalIndent(struct {
		Libraries []library `json:"libraries"`
	}{[]library{lib}}, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("mygo-plugin.json", append(data, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote mygo-plugin.json; publish %s as the assets of %s", *out, *release)
}
