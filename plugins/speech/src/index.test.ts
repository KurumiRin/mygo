import { afterEach, expect, test } from "bun:test";
import { fakeGo } from "../../fake-go";
import { isSupported, listen, requestPermission, SpeechError, type SpeechResult } from "./index";

let cleanup = () => {};
afterEach(() => cleanup());

const aborted = (signal: AbortSignal) =>
  new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));

test("streams the results, and stop gives the final one", async () => {
  let stop = () => {};
  const go = fakeGo({
    "plugin:speech.Listen": async ({ send, args }) => {
      expect(args[0]).toEqual({ id: expect.any(String), locale: "en-US", onDevice: false });
      send(1, { type: "ready" });
      send(1, { type: "result", text: "hello" });
      send(1, { type: "result", text: "hello world" });
      await new Promise<void>((resolve) => (stop = resolve));
      send(1, { type: "result", text: "hello world.", final: true });
      return null;
    },
    "plugin:speech.Stop": ({ args }) => {
      expect(typeof args[0]).toBe("string");
      stop();
    },
  });
  cleanup = go.uninstall;
  const r = await listen({ locale: "en-US" });
  const seen: SpeechResult[] = [];
  for await (const result of r) {
    seen.push(result);
    if (seen.length === 2) void r.stop();
  }
  expect(seen).toEqual([
    { text: "hello", final: false },
    { text: "hello world", final: false },
    { text: "hello world.", final: true },
  ]);
  await expect(r.closed).resolves.toBeUndefined();
  await r.stop();
  expect(go.calls.filter(([m]) => m === "plugin:speech.Stop")).toHaveLength(1);
});

test("cancel and breaking out of the loop close the channel", async () => {
  let cancels = 0;
  const go = fakeGo({
    "plugin:speech.Listen": async ({ send, signal }) => {
      send(1, { type: "ready" });
      send(1, { type: "result", text: "a" });
      await aborted(signal);
      cancels++;
      return null;
    },
  });
  cleanup = go.uninstall;
  const r = await listen();
  for await (const result of r) {
    expect(result.text).toBe("a");
    break;
  }
  await r.closed;
  const r2 = await listen();
  await r2.cancel();
  await r2.cancel();
  expect(cancels).toBe(2);
});

test("failures are SpeechErrors with their code", async () => {
  const go = fakeGo({
    "plugin:speech.Listen": ({ send, args }) => {
      if ((args[0] as { locale: string }).locale === "xx") {
        return { code: "language", message: "speech: the language is not supported" };
      }
      send(1, { type: "ready" });
      send(1, { type: "result", text: "partial" });
      return { code: "failed", message: "speech: the network failed" };
    },
    "plugin:speech.RequestPermission": () => ({ code: "denied", message: "speech: not allowed" }),
    "plugin:speech.Supported": () => true,
  });
  cleanup = go.uninstall;
  const err = await listen({ locale: "xx" }).catch((e) => e);
  expect(err).toBeInstanceOf(SpeechError);
  expect(err.code).toBe("language");
  const r = await listen();
  const seen: SpeechResult[] = [];
  const iterate = (async () => {
    for await (const result of r) seen.push(result);
  })();
  await expect(iterate).rejects.toMatchObject({ code: "failed" });
  expect(seen).toEqual([{ text: "partial", final: false }]);
  await expect(requestPermission()).rejects.toMatchObject({ code: "denied" });
  expect(await isSupported()).toBe(true);
});

test("an aborted signal never calls Go, and aborting cancels", async () => {
  const go = fakeGo({
    "plugin:speech.Listen": async ({ send, signal }) => {
      send(1, { type: "ready" });
      await aborted(signal);
      return null;
    },
  });
  cleanup = go.uninstall;
  await expect(listen({ signal: AbortSignal.abort("no") })).rejects.toBe("no");
  expect(go.calls).toHaveLength(0);
  const controller = new AbortController();
  const r = await listen({ signal: controller.signal });
  controller.abort("done");
  await expect(r.closed).rejects.toBe("done");
});
