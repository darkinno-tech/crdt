# Packed RGA v3 wire protocol

This is the normative contract for compact RGA frames with state TypeID `29`,
delta TypeID `30`, empty codec ID, and semantics version `3`. It is an
explicitly negotiated protocol: scalar RGA v1 (`11/12`) and run-v2 (`19/20`)
remain immutable, distinct frame pairs. A peer MUST reject another pair rather
than falling back or attempting conversion.

Packed v3 preserves the same logical state as run-v2: immutable scalar
`(position, parent, rune)` nodes, tombstones, descending sibling order, and
pending-parent behavior. It reduces bytes only for a canonical same-replica
parent chain whose HLC tags can be reconstructed exactly. It never combines
positions, changes deletion semantics, or authorizes tombstone compaction.

## Negotiation and bounds

Before any bytes are accepted, an authenticated Manifest MUST bind TypeIDs
`29/30`, semantics version `3`, the empty codec ID, application schema and
epoch, and compatible frame, node, tombstone, pending, replica-ID, and retained
state limits. Type IDs and CRC-32C do not provide authentication, authorization,
replay protection, or confidentiality.

The Go API is deliberately opt-in:

```go
kind := text.PackedFrameType()
manifest, err := replica.NewManifest("notes/42", "example.com/note/v3", 1,
    replica.Protocol{StateID: kind.StateID, DeltaID: kind.DeltaID, SemanticsVersion: kind.SemanticsVersion},
    crdt.ProtocolPolicy{})
_ = manifest
_ = err

update, err := document.InsertPackedBinaryWithLimits(0, pastedText, receiveLimits)
```

`InsertPackedBinaryWithLimits`, delete/replace counterparts, complete-state
methods, snapshots, and snapshot-delta generation preflight output before local
mutation. A host MUST persist `{state, HLC state, delivery frontier/outbox}`
atomically before reusing a replica ID.

## Envelope and payload

Every message uses the canonical v1 CRDT envelope. All integers are shortest
unsigned LEB128 `uvarint`; `bytes` is a length followed by exactly that many
bytes.

```text
frame   = "CRDT" uvarint(1) uvarint(type-id) bytes(empty-codec) bytes(payload) crc32c
payload = block-count block* tombstone-count tombstone*

node-block    = uvarint(0) tag parent rune
chain-block   = uvarint(1) count replica-id parent (wall logical rune)*
packed-chain  = uvarint(2) count replica-id parent first-wall first-logical
                transition-bits wall-gap* text
parent        = uvarint(0) / uvarint(1) tag
transition-bits = bytes ; exactly ceil((count - 1) / 8) bytes
text          = bytes ; valid UTF-8 with exactly count Unicode scalars
```

`count` is at least two for chain blocks. A packed chain starts at
`(first-wall, first-logical, replica-id)`. For every following scalar, the
corresponding least-significant-first bitmap bit is either:

- `0`: the next tag is the same wall time with `logical + 1`;
- `1`: read one positive `wall-gap`; the next tag is
  `(wall + wall-gap, 0, replica-id)`.

Unused high bitmap bits MUST be zero. Wall addition and logical increment MUST
not overflow. The first node uses `parent`; each following node's parent is the
previous reconstructed node. The text's UTF-8 scalar sequence supplies the
runes in that order.

The ordinary `chain-block` is retained for any canonical chain that does not
meet those reconstruction rules or would not be smaller. This makes mixed
topologies and imported/non-dense HLC tags safe without adding a lossy mode.

## Canonicality, correctness, and resource safety

The decoder MUST first bound the complete frame and every declared count/byte
length, verify the checksum, then reject malformed tags, bad UTF-8, duplicate
nodes or tombstones, an invalid scalar, an incomplete state graph, cycles,
trailing bytes, non-shortest varints, unused bitmap bits, overflow, and an
alternative representation of the same graph. It reconstructs all tags and
re-encodes the complete graph; only byte-for-byte identical payloads are
canonical.

A state frame MUST contain each non-root parent and be acyclic. A delta MAY
start from an external parent, but the receiver may retain it only within its
bounded pending policy and MUST NOT serialize unresolved state as a snapshot.
Validation, graph construction, canonicality, retention preflight, HLC witness,
and installation are atomic: a failed frame cannot change visible text, HLC,
pending state, or persisted state.

The contract does not change RGA's tombstone rule. Deleted markers still anchor
descendants; compact them only after authenticated, exact current-member
acknowledgements, durable checkpointing, and old-frame retirement.

## Optional outer frame v2 for initial sync

Packed-v3 can additionally use the separately negotiated
[compression-aware outer frame v2](frame-v2.md). This is not a packed-v4
semantic protocol: it retains TypeIDs `29/30`, semantics version `3`, and the
same canonical decoded packed-v3 payload. A group MUST bind
`WireFormatVersion: frame.FormatVersionV2` in its authenticated
`replica.Protocol`; an outer-v1 packed-v3 group and an outer-v2 packed-v3 group
are intentionally incompatible.

The direct Go APIs are `MarshalPackedFrameV2`, `Delta.MarshalPackedFrameV2`,
the packed `Insert`/`Delete`/`Replace` `FrameV2WithLimits` variants,
`SnapshotPackedFrameV2CurrentState`, and packed snapshot-delta v2 methods.
They preflight the final v2 envelope before a local mutation. The compressor
uses raw v2 when DEFLATE would not reduce the complete frame, but even those
small frames require an outer-v2 Manifest.

Build the bounded browser artifact with `WASM_RGA_PROTOCOL=packed-v3-v2` and
pass `RGA_PROTOCOL_PACKED_V3_V2` to `initRGAWasm`. It exposes outer format
version `2` alongside the existing frame pair, rejects v1 inputs before any
RGA mutation, and is rejected by the ordinary `packed-v3` artifact. The
dependency-free TypeScript `decodeFrame` helper still validates v1 envelopes
only; the loader deliberately delegates v2 frame decoding to the bounded Go
Wasm runtime. A pure TypeScript decoder must implement the same bounded raw
DEFLATE checks before it advertises v2.

DEFLATE is neither authentication nor encryption. The outer decoder checks
`MaxFrameBytes` before parsing and `MaxPayload` before expansion; applications
still need transport body caps, TLS, authorization, and an authenticated exact
Manifest.

## Compatibility and verification

Packed v3 is implemented by the Go library and an explicitly built Go/Wasm
runtime (`WASM_RGA_PROTOCOL=packed-v3` or the outer-v2
`WASM_RGA_PROTOCOL=packed-v3-v2`). The TypeScript loader exposes only the exact
manifest contract and delegates every RGA frame to that runtime; it does not
decode or translate packed frames. Native implementations MUST NOT
advertise TypeIDs `29/30` until they implement this exact decoder, limits,
canonical re-encoder, and vectors. It is not Yjs wire compatibility; Yjs
updates stay in the separately bounded opaque relay/store boundary.

[`testdata/rga-packed-v3-vectors.json`](testdata/rga-packed-v3-vectors.json)
contains deterministic dense-chain, wall-transition, and tombstone data. Every
implementation MUST decode its envelope, reconstruct the listed node graph,
re-encode byte-for-byte, apply it to an empty document, and reject a changed
checksum, TypeID, bitmap, varint, or resource limit without state mutation.

Run the focused checks with:

```sh
go test ./text ./replica ./cmd/crdt-compare
go test -race ./text
go test -run='^$' -fuzz=FuzzRGAPackedUnmarshal -fuzztime=150000x -parallel=1 ./text
go test -run='^$' -bench='BenchmarkRGADeltaWireProtocols/(run-v2|packed-v3)$|BenchmarkRGAMarshalLinearDocument/(run_v2|packed_v3)$' -benchmem ./text
make wasm-packed-test
make wasm-packed-v2-test
go run ./cmd/crdt-compare -protocol=packed-v3 -scenario=initial -sizes=4096,16384
go run ./cmd/crdt-compare -protocol=packed-v3-v2 -scenario=initial -sizes=4096,16384
```

Any incompatible field, ordering, reconstruction rule, or compaction behavior
requires a new TypeID pair, semantic version, manifest agreement, and vectors.
