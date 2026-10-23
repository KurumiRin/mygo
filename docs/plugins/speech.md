# Speech

The speech plugin turns speech into text: it listens to the microphone
and recognizes what the user says with the recognizer of the system,
SpeechAnalyzer on macOS 26 and later, SFSpeechRecognizer on older macOS,
and Windows.Media.SpeechRecognition on Windows. Pages use it through `@mygo-plugins/speech`; Go code and native
UI call the package directly.

Linux has no recognizer of the system, so there the plugin reports
speech recognition as unsupported, and so does a native UI app in a
browser.

## Set up

Use the plugin in Go:

```go
import (
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/speech"
)

func main() {
	mygo.Use(speech.Plugin)
	// ...
}
```

and add its package to the frontend:

```sh
bun add @mygo-plugins/speech
```

On macOS the app must say why it wants speech recognition and the
microphone, in the usage descriptions of its `Info.plist`, or macOS ends
the app when it asks for them. The plugin checks and fails with an error
naming the missing keys instead:

```ts
// mygo.config.ts
import { defineConfig } from "mygo-cli";

export default defineConfig({
  macos: {
    infoPlist: {
      NSSpeechRecognitionUsageDescription: "Dictate notes.",
      NSMicrophoneUsageDescription: "Dictate notes.",
    },
  },
});
```

The keys can be translated like the others of `Info.plist` (see
[Distribution](../distribution.md)). A program run without a bundle, as
`go run` does, has no `Info.plist`, so speech recognition needs the app
built or run with `mygo dev`.

## Pages

`listen` asks for permission, starts listening and resolves once the
microphone listens. Iterate the recognition for the results: each has the
transcript of everything said so far, which the recognizer may revise
until the final result.

```ts
import { isSupported, listen, SpeechError } from "@mygo-plugins/speech";

micButton.hidden = !(await isSupported());

micButton.onclick = async () => {
  const recognition = await listen({ locale: "en-US" });
  stopButton.onclick = () => recognition.stop();
  try {
    for await (const result of recognition) {
      input.value = result.text;
      if (result.final) console.log("done:", result.text);
    }
  } catch (err) {
    if (err instanceof SpeechError && err.code === "denied") showSettingsHint();
    else throw err;
  }
};
```

- `stop()` stops listening: the recognizer finishes what it heard and
  sends the final result, then the iteration ends.
- `cancel()`, breaking out of the loop, an aborted `signal` or the page
  navigating stop at once, without a final result.
- `closed` settles once the recognition ended, rejecting with what ended
  it.
- `requestPermission()` asks for permission without listening, for an
  app that explains before the system's prompt. On Windows, which has no
  prompt for desktop apps, it resolves, and `listen` rejects with
  `denied` when Settings refuse.

A `SpeechError` has a code:

| Code | Meaning |
| --- | --- |
| `unsupported` | the system has no recognizer, or not the one asked for (`onDevice`) |
| `denied` | the user or the system doesn't let the app recognize speech or use the microphone |
| `language` | the system can't recognize `locale` |
| `busy` | another recognition is running: an app recognizes one at a time |
| `failed` | something else failed, as the network, said in the message |

## Go and native UI

```go
r, err := speech.Start(ctx, speech.Options{Locale: "en-US"})
if err != nil {
	return err // speech.ErrUnsupported, ErrDenied, ErrLanguage, ErrBusy…
}
go func() {
	<-stopClicked
	r.Stop() // the final result follows
}()
for res := range r.Results() {
	field.SetText(res.Text)
}
return r.Err() // nil after Stop and silence, context.Canceled after Cancel
```

`speech.Supported()` reports whether the system has a recognizer, and
`speech.RequestPermission(ctx)` asks for permission. Canceling the
context of `Start` cancels the recognition. Results arrive on a channel
that never makes the recognizer wait: a result the receiver was too slow
to take gives way to the next. Every function is safe from any goroutine,
the main one included.

## Options

| Option | Meaning |
| --- | --- |
| `Locale` / `locale` | the language spoken, a BCP 47 tag such as `en-US`: the user's language by default |
| `OnDevice` / `onDevice` | requires recognition on the device, which sends no audio to a server (SpeechAnalyzer always is) |

## Platforms

- **macOS 26 and later** recognize with SpeechAnalyzer's
  SpeechTranscriber, on the device, through a small Swift library of the
  plugin's (SpeechAnalyzer is a Swift API, which purego doesn't reach),
  which `mygo build` and `mygo dev` put into the app, and which programs
  run with `go run` download into the user's cache. The first use of a
  language downloads its model, which the system then keeps for every
  app. It has no time limit.
- **macOS 12 to 15**, and macOS 26 where the transcriber has no model of
  the language or the library is missing, recognize with
  SFSpeechRecognizer, the microphone through AVAudioEngine. Recognition
  happens on Apple's servers unless the language supports it on the
  device, which `OnDevice` requires; a server recognition ends after
  about a minute. On macOS 13 and later the transcript has punctuation.
- On every version, the first time, macOS asks the user for speech
  recognition and for the microphone; afterwards System Settings > Privacy
  & Security decides.
- **Windows** dictates with Windows.Media.SpeechRecognition's continuous
  recognition, which ends after a long silence too. It needs online
  speech recognition on (Settings > Privacy > Speech) and the microphone
  allowed for desktop apps (Settings > Privacy > Microphone): Windows
  doesn't prompt desktop apps, so `listen` fails with `denied` and a
  message saying which. The language needs its speech pack installed;
  `OnDevice` is unsupported.
- **Linux** and browsers: unsupported.
