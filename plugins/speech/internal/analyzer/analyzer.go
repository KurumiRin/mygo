// Package analyzer runs SpeechAnalyzer, the speech recognizer of macOS 26
// and later, for the speech plugin: a Swift API, which the plugin reaches
// through a shim of C functions in a library of its own
// (native/analyzer.swift), loaded with purego. The library is a release
// asset that `mygo build` and `mygo dev` put into apps, as mygo-plugin.json
// names it; on older systems, or without it, the plugin recognizes with
// SFSpeechRecognizer.
package analyzer
