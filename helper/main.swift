// supersearch-helper: Apple Vision OCR + PDFKit text for the Go server.
//
//   supersearch-helper ocr <image> [langs]          → recognized text
//   supersearch-helper pdftext <pdf>                → text layer, pages ended by \f
//   supersearch-helper transcribe <audio> [lang]    → speech-to-text, on device
//   supersearch-helper serve                        → JSON lines on stdin/stdout:
//     {"op":"ocr","path":…,"langs":…}         → {"answer":text} or {"error":…}  (Vision, model stays loaded)
//     {"op":"ask","instructions":…,"prompt":…} → {"answer":…} or {"error":…}   (on-device LLM)
//
// langs: comma-separated BCP-47 codes (en-US,hi-IN); empty = en-US; "auto" = auto-detect.
import AVFoundation
import CoreGraphics
import Foundation
import FoundationModels
import ImageIO
import PDFKit
import Speech
import Vision

func fail(_ msg: String) -> Never {
    FileHandle.standardError.write((msg + "\n").data(using: .utf8)!)
    exit(1)
}

func recognize(_ image: CGImage, _ langs: [String]) -> String {
    let req = VNRecognizeTextRequest()
    req.recognitionLevel = .accurate
    req.usesLanguageCorrection = true
    // Auto-detect reads latin look-alikes in slides as Cyrillic ("BT" → "ВТ"),
    // so it is only used when asked for with "auto".
    req.automaticallyDetectsLanguage = langs == ["auto"]
    if langs != ["auto"] {
        req.recognitionLanguages = langs.isEmpty ? ["en-US"] : langs
    }
    do {
        try VNImageRequestHandler(cgImage: image).perform([req])
    } catch {
        fail("vision: \(error)")
    }
    // Vision returns boxes, not reading order. Rebuild lines: top to bottom,
    // boxes whose vertical centers are within half a box height share a line,
    // left to right within it. Keeps table rows ("P1 0 5 5 5 0") together.
    let boxes = (req.results ?? []).compactMap { o in o.topCandidates(1).first.map { (o.boundingBox, $0.string) } }
        .sorted { $0.0.midY > $1.0.midY }
    var lines: [[(CGRect, String)]] = []
    for b in boxes {
        if let last = lines.last?.last, abs(last.0.midY - b.0.midY) < min(last.0.height, b.0.height) / 2 {
            lines[lines.count - 1].append(b)
        } else {
            lines.append([b])
        }
    }
    let text = lines.map { $0.sorted { $0.0.minX < $1.0.minX }.map { $0.1 }.joined(separator: " ") }.joined(separator: "\n")
    // Even with en-US, Vision emits Cyrillic look-alikes ("ВТ" for "BT"),
    // which then never match a typed query. Fold them back unless a Cyrillic
    // language was asked for.
    if langs.contains(where: { $0.hasPrefix("ru") || $0.hasPrefix("uk") || $0 == "auto" }) { return text }
    return String(text.map { homoglyphs[$0] ?? $0 })
}

let homoglyphs: [Character: Character] = [
    "А": "A", "В": "B", "С": "C", "Е": "E", "Н": "H", "І": "I", "Ј": "J", "К": "K", "М": "M",
    "О": "O", "Р": "P", "Ѕ": "S", "Т": "T", "Х": "X", "У": "Y", "З": "3",
    "а": "a", "в": "b", "с": "c", "е": "e", "і": "i", "ј": "j", "к": "k", "м": "m", "н": "h",
    "о": "o", "р": "p", "ѕ": "s", "т": "t", "у": "y", "х": "x",
]

// Flattens an image onto white. Vision reads transparent pixels as black, so
// dark text on a transparent PNG (screenshots, exported diagrams) came back empty.
func onWhite(_ image: CGImage) -> CGImage {
    guard image.alphaInfo != .none, image.alphaInfo != .noneSkipLast, image.alphaInfo != .noneSkipFirst,
          let ctx = CGContext(data: nil, width: image.width, height: image.height, bitsPerComponent: 8, bytesPerRow: 0,
                              space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)
    else { return image }
    let rect = CGRect(x: 0, y: 0, width: image.width, height: image.height)
    ctx.setFillColor(CGColor(red: 1, green: 1, blue: 1, alpha: 1))
    ctx.fill(rect)
    ctx.draw(image, in: rect)
    return ctx.makeImage() ?? image
}

struct Request: Decodable {
    let op: String
    var instructions: String?
    var prompt: String?
    var path: String?
    var langs: String?
}
struct AskReply: Encodable { var answer: String?; var error: String? }

func reply<T: Encodable>(_ v: T) {
    FileHandle.standardOutput.write(try! JSONEncoder().encode(v) + Data("\n".utf8))
}

// serve keeps the language model loaded between questions.
func serve() async {
    while let line = readLine() {
        guard let req = try? JSONDecoder().decode(Request.self, from: Data(line.utf8)) else {
            reply(AskReply(error: "bad request"))
            continue
        }
        switch req.op {
        case "ocr":
            let text = autoreleasepool { ocrImage(URL(fileURLWithPath: req.path ?? ""), splitLangs(req.langs)) }
            reply(text.map { AskReply(answer: $0) } ?? AskReply(error: "cannot decode image"))
        case "ask":
            guard case .available = SystemLanguageModel.default.availability else {
                reply(AskReply(error: "Apple Intelligence is not available: \(SystemLanguageModel.default.availability)"))
                continue
            }
            do {
                let session = LanguageModelSession(instructions: req.instructions ?? "")
                reply(AskReply(answer: try await session.respond(to: req.prompt ?? "").content))
            } catch {
                reply(AskReply(error: "\(error)"))
            }
        default:
            reply(AskReply(error: "unknown op \(req.op)"))
        }
    }
}

// Exit if the server that started us is gone (it was killed, or Obsidian
// quit): otherwise a long OCR or transcription keeps running as an orphan.
Thread.detachNewThread {
    while true {
        if getppid() == 1 { exit(1) }
        Thread.sleep(forTimeInterval: 2)
    }
}

let args = CommandLine.arguments
if args.count == 2 && args[1] == "serve" {
    await serve()
    exit(0)
}
guard args.count >= 3 else { fail("usage: supersearch-helper ocr|pdftext <file> … | serve") }
let url = URL(fileURLWithPath: args[2])
func splitLangs(_ s: String?) -> [String] {
    (s ?? "").split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
}
func langs(_ i: Int) -> [String] { splitLangs(args.count > i ? args[i] : nil) }

func ocrImage(_ url: URL, _ langs: [String]) -> String? {
    guard let src = CGImageSourceCreateWithURL(url as CFURL, nil),
          let image = CGImageSourceCreateImageAtIndex(src, 0, nil)
    else { return nil }
    return recognize(onWhite(image), langs)
}
var out = ""

switch args[1] {
case "ocr":
    guard let text = ocrImage(url, langs(3)) else { fail("cannot decode image") }
    out = text

case "transcribe":
    let lang = langs(3).first.flatMap { $0 == "auto" ? nil : $0 } ?? "en-US"
    let transcriber = SpeechTranscriber(locale: Locale(identifier: lang), preset: .transcription)
    do {
        if let install = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
            try await install.downloadAndInstall() // one-time language model fetch, done by the OS
        }
        let analyzer = SpeechAnalyzer(modules: [transcriber])
        let file = try AVAudioFile(forReading: url)
        async let text = transcriber.results.reduce("") { $0 + String($1.text.characters) + " " }
        if let last = try await analyzer.analyzeSequence(from: file) {
            try await analyzer.finalizeAndFinish(through: last)
        } else {
            await analyzer.cancelAndFinishNow()
        }
        out = try await text
    } catch {
        fail("transcribe: \(error)")
    }

case "pdftext":
    guard let doc = PDFDocument(url: url) else { fail("cannot open pdf") }
    if doc.isLocked { fail("pdf is encrypted") }
    for i in 0..<doc.pageCount {
        out += (doc.page(at: i)?.string ?? "") + "\u{0C}"
    }

default:
    fail("unknown command \(args[1])")
}
FileHandle.standardOutput.write(out.data(using: .utf8)!)
