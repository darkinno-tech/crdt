# CRDT collection extension design

This document defines stable collection boundaries after the v1 core. LWW-Set,
LWW-Map, legacy scalar RGA v1, and generic list RGA have framed codecs,
bounded decoders, HLC snapshots, exact-acknowledgement tombstone retirement,
fuzz coverage, and normative vectors. New Go RGA groups use compact run-v2
frames (TypeIDs 19/20) through `crdt.DefaultRGAFrameType()`. Run-v2 retains
scalar position semantics but must be bound to its own matching manifest; it
cannot share a group with a scalar-v1 client. Scalar RGA v1 remains stable for
migration and has bounded delayed integration, indexed projection, complete
snapshots, and leaf-only compaction.

Run-v2 is a documented cross-language contract: [the wire specification](../protocol/rga-run-v2.md)
defines its canonical outer frame, block encoding, ordering, resource boundary,
and independently consumable vectors. A Wasm integration is available for
semantic reuse, while a native client must implement that specification rather
than infer a format from Go internals.

## Protocol negotiation

`crdt.ProtocolPolicy` enumerates every implemented stable frame pair. Peers
must still compare the authenticated manifest-selected state/delta pair before
sending frames; this is capability negotiation, not a dynamic plugin registry
and not a replacement for authentication, authorization, decoder limits, or
application-level schema validation. `AllowExperimental` is a no-op retained
only for source compatibility with earlier releases.

The policy does not change frame parsing or make an unknown type acceptable.
Callers that opt in must persist the associated HLC state and retain all
LWW-Set, LWW-Map, legacy RGA v1, or generic RGA list tombstones. Stable
HLC-backed protocols use the same recovery and tombstone-retention discipline.
Exact acknowledgement and compaction remain application responsibilities.

## Semantics

| Structure | Conflict rule | Local API | Merge cost | Retained metadata |
| --- | --- | --- | --- | --- |
| PN-Counter | component-wise maximum | increment/decrement | O(replicas) | two component maps |
| LWW-Set | largest HLC tag per element | add/remove | O(changed elements) | removed entries |
| LWW-Map | largest HLC tag per string key | set/delete | O(changed keys) | deleted entries and tags |
| RGA text (legacy scalar v1 or run-v2) | union nodes plus tombstones; sibling IDs sort by descending tag | insert/delete by rune offset | O(log n) offset lookup; O(n) render | deleted structural anchors |
| OR-Tree | union immutable parent links plus tombstones | add/remove node instance | O(changed nodes); O(nodes) projection | deleted structural anchors |
| Attachment reference | largest HLC tag per application key | put/delete immutable object reference | O(changed keys) | deleted entries and tags; no media bytes |

LWW deliberately has no hidden "add wins" tie. `crdt.Tag.Compare` orders wall
time, logical counter, then replica ID. Every replica ID is globally unique and
its HLC state is persisted before restart. A tag collision with incompatible
content is rejected rather than resolved arbitrarily.

RGA uses a root position, a parent position per Unicode scalar, and a stable
node ID. A local offset is converted to its visible predecessor only at the
mutation boundary. An insertion and deletion may be delivered in either order:
the tombstone is retained and wins when the node eventually arrives. Unknown
parents are allowed for reordering; completed cycles and conflicting node IDs
are rejected.

## LWW stability requirements

LWW-Set and LWW-Map both provide state and delta frames, bounded canonical
decoders, HLC-bearing snapshots, delta coalescing compatibility, golden-frame
coverage, malformed-input fuzz targets, and exact-acknowledgement compaction.
Neither type may silently discard a delete while an offline replica can still
hold an older write.

An application using either protocol must therefore:

1. Bind concrete frame IDs, element codec (for LWW-Set), and semantics version
   in an authenticated manifest.
2. Persist the HLC-backed snapshot with the application frontier/outbox record
   before reusing a replica ID.
3. Apply limits to every transport body, frame, payload, element, tag, string,
   and retained-entry budget; malformed or conflicting input must not partly
   mutate the receiver.
4. Define retention, rejoin, and recovery behavior before calling
   `CompactTombstones`; use an authenticated exact-acknowledgement coordinator,
   durable post-compaction checkpoint, and old-frame retirement.

## Performance and safety constraints

- No caller-owned byte slice is retained by LWW-Map. Reads also return copies.
- RGA keeps enter/exit markers in an indexed sequence and a linked scan order,
  so it does not rebuild adjacency for each edit. Parent children are inserted
  deterministically by descending tag; run-v2 compacts same-replica parent
  chains on the wire without changing scalar Position semantics.
- RGA compaction only removes deleted leaves after external authenticated exact
  acknowledgement, durable post-compaction checkpointing, and retirement of
  old deltas. Descendant anchors remain retained. A coordinator may use RGA's
  `CompactEligibleTombstones` only to process an already proven batch in
  child-before-parent order; pending state and unacknowledged tags still block
  collection.
- Stable OR-Tree v1 has the same lifecycle boundary. `tree.Options` bounds
  node, tombstone, and value retention on mutation and recovery; its ordinary
  compactor removes only requested tombstoned leaves, while its eligible
  compactor makes leaf-to-root progress only through an already exact-
  acknowledged deleted branch. `tombstonegc.Coordinator.AcknowledgeAndCompactTarget`
  can reuse the exact-acknowledgement epoch, but cannot substitute for durable
  checkpointing and epoch-bound retirement of old frames.
- Bound document nodes, operation bytes, sibling fan-out, and retained
  tombstones at the transport/application boundary. Checksums detect corruption
  only; authentication, authorization, encryption, rate limits, and quotas
  remain the embedding application's responsibility.
- Images, audio, video, and files are replicated as `attachment.Reference`
  metadata only: opaque object ID, canonical MIME type, declared size, and
  SHA-256 digest. The application owns object authorization, malware/content
  scanning, and delivery quotas; it calls `Reference.Verify` to stream-check
  exact size and digest before decode/render.
  `attachment.Register` uses the stable LWW-Map TypeIDs 9/10 under a
  distinct manifest schema and semantics version; raw or signed media URLs
  never belong in CRDT values.

## Verification matrix

- Algebra: merge is commutative, associative, and idempotent.
- Delivery: duplicate, reversed, and delete-before-insert deltas converge.
- Safety: invalid UTF-8, range errors, tag conflicts, cycles, oversized frames,
  and non-canonical frames leave the receiver unchanged.
- Concurrency: race tests cover local edits, merge, reads, and serialization.
- Capacity: benchmark live text, tombstone-heavy text, wide sibling fan-out,
  large LWW values, and merge under a target production limit.

## OR-Tree design boundary

The tree type is deliberately not modelled as `map[node]parent` with a
last-write-wins parent pointer: concurrent moves can create `A -> B -> A`, and
each replica can render a different repair. The first tree protocol is an
observed-remove rooted forest with these rules:

1. A node instance is identified by its immutable creation tag, not a display
   name. An add records one parent instance and carries the complete node
   identity.
2. A remove tombstones only creation tags observed by that replica. It may
   arrive before its add, exactly as an OR-Set tombstone does.
3. The visible projection starts at the synthetic root, follows only live
   parent links, sorts siblings by canonical tag, and hides dangling nodes
   until their parents arrive. Frames reject completed cycles; snapshots also
   reject missing non-root parents.
4. v1 has insert and observed-remove only. A move is represented as a remove
   plus a newly created node instance; a later move protocol must introduce an
   explicit, independently proven attachment register rather than mutating a
   parent pointer in place.

This keeps merge a set union plus tombstone subtraction, preserving
commutativity, associativity, idempotence, and a mechanically testable no-cycle
invariant. Tree framing, bounded decode, snapshot/HLC recovery, canonical
cross-language vectors, and exact-acknowledgement leaf-to-root compaction are
stable v1; [the normative protocol](../protocol/or-tree-v1.md) fixes that
contract. Before calling `CompactTombstones` or `CompactEligibleTombstones`, an
application must collect exact authenticated acknowledgements from every
current member in one epoch, persist a post-compaction checkpoint, and reject
or retire every old-epoch frame. The structural restriction is deliberately
stricter than an acknowledgement proof: an unselected or live child makes its
deleted parent a required anchor.
