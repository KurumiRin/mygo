// The speech plugin's shim of SpeechAnalyzer (macOS 26), a Swift API that
// Objective-C's runtime doesn't reach: C functions that the plugin calls
// through purego, and a C callback that tells it what happens. The shim
// listens to the microphone with AVAudioEngine, converts its audio to the
// analyzer's format, and transcribes it with a SpeechTranscriber.
//
// Built by `go generate ./plugins/speech/internal/analyzer` with Xcode 26
// for macOS 12, so that it loads there too and reports itself unavailable.

import AVFoundation
import Foundation
import Speech

/// The callback: the recognition's id, the event and its text, which is
/// valid during the call.
public typealias EventCallback = @convention(c) (UInt64, Int32, UnsafePointer<CChar>?) -> Void

// Events; the Go side has the same.
let eventReady: Int32 = 0  // the microphone listens
let eventText: Int32 = 1  // the transcript so far
let eventFinal: Int32 = 2  // the last transcript, after stop: the end
let eventFailed: Int32 = 3  // the end, with what failed
let eventCanceled: Int32 = 4  // the end, after cancel
let eventUnavailable: Int32 = 5  // the end, before ready: no transcriber for this system
let eventLanguage: Int32 = 6  // the end, before ready: the language is not supported

/// Reports whether SpeechTranscriber can run here: 1 or 0.
@_cdecl("mygo_speech_available")
public func mygoSpeechAvailable() -> Int32 {
    if #available(macOS 26.0, *) {
        return SpeechTranscriber.isAvailable ? 1 : 0
    }
    return 0
}

/// Starts recognition id in the language of the BCP 47 tag locale (the
/// user's when empty), then calls callback with its events, ending with
/// one of the ends.
@_cdecl("mygo_speech_start")
public func mygoSpeechStart(_ id: UInt64, _ locale: UnsafePointer<CChar>?, _ callback: EventCallback) {
    let tag = locale.map { String(cString: $0) } ?? ""
    guard #available(macOS 26.0, *) else {
        callback(id, eventUnavailable, nil)
        return
    }
    let session = Session(id: id, callback: callback)
    sessions.withLock { $0[id] = session }
    Task { await session.run(tag: tag) }
}

/// Stops listening: the transcriber finalizes, then the recognition ends
/// with eventFinal.
@_cdecl("mygo_speech_stop")
public func mygoSpeechStop(_ id: UInt64) {
    if #available(macOS 26.0, *) {
        (sessions.withLock { $0[id] } as? Session)?.stop()
    }
}

/// Cancels: the recognition ends at once with eventCanceled.
@_cdecl("mygo_speech_cancel")
public func mygoSpeechCancel(_ id: UInt64) {
    if #available(macOS 26.0, *) {
        (sessions.withLock { $0[id] } as? Session)?.cancel()
    }
}

extension NSLock {
    // NSLock's own withLock needs macOS 13.
    func locked<R>(_ body: () throws -> R) rethrows -> R {
        lock()
        defer { unlock() }
        return try body()
    }
}

final class Locked<Value>: @unchecked Sendable {
    private let lock = NSLock()
    private var value: Value
    init(_ value: Value) { self.value = value }
    func withLock<R>(_ body: (inout Value) -> R) -> R {
        lock.lock()
        defer { lock.unlock() }
        return body(&value)
    }
}

/// The running recognitions, by id: AnyObject, for systems before 26.
let sessions = Locked<[UInt64: AnyObject]>([:])

/// Separates phrases with a space, unless one side already has one.
func join(_ a: String, _ b: String) -> String {
    if a.isEmpty { return b }
    if b.isEmpty { return a }
    if a.last!.isWhitespace || b.first!.isWhitespace { return a + b }
    return a + " " + b
}

@available(macOS 26.0, *)
final class Session: @unchecked Sendable {
    let id: UInt64
    let callback: EventCallback

    // Guarded by lock.
    private let lock = NSLock()
    private var engine: AVAudioEngine?
    private var input: AsyncStream<AnalyzerInput>.Continuation?
    private var analyzer: SpeechAnalyzer?
    private var finalized = ""
    private var volatile = ""
    private var stopping = false
    private var canceled = false
    private var ended = false

    init(id: UInt64, callback: @escaping EventCallback) {
        self.id = id
        self.callback = callback
    }

    private func emit(_ event: Int32, _ text: String?) {
        if let text {
            text.withCString { callback(id, event, $0) }
        } else {
            callback(id, event, nil)
        }
    }

    private var transcript: String { join(finalized, volatile) }

    /// end ends the recognition once: the microphone stops, and the last
    /// event goes to Go.
    private func end(_ event: Int32, _ text: String?) {
        let first = lock.locked {
            let first = !ended
            ended = true
            return first
        }
        guard first else { return }
        stopAudio()
        sessions.withLock { $0[id] = nil }
        emit(event, text)
    }

    private func stopAudio() {
        let (engine, input) = lock.locked {
            let pair = (self.engine, self.input)
            self.engine = nil
            self.input = nil
            return pair
        }
        if let engine {
            engine.stop()
            engine.inputNode.removeTap(onBus: 0)
        }
        input?.finish()
    }

    func run(tag: String) async {
        do {
            try await listen(tag: tag)
        } catch {
            let canceled = lock.locked { self.canceled }
            end(canceled ? eventCanceled : eventFailed, canceled ? nil : "speech: \(error.localizedDescription)")
        }
    }

    private func listen(tag: String) async throws {
        let wanted = tag.isEmpty ? Locale.current : Locale(identifier: tag)
        guard let locale = await SpeechTranscriber.supportedLocale(equivalentTo: wanted) else {
            end(eventLanguage, nil)
            return
        }
        let transcriber = SpeechTranscriber(locale: locale, preset: .progressiveTranscription)
        // The model of the language, once per system: the first time takes
        // a download.
        if let request = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
            try await request.downloadAndInstall()
        }
        guard let format = await SpeechAnalyzer.bestAvailableAudioFormat(compatibleWith: [transcriber]) else {
            end(eventUnavailable, nil)
            return
        }
        if lock.locked({ canceled }) {
            end(eventCanceled, nil)
            return
        }

        let engine = AVAudioEngine()
        let node = engine.inputNode
        let micFormat = node.outputFormat(forBus: 0)
        guard micFormat.sampleRate > 0, let converter = AVAudioConverter(from: micFormat, to: format) else {
            end(eventFailed, "speech: there is no microphone")
            return
        }
        let (stream, builder) = AsyncStream.makeStream(of: AnalyzerInput.self)
        node.installTap(onBus: 0, bufferSize: 4096, format: micFormat) { buffer, _ in
            if let converted = convert(buffer, with: converter, to: format) {
                builder.yield(AnalyzerInput(buffer: converted))
            }
        }
        let analyzer = SpeechAnalyzer(modules: [transcriber])
        lock.locked {
            self.engine = engine
            self.input = builder
            self.analyzer = analyzer
        }
        engine.prepare()
        do {
            try engine.start()
        } catch {
            end(eventFailed, "speech: the microphone did not start: \(error.localizedDescription)")
            return
        }
        try await analyzer.start(inputSequence: stream)
        if lock.locked({ canceled }) {
            await analyzer.cancelAndFinishNow()
            end(eventCanceled, nil)
            return
        }
        emit(eventReady, nil)

        // The results end once the analyzer finished: after stop, cancel
        // or a failure.
        for try await result in transcriber.results {
            let text = String(result.text.characters)
            let all = lock.locked {
                if result.isFinal {
                    finalized = join(finalized, text)
                    volatile = ""
                } else {
                    volatile = text
                }
                return transcript
            }
            if !lock.locked({ ended || canceled }) {
                emit(eventText, all)
            }
        }
        let (canceled, all) = lock.locked { (self.canceled, join(finalized, volatile)) }
        end(canceled ? eventCanceled : eventFinal, canceled ? nil : all)
    }

    func stop() {
        let analyzer = lock.locked {
            if stopping || ended { return nil as SpeechAnalyzer? }
            stopping = true
            return self.analyzer
        }
        guard let analyzer else { return }
        stopAudio()
        Task {
            do {
                try await analyzer.finalizeAndFinishThroughEndOfInput()
            } catch {
                self.end(eventFailed, "speech: \(error.localizedDescription)")
            }
        }
    }

    func cancel() {
        let analyzer = lock.locked {
            canceled = true
            return self.analyzer
        }
        stopAudio()
        end(eventCanceled, nil)
        if let analyzer {
            Task { await analyzer.cancelAndFinishNow() }
        }
    }
}

/// convert converts a buffer of the microphone to the analyzer's format.
func convert(_ buffer: AVAudioPCMBuffer, with converter: AVAudioConverter, to format: AVAudioFormat) -> AVAudioPCMBuffer? {
    let ratio = format.sampleRate / buffer.format.sampleRate
    let capacity = AVAudioFrameCount((Double(buffer.frameLength) * ratio).rounded(.up)) + 1
    guard let out = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: capacity) else { return nil }
    var consumed = false
    var error: NSError?
    let status = converter.convert(to: out, error: &error) { _, inputStatus in
        if consumed {
            inputStatus.pointee = .noDataNow
            return nil
        }
        consumed = true
        inputStatus.pointee = .haveData
        return buffer
    }
    if status == .error || out.frameLength == 0 { return nil }
    return out
}
