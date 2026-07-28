# CRDT collection extension design

This document defines the next collection boundary. The in-memory LWW and RGA
implementations provide the merge semantics and validation surface; they must
not be advertised as transport-ready until the framing work below lands.

## Semantics

| Structure | Conflict rule | Local API | Merge cost | Retained metadata |
| --- | --- | --- | --- | --- |
| PN-Counter | component-wise maximum | increment/decrement | O(replicas) | two component maps |
| LWW-Set | largest HLC tag per element | add/remove | O(changed elements) | removed entries |
| LWW-Map | largest HLC tag per string key | set/delete | O(changed keys) | deleted entries and tags |
| RGA text | union nodes plus tombstones; sibling IDs sort by descending tag | insert/delete by rune offset | O(changed nodes), render O(nodes + sibling sorting) | deleted node IDs |

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

## Protocol completion gate

Before exporting these types as network-replicable library primitives, deliver
all of the following together:

1. Reserve state/delta type IDs, canonical framed codecs, bounded decoders,
   canonical re-encoding tests, and delta coalescer support.
2. Add snapshots that include HLC state, plus explicit tombstone-compaction
   preconditions. LWW and RGA cannot silently discard deleted entries while an
   offline replica may still hold an older write/node.
3. Keep all decoded values bounded by application-selected frame limits. Reject
   malformed UTF-8, non-canonical integers, duplicate IDs, tag conflicts,
   unresolved parents in complete snapshots, and cycles.
4. Add independent golden vectors and fuzz targets. A unit test that encodes
   with the same constructor it decodes is insufficient evidence of wire
   compatibility.

## Performance and safety constraints

- No caller-owned byte slice is retained by LWW-Map. Reads also return copies.
- RGA rendering uses an explicit traversal stack, not recursion proportional to
  document length. It materializes adjacency once per render and sorts only
  siblings; a future high-throughput editor can cache an indexed projection,
  but must invalidate it on every merge.
- A production text integration should batch adjacent local characters into
  chunks only after preserving the above position/order semantics. Chunking is
  an optimization, not an alternate conflict rule.
- Bound document nodes, operation bytes, sibling fan-out, and retained
  tombstones at the transport/application boundary. Checksums detect corruption
  only; authentication, authorization, encryption, rate limits, and quotas
  remain the embedding application's responsibility.

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
invariant. Tree framing, bounded decode, snapshot/HLC recovery, and exact
tombstone GC must ship as one unit.
