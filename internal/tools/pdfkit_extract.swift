import Foundation
import PDFKit

struct PageResult: Codable {
    let pageNumber: Int
    let text: String
    let charCount: Int
    let lineCount: Int
}

struct DocumentResult: Codable {
    let sourcePath: String
    let pageCount: Int
    let encrypted: Bool
    let locked: Bool
    let unlockAttempted: Bool
    let unlockedWithEmptyPassword: Bool
    let pages: [PageResult]
}

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(1)
}

guard CommandLine.arguments.count >= 2 else {
    fail("missing pdf path")
}

let path = CommandLine.arguments[1]
let url = URL(fileURLWithPath: path)
guard let doc = PDFDocument(url: url) else {
    fail("open failed")
}

let encrypted = doc.isEncrypted
var unlockAttempted = false
var unlockedWithEmptyPassword = false
if doc.isLocked {
    unlockAttempted = true
    unlockedWithEmptyPassword = doc.unlock(withPassword: "")
}

var pages: [PageResult] = []
if !doc.isLocked {
    for index in 0..<doc.pageCount {
        guard let page = doc.page(at: index) else { continue }
        let raw = page.string ?? ""
        let text = raw.replacingOccurrences(of: "\r\n", with: "\n").replacingOccurrences(of: "\r", with: "\n")
        pages.append(PageResult(
            pageNumber: index + 1,
            text: text,
            charCount: text.count,
            lineCount: text.split(separator: "\n", omittingEmptySubsequences: false).count
        ))
    }
}

let result = DocumentResult(
    sourcePath: path,
    pageCount: doc.pageCount,
    encrypted: encrypted,
    locked: doc.isLocked,
    unlockAttempted: unlockAttempted,
    unlockedWithEmptyPassword: unlockedWithEmptyPassword,
    pages: pages
)

let encoder = JSONEncoder()
guard let data = try? encoder.encode(result) else {
    fail("encode failed")
}
FileHandle.standardOutput.write(data)
