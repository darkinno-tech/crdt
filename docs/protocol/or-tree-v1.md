# Observed-remove tree v1 wire protocol

This is the normative wire specification for the stable observed-remove tree.
It defines immutable parent links, deterministic visibility, and bounded
canonical frames; it deliberately does **not** define a concurrent move
operation. Implementations MUST satisfy this document and the
[canonical vectors](testdata/or-tree-v1-vectors.json).

The key words **MUST**, **MUST NOT**, **SHOULD**, and **MAY** are normative.

## 1. Contract and negotiation

Before sending a frame, an authenticated `replica.Manifest` MUST bind:

| Field | Required value |
| --- | --- |
| state TypeID | `17` |
| delta TypeID | `18` |
| codec ID | empty byte string |
| semantics version | `1` (`tree.SemanticsVersion`) |
| schema ID | one exact, application-owned node-value schema |
| epoch and limits | exact-match authenticated group values |

`SchemaID` MUST identify how opaque node values are decoded, validated, and
rendered. The empty codec ID does not mean untyped values. TypeIDs, CRC-32C,
and a matching schema ID do not authenticate a peer, authorize a mutation,
sanitize a value, or encrypt content. A tree manifest MUST NOT share a
replication group with a different CRDT type or a future tree-move protocol.

## 2. Frame and payload

Each message uses the repository's canonical CRDT frame envelope with TypeID
`17` or `18` and an empty codec ID. `uvarint`, `bytes`, `tag`, checksum, and
canonical tag ordering are defined by the [RGA run-v2 protocol](rga-run-v2.md).

```text
node          = id parent-present [parent] value
id            = tag
parent-present = uvarint(0 / 1)
parent        = tag
value         = bytes ; opaque, application-schema-defined bytes
payload       = node-count node* tombstone-count tombstone*
tombstone     = tag
```

`node-count` and `tombstone-count` are uvarints. Nodes sort by `id` ascending;
tombstones sort by tag ascending. `parent-present = 0` means the synthetic
root. `parent-present = 1` requires a valid nonzero parent tag. A decoder MUST
reject non-canonical varints, duplicate or unsorted tags, an unknown flag,
invalid tags, malformed lengths, trailing bytes, invalid checksums, or a
non-empty codec ID.

A TypeID `17` state MUST contain every non-root parent and no cycle. A TypeID
`18` delta MAY reference a missing parent, because independently delivered
deltas can arrive out of order; it MUST still be acyclic with its own known
nodes. A receiver applies an incoming delta only if the union is acyclic and an
existing node ID has exactly the same immutable `{parent, value}`. A conflicting
reuse of an ID is rejected without mutation.

## 3. Semantics and projection

Tree v1 state is the union of immutable node instances and deletion tombstones.
A creation tag identifies an instance, rather than a display name. `Add` creates
one immutable parent link; `Remove` adds tombstones only for observed creation
tags. A tombstone wins when its add arrives later.

The visible tree starts at the synthetic root, follows only live parent links,
and traverses siblings in descending canonical tag order. A deleted or missing
parent hides its descendants from the visible projection while the retained
records remain available to resolve reordering. Union plus tombstone
subtraction makes duplicate delivery idempotent and merge commutative and
associative.

There is no in-place move. Moving a visible node in v1 means removing its old
instance and adding a new instance below the new parent. Concurrent moves need
a separately specified and proven protocol; they MUST NOT be emulated by
rewriting a v1 parent link. This boundary avoids claiming the much stronger
semantics required by formal move-capable tree CRDT algorithms
([Kleppmann et al.](https://martin.kleppmann.com/2021/10/07/crdt-tree-move-operation.html)).

## 4. Limits, security, and persistence

Receivers MUST bound outer frame bytes, payload bytes, node and tombstone
counts, tag/replica-ID bytes, and each opaque value before allocating or
mutating. They MUST set retained `MaxNodes`, `MaxTombstones`, and
`MaxValueBytes` according to the replication group rather than process memory.
A rejected delta or state MUST leave nodes, tombstones, and the HLC unchanged.

Applications MUST authenticate and authorize the group before decoding;
validate the manifest-selected node schema before use; rate-limit writers; and
atomically persist `{state frame, HLC state, delivery frontier/outbox}`. Recover
a same-ID replica through `SnapshotCurrentStateWithLimits` and
`NewFromSnapshotWithOptionsAndLimits`; state bytes without the HLC can cause
new tag reuse. CRC-32C detects accidental corruption only.

## 5. Tombstone lifecycle

Tombstones preserve out-of-order safety and structural anchors. They MUST NOT
be discarded solely because a maximum HLC, Merkle digest, or ordinary receipt
was observed. After every member in one authenticated membership epoch has
acknowledged the exact tombstone tags, a durable post-compaction snapshot has
been recorded, and old-epoch frames have been retired, an application MAY use
`CompactTombstones` or `CompactEligibleTombstones` (including via
`tombstonegc.Coordinator`).

`CompactTombstones` is all-or-nothing and accepts only deleted leaves. The
eligible variant makes safe leaf-to-root progress through one
exact-acknowledged batch: an unselected or live structural child remains an
anchor, so its parent is retained. Neither method proves membership authority,
checkpoint durability, or old-frame retirement.

## 6. Conformance and versioning

For every [vector](testdata/or-tree-v1-vectors.json), an implementation MUST
verify its frame, decode it, re-encode it identically, and produce the listed
visible values and tombstone count. It SHOULD also mutate a checksum, TypeID,
ordering, varint representation, parent link, and resource limit, confirming
rejection without state change. The repository additionally runs shuffled
duplicate/out-of-order three-editor delivery and exact-acknowledgement
compaction simulations, race detection, fuzzing, and wide-tree/tombstone
benchmarks.

TypeIDs `17/18` and semantics version `1` are immutable. Any incompatible
change to framing, ordering, conflict resolution, node-value schema semantics,
compaction, or move behavior requires a new TypeID pair, semantic version,
vectors, manifest agreement, and migration path.
