# RGA run-v2 wire protocol

This is the normative wire specification for the compact RGA run-v2 protocol.
It lets a browser, mobile, or server implementation exchange collaborative-text
state and deltas without embedding Go or WebAssembly. A Go/Wasm runtime remains
the lowest-risk integration path because it reuses the implementation; a native
implementation **must** satisfy every rule in this document and the published
[canonical vectors](testdata/rga-run-v2-vectors.json).

The key words **MUST**, **MUST NOT**, **SHOULD**, and **MAY** are normative.

## 1. Contract and negotiation

Run-v2 has no in-frame semantic negotiation. Before sending any bytes, an
authenticated replication-group manifest MUST bind all of the following:

| Field | Required value |
| --- | --- |
| RGA state TypeID | `19` |
| RGA delta TypeID | `20` |
| codec ID | the empty byte string |
| semantics version | `2` |
| application schema ID and epoch | application-owned, exact-match values |
| input and retained-state limits | an agreed receiver policy |

The state/delta pair is one protocol. A peer MUST NOT fall back between run-v2
and legacy scalar RGA v1 (TypeIDs 11/12), and MUST reject a frame whose type,
codec, or manifest does not match its selected pair. Type IDs and CRC-32C do
not authenticate a peer, authorize access, prevent replay, or encrypt content.

## 2. Common frame envelope

Every RGA run-v2 message is one complete canonical CRDT frame. Integer fields
use an unsigned, little-endian base-128 varint (`uvarint`): the low seven bits
are emitted first, continuation bytes set bit 7, values are at most 64 bits,
and only the shortest representation is valid. `bytes` is `uvarint length`
followed by exactly that many bytes.

```text
frame = "CRDT" format-version type-id codec-id payload checksum
format-version = uvarint(1)
type-id = uvarint(19 for state, 20 for delta)
codec-id = bytes                 ; length MUST be zero for RGA run-v2
payload = bytes
checksum = four-byte big-endian CRC-32C (Castagnoli)
```

The checksum covers every byte after `"CRDT"` through the final payload byte;
it excludes the magic and checksum itself. The declared payload length MUST end
immediately before the checksum—trailing bytes are invalid. A receiver MUST
check the total body limit before copying or allocating from an untrusted
length, verify the checksum, then reject overlong varints, unknown envelope
versions, a zero TypeID, or any inconsistent length.

## 3. Identifiers and ordering

An RGA `Position` is a `Tag` with this serialized form:

```text
tag = replica-id wall-time logical
replica-id = bytes
wall-time = uvarint
logical = uvarint
```

`replica-id` MUST be non-empty after the implementation's replica-ID validation
(the Go implementation rejects a whitespace-only ID). A live logical replica
MUST have a globally unique ID, and recovery with the same ID MUST restore the
saved HLC state before creating another local mutation.

For canonical ordering, compare two tags by `wall-time`, then `logical`, then
the raw bytewise lexical order of `replica-id`, all ascending. This ordering is
used for node IDs and tombstones. It is deliberately independent of arrival
order and local wall-clock time.

## 4. Run-v2 payload grammar

The frame payload is:

```text
payload = block-count block* tombstone-count tombstone*
block-count = uvarint
tombstone-count = uvarint
tombstone = tag

block = node-block / chain-block
node-block = uvarint(0) tag parent rune
chain-block = uvarint(1) chain-count replica-id parent chain-item*
chain-count = uvarint       ; MUST be at least 2
chain-item = wall-time logical rune

parent = parent-present [tag]
parent-present = uvarint(0 or 1)
rune = uvarint              ; a valid Unicode scalar value
```

For a `node-block`, `tag` is the node ID. For a `chain-block`, every node's ID
uses the block `replica-id`; its wall time, logical counter, and rune come from
one `chain-item`. The first chain node has the supplied `parent`; each later
node's parent is the immediately preceding chain node. A parent flag of zero
means the synthetic root. Values outside the Unicode scalar range, duplicate
node IDs, duplicate tombstones, self-parenting, cycles, malformed tags,
truncation, and trailing payload bytes are invalid.

A **state** frame (TypeID 19) MUST contain every non-root parent and MUST be
acyclic. A **delta** frame (TypeID 20) MAY name an external initial parent. The
receiver retains such a delta only within its bounded pending-dependency policy
and integrates it once that parent arrives. It MUST NOT serialize a pending,
incomplete document as state.

## 5. Canonical block construction

The decoder expands blocks into scalar nodes, validates the complete CRDT
delta, then canonicalizes the result. A frame is valid only if re-encoding that
result produces byte-for-byte identical bytes. This makes different block
partitions, map iteration order, overlong varints, and alternate tag ordering
invalid even when they could describe the same logical graph.

To construct the canonical blocks for a node set:

1. Sort every unused node ID in ascending tag order.
2. Start a block at the first unused ID. Repeatedly append the only unused
   child whose parent is the current node and whose replica ID is the same as
   the block's first ID. Stop when there is zero or more than one such child.
3. Emit one `node-block` for a one-node block; otherwise emit one `chain-block`.
4. Continue with the next unused node ID in ascending order.
5. Sort tombstones by ascending tag order and append them after every block.

The rule preserves scalar RGA semantics while compacting the common contiguous
same-replica edit. It is a compression rule, not a new identifier scheme.

## 6. Replicated semantics

The logical state is a set of immutable `(position, parent, rune)` nodes plus a
set of tombstoned positions. Merge is set union for both sets. A tombstone that
arrives before its insertion MUST be retained; when the insertion later arrives
it remains hidden. A conflicting payload for an existing position is invalid.

Visible text is a deterministic depth-first traversal from the synthetic root.
For each parent, visit children in descending tag order; emit a node's rune only
when that node is not tombstoned, but always traverse its children. Thus a
deleted node remains a structural anchor until a separately authorized,
exact-acknowledgement compaction workflow proves it safe to remove. This wire
format does not make tombstone GC safe by itself.

All decoding and application MUST be atomic: validate the full frame, expanded
graph, canonical form, resource budgets, and manifest before changing visible
text, HLC state, pending queues, or persistence. Duplicate frames and a delta
whose contents are already present are successful no-ops.

## 7. Limits and persistence

Limits are part of deployment policy, not an excuse to accept partial input.
At minimum, bound transport bytes, complete frame and payload bytes, codec and
replica-ID lengths, node/tombstone counts, pending nodes and bytes, retained
nodes and tombstones, and local edit bytes/runes. The Go/Wasm browser runtime
uses a conservative default of 1 MiB per frame, 100,000 tags, 64 KiB replica
IDs, 100,000 retained nodes/tombstones, 10,000 pending nodes, and 512 KiB
pending bytes; an application MAY choose lower compatible limits.

Persist `{state frame, HLC clock state, frontier/outbox position}` atomically.
Persisting state alone and then reusing a replica ID can create a tag that is
not greater than a previous local mutation. Before compacting tombstones, use
an authenticated authoritative membership epoch, exact acknowledgements for
each requested tag, a durable post-compaction checkpoint, and retirement of
old-epoch frames.

## 8. Conformance vectors and verification

[`testdata/rga-run-v2-vectors.json`](testdata/rga-run-v2-vectors.json) contains
machine-readable canonical frames. Unsigned 64-bit values are JSON strings in
node/tombstone metadata so JavaScript and other runtimes do not lose precision.
Every implementation SHOULD, for every vector:

1. Decode the hexadecimal frame and verify the common envelope and CRC-32C.
2. Check its TypeID, empty codec ID, expanded nodes, parents, runes, and
   tombstones against the metadata.
3. Re-encode it byte-for-byte identically.
4. Apply it to an empty document and obtain the listed visible text.
5. Mutate a checksum byte, a varint representation, a type ID, and a resource
   limit; each case MUST fail without changing the target document.

The repository runs the same fixture through `go test ./text` and the
dependency-free TypeScript envelope decoder through `make typescript-test`.
Run `make wasm-test` for a real Go/Wasm–TypeScript three-replica session, and
`make wasm-v1-test` only when maintaining an explicitly negotiated legacy
migration group.

## 9. Versioning rule

TypeIDs 19/20 and semantics version 2 are immutable. Any incompatible field,
ordering, checksum, graph, or compaction change requires a new TypeID pair and
semantic version, a new manifest agreement, new vectors, and a migration path.
Never reinterpret an existing frame or silently accept a future version.
