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

    let mapVector = Data(hex: "435244540109000e01016105616c69636501000101783c3edf37")
    let mapReader = try LWWMap(replicaID: "swift-map-vector-reader")
    try mapReader.applyFrame(mapVector)
    try require(try mapReader.get("a") == Data("x".utf8), "Go LWW-Map vector projected the wrong value")
    try require(try mapReader.state() == mapVector, "Go LWW-Map state did not re-encode canonically")

    let mapAlice = try LWWMap(replicaID: "swift-map-alice")
    let mapBob = try LWWMap(replicaID: "swift-map-bob")
    let mapCarol = try LWWMap(replicaID: "swift-map-carol")
    let mapInitial = try mapAlice.set("title", value: Data("draft".utf8))
    try mapBob.applyFrame(mapInitial)
    try mapCarol.applyFrame(mapInitial)
    let mapBobEdit = try mapBob.set("owner", value: Data("bob".utf8))
    let mapCarolEdit = try mapCarol.set("title", value: Data("reviewed".utf8))
    let mapDelete = try mapAlice.delete("obsolete")
    for frame in [mapCarolEdit, mapBobEdit, mapDelete, mapBobEdit, mapInitial] {
        try mapAlice.applyFrame(frame)
    }
    for frame in [mapDelete, mapCarolEdit, mapInitial, mapDelete] {
        try mapBob.applyFrame(frame)
    }
    for frame in [mapBobEdit, mapDelete, mapBobEdit, mapInitial] {
        try mapCarol.applyFrame(frame)
    }
    try require(try mapAlice.get("title") == Data("reviewed".utf8), "LWW map did not retain highest write")
    try require(try mapAlice.keys() == ["owner", "title"], "LWW map keys were not canonical")
    try require(try mapAlice.state() == mapBob.state() && mapAlice.state() == mapCarol.state(), "LWW map replicas did not converge")
    let recoveredMap = try LWWMap(clockState: try mapAlice.clockState())
    try recoveredMap.applyFrame(mapAlice.state())
    let mapRecoveredEdit = try recoveredMap.set("after-recovery", value: Data("safe".utf8))
    try mapAlice.applyFrame(mapRecoveredEdit)
    try require(try mapAlice.state() == recoveredMap.state(), "LWW map same-ID recovery produced a conflicting mutation")
    let limitedMap = try LWWMap(replicaID: "swift-map-limited", limits: LWWMapLimits(maxEntries: 1))
    _ = try limitedMap.set("first", value: Data("safe".utf8))
    let limitedBefore = try limitedMap.state()
    var limitRejected = false
    do {
        _ = try limitedMap.set("second", value: Data("rejected".utf8))
    } catch let error as CRDTError {
        limitRejected = error.code == CRDTError.resourceLimit
    }
    let limitStateUnchanged = try limitedMap.state() == limitedBefore
    try require(limitRejected && limitStateUnchanged, "LWW map limit rejection was not atomic")
    print("PASS: Go vectors, three-replica convergence, and snapshot recovery for RGA and LWW-Map")
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
