import CRDTRGAFFI
import Foundation

/// Errors returned by the bounded Rust runtime's stable C ABI.
public struct CRDTError: Error, Equatable, Sendable {
    /// Stable status code from `crdt_rga.h`.
    public let code: Int32

    /// The runtime rejected a frame or mutation before exceeding a negotiated bound.
    public static let resourceLimit = Int32(3)

    init(_ code: Int32) {
        self.code = code
    }
}

/// Persistable HLC state. Store it atomically with an `RGA.state()` frame.
public struct RGAClockState: Equatable, Sendable {
    /// Logical replica ID used for future local tags.
    public let replicaID: String
    /// Last observed/emitted HLC wall time in milliseconds.
    public let wallTime: UInt64
    /// Last observed/emitted HLC logical counter.
    public let logical: UInt64

    public init(replicaID: String, wallTime: UInt64, logical: UInt64) {
        self.replicaID = replicaID
        self.wallTime = wallTime
        self.logical = logical
    }
}

/// Authenticated receiver bounds for an `LWWMap` replica.
public struct LWWMapLimits: Equatable, Sendable {
    public let maxFrameBytes: Int
    public let maxPayloadBytes: Int
    public let maxStringBytes: Int
    public let maxEntries: Int
    public let maxTombstones: Int

    public init(
        maxFrameBytes: Int = 1 << 20,
        maxPayloadBytes: Int = 1 << 20,
        maxStringBytes: Int = 64 << 10,
        maxEntries: Int = 100_000,
        maxTombstones: Int = 100_000
    ) {
        self.maxFrameBytes = maxFrameBytes
        self.maxPayloadBytes = maxPayloadBytes
        self.maxStringBytes = maxStringBytes
        self.maxEntries = maxEntries
        self.maxTombstones = maxTombstones
    }

    fileprivate func native() throws -> crdt_lww_map_limits {
        guard maxFrameBytes > 0,
              maxPayloadBytes > 0,
              maxStringBytes > 0,
              maxEntries > 0,
              maxTombstones > 0 else {
            throw CRDTError(Int32(CRDT_INVALID_ARGUMENT))
        }
        return crdt_lww_map_limits(
            max_frame_bytes: maxFrameBytes,
            max_payload_bytes: maxPayloadBytes,
            max_string_bytes: maxStringBytes,
            max_entries: maxEntries,
            max_tombstones: maxTombstones
        )
    }
}

/// A Swift-owned handle to the native Rust `rga-run-v2` implementation.
///
/// This is not an independent Swift protocol implementation. Authenticate and
/// negotiate the TypeID 19/20 group manifest before applying a frame. Persist
/// `state()` together with the caller's clock/frontier/outbox transaction
/// before reusing the same replica identifier.
public final class RGA: @unchecked Sendable {
    private var handle: OpaquePointer?
    private let lock = NSLock()

    /// Creates an empty document using the conservative native-client limits.
    public init(replicaID: String) throws {
        let replica = Array(replicaID.utf8)
        var created: OpaquePointer?
        let status = replica.withUnsafeBufferPointer { buffer in
            crdt_rga_new(buffer.baseAddress, buffer.count, &created)
        }
        guard status == CRDT_OK else {
            throw CRDTError(status)
        }
        handle = created
    }

    /// Restores a runtime clock. Apply the atomically paired complete state
    /// frame before creating any new local mutation.
    public convenience init(clockState: RGAClockState) throws {
        let replica = Array(clockState.replicaID.utf8)
        var created: OpaquePointer?
        let status = replica.withUnsafeBufferPointer { buffer in
            crdt_rga_new_from_clock(buffer.baseAddress, buffer.count, clockState.wallTime, clockState.logical, &created)
        }
        guard status == CRDT_OK else {
            throw CRDTError(status)
        }
        self.init(adopting: created!)
    }

    private init(adopting handle: OpaquePointer) {
        self.handle = handle
    }

    deinit {
        if let handle {
            crdt_rga_free(handle)
        }
    }

    /// Applies one authenticated, bounded state or delta frame atomically.
    public func applyFrame(_ frame: Data) throws {
        try lock.withLock {
            let status = frame.withUnsafeBytes { bytes in
                crdt_rga_apply(requireHandle(), bytes.bindMemory(to: UInt8.self).baseAddress, frame.count)
            }
            try check(status)
        }
    }

    /// Inserts text before a Unicode-scalar offset and returns its applied delta.
    public func insert(at offset: Int, value: String) throws -> Data {
        guard offset >= 0 else { throw CRDTError(Int32(CRDT_RANGE)) }
        let bytes = Array(value.utf8)
        return try lock.withLock {
            var output = crdt_buffer(data: nil, len: 0)
            let status = bytes.withUnsafeBufferPointer { input in
                crdt_rga_insert(requireHandle(), offset, input.baseAddress, input.count, &output)
            }
            return try take(&output, status: status)
        }
    }

    /// Tombstones a visible Unicode-scalar range and returns its applied delta.
    public func delete(at offset: Int, count: Int) throws -> Data {
        guard offset >= 0, count >= 0 else { throw CRDTError(Int32(CRDT_RANGE)) }
        return try lock.withLock {
            var output = crdt_buffer(data: nil, len: 0)
            let status = crdt_rga_delete(requireHandle(), offset, count, &output)
            return try take(&output, status: status)
        }
    }

    /// Returns a complete canonical state frame or rejects unresolved parents.
    public func state() throws -> Data {
        try lock.withLock {
            var output = crdt_buffer(data: nil, len: 0)
            return try take(&output, status: crdt_rga_state(requireHandle(), &output))
        }
    }

    /// Returns the HLC state to persist atomically with `state()`.
    public func clockState() throws -> RGAClockState {
        try lock.withLock {
            var output = crdt_clock_state(replica_id: crdt_buffer(data: nil, len: 0), wall_time: 0, logical: 0)
            try check(crdt_rga_clock_state(requireHandle(), &output))
            defer { crdt_clock_state_free(output) }
            guard let replica = output.replica_id.data,
                  let replicaID = String(data: Data(bytes: replica, count: output.replica_id.len), encoding: .utf8) else {
                throw CRDTError(Int32(CRDT_INTERNAL))
            }
            return RGAClockState(replicaID: replicaID, wallTime: output.wall_time, logical: output.logical)
        }
    }

    /// Returns the deterministic visible text projection.
    public func text() throws -> String {
        try lock.withLock {
            var output = crdt_buffer(data: nil, len: 0)
            let value = try take(&output, status: crdt_rga_text(requireHandle(), &output))
            guard let result = String(data: value, encoding: .utf8) else {
                throw CRDTError(Int32(CRDT_INTERNAL))
            }
            return result
        }
    }

    private func requireHandle() -> OpaquePointer {
        guard let handle else {
            fatalError("RGA used after deinitialization")
        }
        return handle
    }

    private func take(_ output: inout crdt_buffer, status: Int32) throws -> Data {
        try check(status)
        defer { crdt_buffer_free(output) }
        guard let data = output.data else {
            guard output.len == 0 else { throw CRDTError(Int32(CRDT_INTERNAL)) }
            return Data()
        }
        return Data(bytes: data, count: output.len)
    }

    private func check(_ status: Int32) throws {
        guard status == CRDT_OK else { throw CRDTError(status) }
    }
}

/// A Swift-owned handle to the native Rust `lww-map-v1` implementation.
///
/// The map exchanges Go-compatible TypeID `9/10` frames. Values remain opaque
/// bytes, so the host must bind the value schema, document, epoch, limits, and
/// authorization in an authenticated manifest before `applyFrame(_:)`.
public final class LWWMap: @unchecked Sendable {
    private var handle: OpaquePointer?
    private let lock = NSLock()

    /// Creates an empty map using the conservative native-client limits.
    public convenience init(replicaID: String) throws {
        try self.init(replicaID: replicaID, limits: LWWMapLimits())
    }

    /// Creates an empty map with manifest-negotiated receiver limits.
    public init(replicaID: String, limits: LWWMapLimits) throws {
        let replica = Array(replicaID.utf8)
        var created: OpaquePointer?
        var nativeLimits = try limits.native()
        let status = replica.withUnsafeBufferPointer { buffer in
            crdt_lww_map_new_with_limits(buffer.baseAddress, buffer.count, &nativeLimits, &created)
        }
        guard status == CRDT_OK, let created else {
            throw CRDTError(status == CRDT_OK ? Int32(CRDT_INTERNAL) : status)
        }
        handle = created
    }

    /// Restores a runtime clock. Apply the atomically paired complete state
    /// frame before creating any new local mutation.
    public convenience init(clockState: RGAClockState) throws {
        try self.init(clockState: clockState, limits: LWWMapLimits())
    }

    /// Restores a runtime clock using the same manifest-negotiated limits.
    public convenience init(clockState: RGAClockState, limits: LWWMapLimits) throws {
        let replica = Array(clockState.replicaID.utf8)
        var created: OpaquePointer?
        var nativeLimits = try limits.native()
        let status = replica.withUnsafeBufferPointer { buffer in
            crdt_lww_map_new_from_clock_with_limits(
                buffer.baseAddress,
                buffer.count,
                clockState.wallTime,
                clockState.logical,
                &nativeLimits,
                &created
            )
        }
        guard status == CRDT_OK, let created else {
            throw CRDTError(status == CRDT_OK ? Int32(CRDT_INTERNAL) : status)
        }
        self.init(adopting: created)
    }

    private init(adopting handle: OpaquePointer) {
        self.handle = handle
    }

    deinit {
        if let handle {
            crdt_lww_map_free(handle)
        }
    }

    /// Applies one authenticated LWW-Map TypeID 9 or 10 frame atomically.
    public func applyFrame(_ frame: Data) throws {
        try lock.withLock {
            let status = frame.withUnsafeBytes { bytes in
                crdt_lww_map_apply(requireHandle(), bytes.bindMemory(to: UInt8.self).baseAddress, frame.count)
            }
            try check(status)
        }
    }

    /// Sets an opaque value and returns its already-applied canonical delta.
    public func set(_ key: String, value: Data) throws -> Data {
        let keyBytes = Array(key.utf8)
        return try lock.withLock {
            var output = crdt_buffer(data: nil, len: 0)
            let status = keyBytes.withUnsafeBufferPointer { keyBuffer in
                value.withUnsafeBytes { valueBuffer in
                    crdt_lww_map_set(
                        requireHandle(),
                        keyBuffer.baseAddress,
                        keyBuffer.count,
                        valueBuffer.bindMemory(to: UInt8.self).baseAddress,
                        value.count,
                        &output
                    )
                }
            }
            return try take(&output, status: status)
        }
    }

    /// Deletes one key and returns its already-applied canonical delta.
    public func delete(_ key: String) throws -> Data {
        let keyBytes = Array(key.utf8)
        return try lock.withLock {
            var output = crdt_buffer(data: nil, len: 0)
            let status = keyBytes.withUnsafeBufferPointer { buffer in
                crdt_lww_map_delete(requireHandle(), buffer.baseAddress, buffer.count, &output)
            }
            return try take(&output, status: status)
        }
    }

    /// Returns an opaque value, preserving the difference between no key and
    /// a present empty value.
    public func get(_ key: String) throws -> Data? {
        let keyBytes = Array(key.utf8)
        return try lock.withLock {
            var output = crdt_buffer(data: nil, len: 0)
            var present: UInt8 = 0
            let status = keyBytes.withUnsafeBufferPointer { buffer in
                crdt_lww_map_get(requireHandle(), buffer.baseAddress, buffer.count, &output, &present)
            }
            try check(status)
            defer { crdt_buffer_free(output) }
            guard present == 1 else { return nil }
            guard let data = output.data else {
                guard output.len == 0 else { throw CRDTError(Int32(CRDT_INTERNAL)) }
                return Data()
            }
            return Data(bytes: data, count: output.len)
        }
    }

    /// Returns visible keys in canonical order. Its local byte list is an FFI
    /// read API, never a CRDT frame or persistence representation.
    public func keys() throws -> [String] {
        let payload = try lock.withLock {
            var output = crdt_buffer(data: nil, len: 0)
            return try take(&output, status: crdt_lww_map_keys(requireHandle(), &output))
        }
        return try decodeKeys(payload)
    }

    /// Returns a complete canonical LWW-Map TypeID 9 state frame.
    public func state() throws -> Data {
        try lock.withLock {
            var output = crdt_buffer(data: nil, len: 0)
            return try take(&output, status: crdt_lww_map_state(requireHandle(), &output))
        }
    }

    /// Returns the HLC state to persist atomically with `state()`.
    public func clockState() throws -> RGAClockState {
        try lock.withLock {
            var output = crdt_clock_state(replica_id: crdt_buffer(data: nil, len: 0), wall_time: 0, logical: 0)
            try check(crdt_lww_map_clock_state(requireHandle(), &output))
            defer { crdt_clock_state_free(output) }
            guard let replica = output.replica_id.data,
                  let replicaID = String(data: Data(bytes: replica, count: output.replica_id.len), encoding: .utf8) else {
                throw CRDTError(Int32(CRDT_INTERNAL))
            }
            return RGAClockState(replicaID: replicaID, wallTime: output.wall_time, logical: output.logical)
        }
    }

    private func requireHandle() -> OpaquePointer {
        guard let handle else {
            fatalError("LWWMap used after deinitialization")
        }
        return handle
    }

    private func take(_ output: inout crdt_buffer, status: Int32) throws -> Data {
        try check(status)
        defer { crdt_buffer_free(output) }
        guard let data = output.data else {
            guard output.len == 0 else { throw CRDTError(Int32(CRDT_INTERNAL)) }
            return Data()
        }
        return Data(bytes: data, count: output.len)
    }

    private func check(_ status: Int32) throws {
        guard status == CRDT_OK else { throw CRDTError(status) }
    }
}

private func decodeKeys(_ data: Data) throws -> [String] {
    let bytes = Array(data)
    var cursor = 0
    let count = try readUvarint(bytes, &cursor)
    guard count <= UInt64(Int.max) else { throw CRDTError(Int32(CRDT_INTERNAL)) }
    var result: [String] = []
    result.reserveCapacity(Int(count))
    for _ in 0..<Int(count) {
        let length = try readUvarint(bytes, &cursor)
        guard length <= UInt64(bytes.count - cursor),
              let key = String(bytes: bytes[cursor..<(cursor + Int(length))], encoding: .utf8) else {
            throw CRDTError(Int32(CRDT_INTERNAL))
        }
        cursor += Int(length)
        result.append(key)
    }
    guard cursor == bytes.count else { throw CRDTError(Int32(CRDT_INTERNAL)) }
    return result
}

private func readUvarint(_ bytes: [UInt8], _ cursor: inout Int) throws -> UInt64 {
    let start = cursor
    var value: UInt64 = 0
    for shift in 0..<10 {
        guard cursor < bytes.count else { throw CRDTError(Int32(CRDT_INTERNAL)) }
        let byte = bytes[cursor]
        cursor += 1
        guard shift < 9 || byte <= 1 else { throw CRDTError(Int32(CRDT_INTERNAL)) }
        value |= UInt64(byte & 0x7F) << UInt64(shift * 7)
        if byte & 0x80 == 0 {
            guard cursor - start == uvarintSize(value) else { throw CRDTError(Int32(CRDT_INTERNAL)) }
            return value
        }
    }
    throw CRDTError(Int32(CRDT_INTERNAL))
}

private func uvarintSize(_ value: UInt64) -> Int {
    var value = value
    var size = 1
    while value >= 0x80 {
        value >>= 7
        size += 1
    }
    return size
}
