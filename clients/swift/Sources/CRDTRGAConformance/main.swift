import CRDTRGA
import Foundation

do {
    let vector = Data(hex: "435244540114001201010205616c696365000100410101b20700c1d69811")
    let vectorReader = try RGA(replicaID: "swift-vector-reader")
    try vectorReader.applyFrame(vector)
    try require(vectorReader.text() == "Aβ", "Go canonical vector projected the wrong text")

    let alice = try RGA(replicaID: "swift-alice")
    let bob = try RGA(replicaID: "swift-bob")
    let carol = try RGA(replicaID: "swift-carol")
    let initial = try alice.insert(at: 0, value: "A")
    try bob.applyFrame(initial)
    try carol.applyFrame(initial)
    let bobEdit = try bob.insert(at: 1, value: "B")
    let carolEdit = try carol.insert(at: 1, value: "C")
    for frame in [carolEdit, bobEdit, bobEdit] {
        try alice.applyFrame(frame)
    }
    try bob.applyFrame(carolEdit)
    try carol.applyFrame(bobEdit)
    try require(alice.text() == bob.text() && alice.text() == carol.text(), "duplicate/reordered replicas did not converge")

    let recovered = try RGA(clockState: try alice.clockState())
    try recovered.applyFrame(alice.state())
    try require(recovered.text() == alice.text(), "state recovery did not converge")
    let recoveredEdit = try recovered.insert(at: recovered.text().count, value: "D")
    try alice.applyFrame(recoveredEdit)
    try require(recovered.text() == alice.text(), "same-ID clock recovery produced a conflicting mutation")
    print("PASS: Go vector, three-replica convergence, and snapshot recovery")
} catch {
    fputs("FAIL: \(error)\n", stderr)
    exit(1)
}

private func require(_ value: @autoclosure () throws -> Bool, _ message: String) throws {
    guard try value() else { throw ConformanceError(message) }
}

private struct ConformanceError: Error, CustomStringConvertible {
    let message: String
    init(_ message: String) { self.message = message }
    var description: String { message }
}

private extension Data {
    init(hex: String) {
        self.init()
        var index = hex.startIndex
        while index < hex.endIndex {
            let next = hex.index(index, offsetBy: 2)
            append(UInt8(hex[index..<next], radix: 16)!)
            index = next
        }
    }
}
