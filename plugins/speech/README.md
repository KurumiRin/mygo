# @mygo-plugins/speech

Speech to text for [MyGo](https://github.com/egoist/mygo) apps' pages:
the microphone, recognized by the system's recognizer: SpeechAnalyzer on
macOS 26 and later, SFSpeechRecognizer on older macOS, and
Windows.Media.SpeechRecognition on Windows.

Use the plugin in Go:

```go
import "github.com/egoist/mygo/plugins/speech"

mygo.Use(speech.Plugin)
```

and listen in the frontend:

```ts
import { listen } from "@mygo-plugins/speech";

const recognition = await listen({ locale: "en-US" });
stopButton.onclick = () => recognition.stop();
for await (const result of recognition) {
  input.value = result.text; // the transcript so far; result.final on the last
}
```

On macOS the app's `Info.plist` needs `NSSpeechRecognitionUsageDescription`
and `NSMicrophoneUsageDescription` (`macos.infoPlist` of `mygo.config.ts`).
Linux has no recognizer: `listen` fails there with a `SpeechError` whose
code is `unsupported`.

[The documentation](https://github.com/egoist/mygo/blob/main/docs/plugins/speech.md)
covers the errors, the Go API for native UI and how each platform
recognizes.
