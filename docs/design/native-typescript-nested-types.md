# Native TypeScript nested shared types

## Decision

Keep `native-ts-v1` unchanged: its JSON values are copied and atomic. Add
`native-ts-nested-v1` as an explicit higher-level semantic contract provided by
`NativeNestedDocument`, `NativeNestedMap`, and `NativeNestedArray`.

It reuses the canonical native update envelope but reserves one exact JSON
value shape for a child reference. A replication group must negotiate
`native-ts-nested-v1`; a plain `NativeDocument` is not a compatible nested
editor, even though it can relay the opaque values without corrupting them.

This is intentionally similar in interaction model to a shared-type tree, not
an attempt to claim Yjs API or update-format compatibility. A child is created
by `map.createMap`, `map.createArray`, `array.insertMap`, or
`array.insertArray`; ordinary `set`/`insert` remain atomic copied JSON.

## Representation and invariants

```text
root NativeMap / NativeArray
  | map-set or array-insert with immutable ID I
  v
{ "$crdt": "native-ts-nested-v1", "id": I, "type": "map" | "array" }
  |
  +-- internal target derived exactly from I
          |
          +-- LWW NativeMap or RGA NativeArray operations
```

- The reference ID must equal its enclosing map-write or array-entry ID.
  A child therefore has one parent, cannot be moved or aliased, and a cycle
  would require reusing a live immutable ID.
- The nested validator rejects marker-shaped JSON at every non-reference depth,
  type mismatches, reference-ID mismatches, active aliases, malformed IDs, and
  capacity excess before native state changes. Local child creation builds an
  operation without advancing the replica counter, preflights the nested
  metadata and native envelope, and only then commits both together.
- A child operation received before its parent reference is retained as one
  bounded canonical update. It is replayed only after the parent makes the
  expected child type known. Snapshots reject while such traffic is pending.
- Map links obey existing LWW ordering. Array links retain the existing RGA
  parent dependency and delete-before-insert tombstone behavior.
- A snapshot includes native roots/state/counter plus child declarations.
  Restore replays them through the same nested validator and advances the
  local counter, so a reused replica ID never creates an old identifier.

## Compatibility and lifecycle

`native-ts-v1` and Go framed protocols do not change. In particular,
`native-ts-nested-v1` has no Go TypeID and must never be passed to a Go frame
decoder or a run-v2 RGA group. Existing native-v1 clients may preserve the
reference JSON but cannot safely edit a nested group, so applications must
bind the semantics version in their authenticated group/schema admission.

Detached child identities are retained in the nested document until a future
protocol defines authenticated, checkpointed compaction. This preserves
reference-conflict detection and out-of-order recovery. Set `maxNestedTypes`
for the group's retained-history budget; the default is 10,000. Child updates
waiting for their parent are separately bounded by
`maxPendingNestedOperations` (default 10,000).

The child target contains its actor and counter. Consequently, a nested group
requires replica IDs short enough to fit the configured root-name byte limit
after this fixed prefix/suffix. The constructor rejects an incompatible budget
instead of truncating or hashing an ID.

## Security and operational boundary

The implementation validates canonical shape, limits, convergence state, and
single ownership. It does not authenticate peers, grant document access,
encrypt updates, enforce a replay window, guarantee durable delivery, or make
untrusted HTML/URLs safe. Hosts must authenticate and authorize before decode,
cap HTTP/WebSocket body bytes before allocating a `Uint8Array`, use compatible
limits at all peers, persist snapshot and counter atomically, and own retry and
outbox retention.

## Performance result and trade-off

`NativeDocument.transact` now accounts for the exact canonical envelope bytes
incrementally. It still validates each new operation and preflights the final
1 MiB/operation-count budget before mutation, but avoids repeatedly canonical-
encoding the entire pending transaction. This changes a large local batch from
quadratic envelope validation to linear byte accounting without changing its
bytes or admission rules.

The controlled 2026-07-30 benchmark on Darwin arm64 / Node 26.5.0 measured
the median of five samples as 25.8 ms for build+merge of 64 nested cards and
19.9 ms for snapshot+restore of the same data. The shuffled three-editor
simulation had a 122.2 ms median per 96-edit scenario. These are development
measurements, not a browser frame or production-capacity claim; use a Worker
for large state recovery and keep document limits tied to the product schema.

## Validation

- Unit tests cover recursive map/array access, copied output, restore/counter
  recovery, malformed reference rejection, ID mismatch, alias/cycle prevention,
  and bounded missing-parent queue rejection.
- A three-replica simulation delivers updates in reverse order with periodic
  duplicate delivery and asserts convergence.
- `bench/nested.bench.mjs` measures construction/merge, recovery, and the
  shuffled three-editor workload. See
  `docs/operations/native-ts-nested-benchmark-2026-07-30.md` for raw samples.
