/**
 * The speech plugin of MyGo: speech to text from the microphone, with the
 * recognizer of the system (SpeechAnalyzer on macOS 26 and later,
 * SFSpeechRecognizer on older macOS, Windows.Media.SpeechRecognition on
 * Windows). The Go side must use the
 * plugin:
 *
 * ```go
 * mygo.Use(speech.Plugin) // github.com/egoist/mygo/plugins/speech
 * ```
 *
 * ```ts
 * import { listen } from "@mygo-plugins/speech";
 *
 * const recognition = await listen({ locale: "en-US" });
 * stopButton.onclick = () => recognition.stop();
 * for await (const result of recognition) {
 *   input.value = result.text; // the transcript so far
 * }
 * ```
 *
 * @module
 */
import { Channel, call } from "mygo-runtime";

/** What the recognizer heard so far. */
export interface SpeechResult {
  /** The transcript of everything said since {@link listen}, which the recognizer may revise until the final result. */
  text: string;
  /** Whether it is the last result, sent once the recognition ended without being canceled. */
  final: boolean;
}

/** Options of {@link listen}. */
export interface ListenOptions {
  /** The language spoken, a BCP 47 tag such as "en-US": the user's language by default. */
  locale?: string;
  /** Requires recognition on the device, which sends no audio to a server: macOS has it for some languages. */
  onDevice?: boolean;
  /** Cancels the recognition when aborted. */
  signal?: AbortSignal;
}

/** Why a recognition failed; see {@link SpeechError}. */
export type SpeechErrorCode = "unsupported" | "denied" | "language" | "busy" | "failed";

/** A recognition that failed to start or failed, with the reason as its code. */
export class SpeechError extends Error {
  constructor(
    readonly code: SpeechErrorCode,
    message: string,
  ) {
    super(message);
    this.name = "SpeechError";
  }
}

/**
 * A running recognition: iterate it for the results, the transcript
 * growing as the user speaks, then the final one. A result the page was
 * too slow to take gives way to the next, which has its text.
 */
export interface Recognition extends AsyncIterable<SpeechResult> {
  /** Settles once the recognition ended, rejecting with the {@link SpeechError} that ended it. */
  readonly closed: Promise<void>;
  /** Stops listening: the final result follows, then the iteration ends. Resolves once it ended. */
  stop(): Promise<void>;
  /** Stops at once, without a final result. Resolves once Go has. */
  cancel(): Promise<void>;
}

/** What Go streams. */
interface Part {
  type: "ready" | "result";
  text?: string;
  final?: boolean;
}

/** How a call ended: the code of what failed, if anything. */
interface Outcome {
  code?: string;
  message?: string;
}

const codes: readonly string[] = ["unsupported", "denied", "language", "busy", "failed"];

const toError = (outcome: Outcome | null | undefined): SpeechError | undefined => {
  if (!outcome?.code) return undefined;
  return codes.includes(outcome.code)
    ? new SpeechError(outcome.code as SpeechErrorCode, outcome.message ?? "speech recognition failed")
    : new SpeechError("failed", "Invalid speech outcome");
};

/** Reports whether the system has a speech recognizer, for instance to show a dictation button. */
export const isSupported = (): Promise<boolean> => call<boolean>("plugin:speech.Supported");

/**
 * Asks the user, the first time, to let the app recognize speech and use
 * the microphone, and rejects with a {@link SpeechError} when they or the
 * system don't. {@link listen} asks too. Windows has no prompt for desktop
 * apps and tells what its Settings refuse only once a recognition starts:
 * there it resolves, and {@link listen} rejects with "denied".
 */
export async function requestPermission(): Promise<void> {
  const err = toError(await call<Outcome | null>("plugin:speech.RequestPermission"));
  if (err) throw err;
}

let lastID = 0;

/**
 * Asks for permission, then listens to the microphone, and resolves once
 * it listens. The recognition ends after {@link Recognition.stop}, a long
 * silence on Windows, the system's limit (about a minute for macOS's
 * server recognition), or when the page navigates.
 */
export async function listen(options: ListenOptions = {}): Promise<Recognition> {
  const locale = options.locale ?? "";
  if (typeof locale !== "string") throw new TypeError("locale must be a string");
  if (options.signal?.aborted) throw options.signal.reason;
  const id = String(++lastID);
  const parts = new Channel<Part>();
  const stream = parts[Symbol.asyncIterator]();
  // The first error wins: the signal's reason, or the call's.
  let terminal: { error: unknown } | undefined;
  let canceling = false;
  let finished = false;
  let iterating = false;
  const record = (error: unknown) => {
    terminal ??= { error };
  };
  const abort = () => {
    if (finished || canceling) return;
    record(options.signal!.reason);
    canceling = true;
    parts.close();
  };
  options.signal?.addEventListener("abort", abort, { once: true });
  const closed = call<Outcome | null>("plugin:speech.Listen", { id, locale, onDevice: options.onDevice ?? false }, parts)
    .then(
      (outcome) => {
        const err = toError(outcome);
        if (err) record(err);
        if (terminal) throw terminal.error;
      },
      (err: unknown) => {
        record(err);
        throw terminal!.error;
      },
    )
    .finally(() => {
      finished = true;
      options.signal?.removeEventListener("abort", abort);
    });
  void closed.catch(() => {});
  const cancel = async () => {
    if (!canceling && !finished) {
      canceling = true;
      parts.close();
    }
    await closed.catch(() => {});
  };
  const stop = async () => {
    if (!canceling && !finished) await call("plugin:speech.Stop", id);
    await closed.catch(() => {});
  };
  try {
    const first = await stream.next();
    if (first.done) {
      await closed;
      throw new Error("Recognition ended before it listened");
    }
    if (first.value.type !== "ready") {
      parts.close();
      await closed;
      throw new Error("Invalid speech ready response");
    }
    if (terminal) {
      await closed;
      throw terminal.error;
    }
  } catch (err) {
    parts.close();
    options.signal?.removeEventListener("abort", abort);
    throw err;
  }
  return {
    closed,
    stop,
    cancel,
    [Symbol.asyncIterator]() {
      if (iterating) throw new Error("A recognition supports one iterator");
      iterating = true;
      return (async function* () {
        let done = false;
        try {
          while (!canceling) {
            const next = await stream.next();
            if (next.done) break;
            if (next.value.type === "result" && !canceling) {
              yield { text: next.value.text ?? "", final: next.value.final ?? false };
            }
          }
          await closed;
          done = true;
        } finally {
          // Breaking out of the loop cancels.
          if (!done) await cancel();
          iterating = false;
        }
      })();
    },
  };
}
