import CRDTRGAFFI
import Foundation

/// Errors returned by the bounded Rust runtime's stable C ABI.
public struct CRDTError: Error, Equatable, Sendable {
    /// Stable status code from `crdt_rga.h`.
    public let code: Int32

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
