//go:build windows && (amd64 || arm64)

package speech

import (
	"crypto/sha1"
	"encoding/binary"
	"testing"
)

// piid derives the IID of a generic WinRT interface or delegate from its
// type signature, as the WinRT type system does: a version 5 UUID in its
// namespace.
func piid(signature string) guid {
	ns := []byte{0x11, 0xf4, 0x7a, 0xd5, 0x7b, 0x73, 0x42, 0xc0, 0xab, 0xae, 0x87, 0x8b, 0x1e, 0x16, 0xad, 0xee}
	h := sha1.Sum(append(ns, signature...))
	h[6] = h[6]&0x0f | 0x50
	h[8] = h[8]&0x3f | 0x80
	g := guid{
		data1: binary.BigEndian.Uint32(h[0:]),
		data2: binary.BigEndian.Uint16(h[4:]),
		data3: binary.BigEndian.Uint16(h[6:]),
	}
	copy(g.data4[:], h[8:16])
	return g
}

func TestDelegateIIDs(t *testing.T) {
	const (
		typedEventHandler = "{9de1c534-6ae1-11e0-84e1-18a905bcc53f}"
		operationHandler  = "{fcdcf02c-e5d8-4478-915a-4d90b74b83a5}"
		ns                = "Windows.Media.SpeechRecognition."
	)
	rc := func(name, iid string) string { return "rc(" + ns + name + ";{" + iid + "})" }
	recognizer := rc("SpeechRecognizer", "0bc3c9cb-c26a-40f2-aeb5-8096b2e48073")
	session := rc("SpeechContinuousRecognitionSession", "6a213c04-6614-49f8-99a2-b5e9b3a085c8")
	for _, c := range []struct {
		name      string
		signature string
		want      guid
	}{
		// A known one: IIterable<String>.
		{"IIterable<String>", "pinterface({faa585ea-6214-4217-afda-7f46de5869b3};string)", mustGUID("e2fcc7c1-3bfc-5a0b-b2b0-72e769d1cb7e")},
		{"compile", "pinterface(" + operationHandler + ";" + rc("SpeechRecognitionCompilationResult", "407e6c5d-6ac7-4da4-9cc1-2fce32cf7489") + ")", iidCompileCompleted},
		{"hypothesis", "pinterface(" + typedEventHandler + ";" + recognizer + ";" + rc("SpeechRecognitionHypothesisGeneratedEventArgs", "55161a7a-8023-5866-411d-1213bb271476") + ")", iidHypothesisHandler},
		{"result", "pinterface(" + typedEventHandler + ";" + session + ";" + rc("SpeechContinuousRecognitionResultGeneratedEventArgs", "19091e1e-6e7e-5a46-40fb-76594f786504") + ")", iidResultHandler},
		{"completed", "pinterface(" + typedEventHandler + ";" + session + ";" + rc("SpeechContinuousRecognitionCompletedEventArgs", "e3d069bb-e30c-5e18-424b-7fbe81f8fbd0") + ")", iidCompletedHandler},
	} {
		if got := piid(c.signature); got != c.want {
			t.Errorf("%s: IID %x, want %x", c.name, got, c.want)
		}
	}
}

// Supported asks Windows for the recognizer's class, which must not fail
// badly where there is none, as on some servers.
func TestSupported(t *testing.T) {
	t.Logf("supported: %v", Supported())
}
