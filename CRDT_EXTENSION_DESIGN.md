# CRDT collection extension design

This document defines the collection boundary after the stable v1 core.
LWW-Map, RGA text (including run-v2), and OR-Tree have framed codecs, bounded
decoders, HLC snapshots, and fuzz coverage, but remain experimental until their
exact tombstone-GC lifecycle ships. RGA text v1 now has bounded delayed
integration, incremental indexed projection, complete snapshots, and leaf-only
compaction guarded by an external exact-acknowledgement epoch.
LWW-Set remains an in-memory collection and its
reserved frame IDs must not be advertised as a wire protocol.

## Experimental protocol policy

`crdt.ProtocolPolicy` is the per-replication-group opt-in boundary. Its zero
value advertises only stable G-Counter, OR-Set, and PN-Counter frames. Setting
`AllowExperimental` additionally advertises LWW-Map, RGA, and OR-Tree. Peers
must compare the advertised `FrameTypes` before sending frames; this is capability
negotiation, not a dynamic plugin registry and not a replacement for
authentication, authorization, decoder limits, or application-level schema
validation.

The policy does not change frame parsing or make an unknown type acceptable.
Callers that opt in must persist the associated HLC state and retain all
LWW-Map, RGA, or OR-Tree tombstones. Exact acknowledgement and compaction for
those types are a release gate before they become stable.

## Semantics

| Structure | Conflict rule | Local API | Merge cost | Retained metadata |
| --- | --- | --- | --- | --- |
| PN-Counter | component-wise maximum | increment/decrement | O(replicas) | two component maps |
| LWW-Set | largest HLC tag per element | add/remove | O(changed elements) | removed entries |
| LWW-Map | largest HLC tag per string key | set/delete | O(changed keys) | deleted entries and tags |
| RGA text v1 | union nodes plus tombstones; sibling IDs sort by descending tag | insert/delete by rune offset | O(log n) offset lookup; O(n) render | deleted structural anchors |
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

## LWW protocol completion gate

LWW-Map meets this gate as an experimental protocol: it has state and delta
frames, bounded canonical decoders, HLC-bearing snapshots, Delta coalescing
compatibility, independent golden-vector coverage, and fuzzing. The remaining
LWW-Set protocol must deliver all of the following before it is exported as a
network-replicable library primitive:

1. Reserve state/delta type IDs, canonical framed codecs, bounded decoders,
   canonical re-encoding tests, and delta coalescer support.
2. Add snapshots that include HLC state, plus explicit tombstone-compaction
   preconditions. LWW cannot silently discard deleted entries while an offline
   replica may still hold an older write.
3. Keep all decoded values bounded by application-selected frame limits. Reject
   malformed UTF-8, non-canonical integers, duplicate IDs, tag conflicts,
   unresolved parents in complete snapshots, and cycles.
4. Add independent golden vectors and fuzz targets. A unit test that encodes
   with the same constructor it decodes is insufficient evidence of wire
   compatibility.

## Performance and safety constraints

- No caller-owned byte slice is retained by LWW-Map. Reads also return copies.
- RGA keeps enter/exit markers in an indexed sequence and a linked scan order,
  so it does not rebuild adjacency for each edit. Parent children are inserted
  deterministically by descending tag; run-v2 compacts same-replica parent
  chains on the wire without changing scalar Position semantics.
- RGA compaction only removes deleted leaves after external authenticated exact
  acknowledgement, durable post-compaction checkpointing, and retirement of
  old deltas. Descendant anchors remain retained.
- OR-Tree has the same lifecycle boundary. `tree.Options` bounds node,
  tombstone, and value retention on both mutation and recovery; its compactor
  removes only requested tombstoned leaves and refuses any retained structural
  anchor. `tombstonegc.Coordinator.AcknowledgeAndCompactTarget` can reuse the
  exact-acknowledgement epoch, but cannot substitute for durable checkpointing
  and epoch-bound retirement of old frames.
- Bound document nodes, operation bytes, sibling fan-out, and retained
  tombstones at the transport/application boundary. Checksums detect corruption
  only; authentication, authorization, encryption, rate limits, and quotas
  remain the embedding application's responsibility.
- Images, audio, video, and files are replicated as `attachment.Reference`
  metadata only: opaque object ID, canonical MIME type, declared size, and
  SHA-256 digest. The application owns object authorization, malware/content
  scanning, delivery quotas, and digest verification before decode/render.
  `attachment.Register` uses the experimental LWW-Map TypeIDs 9/10 under a
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
invariant. Tree framing, bounded decode, snapshot/HLC recovery, and leaf-only
compaction are implemented experimentally. Before calling
`CompactTombstones`, an application must collect exact authenticated
acknowledgements from every current member in one epoch, persist a
post-compaction checkpoint, and reject or retire every old-epoch frame. The
leaf restriction is deliberately stricter than an acknowledgement proof: a
known child makes its deleted parent a required structural anchor. These
requirements remain a stable-promotion gate.
