import AppKit
import Foundation
import PDFKit
import Vision

struct OCRResult: Codable {
    let pageNumber: Int
    let text: String
    let confidence: Double
}

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(1)
}

guard CommandLine.arguments.count >= 3 else {
    fail("usage: vision_ocr.swift <pdf-path> <page-number>")
}

let path = CommandLine.arguments[1]
guard let pageNumber = Int(CommandLine.arguments[2]), pageNumber > 0 else {
    fail("invalid page number")
}
guard let document = PDFDocument(url: URL(fileURLWithPath: path)) else {
    fail("open failed")
}
if document.isLocked && !document.unlock(withPassword: "") {
    fail("document locked")
}
guard let page = document.page(at: pageNumber - 1) else {
    fail("page not found")
}

let bounds = page.bounds(for: .mediaBox)
let size = NSSize(width: max(bounds.width * 2, 1200), height: max(bounds.height * 2, 1200))
let image = page.thumbnail(of: size, for: .mediaBox)
guard let cgImage = image.cgImage(forProposedRect: nil, context: nil, hints: nil) else {
    fail("render failed")
}

let request = VNRecognizeTextRequest()
request.recognitionLevel = .accurate
request.usesLanguageCorrection = true

let handler = VNImageRequestHandler(cgImage: cgImage, options: [:])
do {
    try handler.perform([request])
} catch {
    fail("vision request failed: \(error.localizedDescription)")
}

let observations = request.results ?? []
var lines: [String] = []
var confidenceTotal = 0.0
var confidenceCount = 0
for observation in observations {
    guard let candidate = observation.topCandidates(1).first else { continue }
    lines.append(candidate.string)
    confidenceTotal += Double(candidate.confidence)
    confidenceCount += 1
}

let result = OCRResult(
    pageNumber: pageNumber,
    text: lines.joined(separator: "\n"),
    confidence: confidenceCount > 0 ? confidenceTotal / Double(confidenceCount) : 0
)
let encoder = JSONEncoder()
guard let data = try? encoder.encode(result) else {
    fail("encode failed")
}
FileHandle.standardOutput.write(data)
