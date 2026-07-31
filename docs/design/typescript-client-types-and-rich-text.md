# TypeScript shared types and rich-editor architecture

## Decision

The TypeScript client now exposes a bounded `native-ts-collections-v1`
semantic layer for PN-Counter, add-wins OR-Set, LWW register, and
immutable-parent OR-Tree. It composes the existing canonical
`native-ts-v1` Map document rather than claiming a byte-compatible TypeScript
implementation of the Go framed protocols.

Rich-editor support remains a separate delivery track. The existing Go
`richtext-v1` protocol (TypeIDs 23/24) is manifest-bound and has a stronger
state/clock persistence contract than the TypeScript plain-text bindings. A
browser binding must use a bounded rich-text runtime, not flatten an editor
tree or send editor JSON/HTML as a string replacement.

## Evidence and trade-offs

| Dimension | Current evidence | Decision |
| --- | --- | --- |
| Correctness | Native Map already supplies canonical updates, copied JSON values, transaction batching, all-or-nothing decode, and LWW ID ordering. Go OR-Tree requires immutable parents and hides descendants of deleted/missing parents. | Build collection types as checked Map projections. Reuse native updates but reject values that violate each collection's invariant before Map mutation. |
| Performance | The native client already caches Map/Array projections; a Map write is suitable for a counter component, immutable add, or tree node. Rich text needs per-position marks and cannot safely be rebuilt after every editor callback. | Use O(actors) counter reads, O(adds) OR-Set projection, and bounded O(nodes) tree projection. Benchmark a three-replica offline workboard separately from protocol/Wasm benchmarks. |
| Security and resource use | Native decoders bound bytes, operations, map entries, JSON values, depth, and pending RGA nodes. Framed Go rich text additionally bounds marks, target x attribute work, and schema values before mutation. | Collection roots are private and unrecognised roots fail closed. Components must be monotone; node links must be acyclic and bounded. Hosts still authenticate/authorize, bind document/schema/limits, rate-limit, and persist outboxes. |
| Compatibility | `native-ts-v1` / `native-ts-nested-v1` are not Go frames, not RGA TypeID 19/20, and not Yjs. The Go rich-text protocol is TypeID 23/24 with an exact Manifest schema ID. | Negotiate `native-ts-collections-v1` independently. Do not describe the TypeScript collection layer as a Go client. Use Go/Wasm (or another independently conforming framed implementation) for an interoperable rich-text group. |
| Editor ergonomics | Quill has a Delta model with text, attributes, embeds, and a required trailing newline; ProseMirror/Tiptap, Slate, and Lexical have application-owned schemas and rich node trees. | Keep present adapters plain-text-only. Add rich bindings only after one explicit attribute/block/embed schema and an atomic rich-text operation API exist. |

## Collection contract

The collection layer owns a `NativeCollectionsDocument`; callers declare each
logical name as exactly one type. It reserves internal `native-ts-v1` Maps and
only accepts `map-set` operations for already-declared roots. This means a
malformed peer cannot introduce a raw Map/Array or bypass type checks through
the collection receive path.

```text
PN-Counter  actor -> { positive, negative, immutable native ID }
OR-Set      add-ID -> value            and  add-ID -> remove tombstone
LWW         "value" -> { ID, value | retained delete }
OR-Tree     node-ID -> { parent-ID | root, value } and node-ID -> tombstone
```

The types retain state needed for reordering. A tree child may arrive before
its parent, and an OR-Set tombstone may arrive before its add. Those entries
count against a declared limit; they are not silently discarded. Counter and
tree operations are immutable under their native IDs. A tree move creates a
new node after deleting the old one.

`NativeCollectionsSnapshot` stores logical declarations plus the underlying
native snapshot. It has the same recovery rule as the base client: root
declarations, retained state, and the actor counter must become durable with
the outbound log/frontier in one host transaction. It does not provide
authentication, authorization, receipt, encryption, or tombstone-GC proof.

## Rich-text implementation gate

The first production rich binding should be `richtext-v1` over a dedicated Go
Wasm runtime. It must expose only the following bounded operations:

```text
create / close
applyDelta(frame) -> spans
replaceWithAttributes(offset, count, text, attributes) -> one rich-text delta
format(offset, count, changes) -> one rich-text delta
snapshot / restore { state frame, HLC state, frontier }
```

`replaceWithAttributes` is a prerequisite: emitting a delete and an insert as
two editor callbacks can leave a replicated delete-only state when insertion is
rejected. It must preflight RGA text, mark counts, attributes, encoded frame
bytes, and the target x attribute product before changing either text or marks.
The browser wrapper must also expose typed spans/anchors, but must keep anchors
out of durable rich-text frames.

Only after that runtime exists, adapters can be introduced one schema at a
time:

1. Quill: translate its Delta retain/insert/delete operations and its terminal
   newline into the negotiated inline schema. Reject unknown formats/embeds;
   do not infer attributes for concurrent inserts.
2. ProseMirror and Tiptap: bind a fixed schema ID. Convert only approved text
   nodes and marks to spans, and model blocks/embeds through separately
   versioned objects. Remote writes use transactions tagged to avoid echo and
   preserve undo semantics.
3. Lexical and Slate: use application-provided schema ports. Their node keys,
   decorators, and custom elements must never be serialized as CRDT IDs.

HTML is not a rich-text transport. Renderer sanitisation, link policy, embed
authorization, selection presence, shared undo grouping, and the transport
receipt boundary stay owned by the host application.

## Verification plan

Collection tests cover shuffled duplicate delivery, remove-before-add,
counter-decrease rejection without state change, deterministic LWW convergence,
tree missing/deleted parents, snapshots, and unknown-root rejection. The
controlled offline-workboard benchmark covers three editing roles, multiple
types, reverse delivery, and duplicate packets.

For the rich-text runtime/bindings gate, require all of the following before
calling it complete:

- Go/TypeScript canonical frame and malformed-frame vectors;
- concurrent insert plus overlapping marks, deletes, duplicate/reordered
  deltas, atomic replacement rejection, snapshot/HLC recovery, and no remote
  editor echo;
- real Quill plus one schema-bound ProseMirror/Tiptap or Lexical integration;
- a 10k+ character rich document benchmark with marked ranges, an offline
  multi-editor simulation, and browser/Wasm performance samples;
- fuzzing of the Wasm host argument boundary and rich-text decoder under
  production receive limits.
