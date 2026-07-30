# Generic list RGA v1 wire protocol

This is the normative stable contract for generic list RGA frames (TypeIDs
`21/22`, semantics version `1`). Implementations MUST satisfy this document
and the [canonical vectors](testdata/list-rga-v1-vectors.json).

An authenticated manifest binds the frame pair, one exact canonical element
codec ID, schema ID, epoch, and limits. A payload is `node-count node*
tombstone-count tombstone*`; nodes and tombstones sort by ascending tag. A node
is `id parent-present [parent] value`. `parent-present` is exactly `0` or `1`.
The value is one application element, not a text byte or a nested CRDT.

State frames MUST contain every non-root parent. Delta frames may refer to a
missing parent for out-of-order delivery, but their known graph MUST be acyclic.
Each received value MUST decode and re-encode to identical bytes before it is
retained. Receivers MUST bound frame/payload bytes, node/tombstone counts,
replica-ID and value bytes, retained state, and pending dependencies before
allocation or mutation.

The CRDT joins immutable node records and tombstones. Siblings use descending
tag order in the visible projection; duplicate delivery is a no-op and a
conflicting reuse of an ID is rejected. Persist state, HLC state, frontier, and
outbox atomically. A tombstone is removable only after exact authenticated
acknowledgements, a durable post-compaction checkpoint, and old-frame
retirement; it must also be a deleted leaf with no unresolved dependent.

Changing node ordering, parent semantics, codec handling, compaction rules, or
payload encoding requires a new TypeID pair and semantic version.
