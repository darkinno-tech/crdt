# Scalar RGA v1 wire protocol

This is the stable legacy scalar RGA contract for TypeIDs `11/12`, semantics
version `1`. It remains independently negotiated from run-v2 (`19/20`): a peer
MUST NOT fall back or reinterpret frames between the two. Implementations MUST
satisfy this document and the [canonical vectors](testdata/rga-scalar-v1-vectors.json).

The payload is `node-count node* tombstone-count tombstone*`, with tags sorted
ascending. A node is `id parent-present [parent] rune`, where `rune` is one
valid Unicode scalar. State frames contain complete parent closure; deltas may
be incomplete only for reordering and must be acyclic. Receivers reject a
wrong TypeID/non-empty codec, malformed or noncanonical payload, invalid rune,
duplicate/conflicting ID, checksum failure, trailing bytes, or a limit breach
before mutation.

Node union plus delete tombstones is commutative, associative, and idempotent.
Visible siblings are ordered by descending tag. Persist the state frame,
HLC state, delivery frontier, and outbox atomically. Exact acknowledgement,
durable post-compaction checkpoints, old-epoch frame retirement, and the
deleted-leaf rule are all required before tombstone removal.

Scalar v1 is stable for migration and existing deployments; new text groups
should use the compact run-v2 protocol. Any incompatible scalar-v1 change
requires a new state/delta pair and semantic version.
