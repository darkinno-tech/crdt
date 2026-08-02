# Document-tree v1: nested shared types and subdocument boundaries

> Historical architecture only. New groups MUST use
> [document-tree v2](document-tree-v2.md), which contains a complete nested
> tree in one replication boundary and does not implement lazy references.

`documenttree` is the Go framed CRDT for a bounded collaborative object graph.
It is deliberately a new protocol rather than an adapter around `lww.Map`,
`list.RGA`, TypeScript `native-ts-nested-v1`, or Yjs updates.

## Decision

| Requirement | Decision | Why |
| --- | --- | --- |
| Map contains independently mutable Map/Array | one object table with immutable child IDs | Concurrent descendants merge without replacing a recursive JSON blob. |
| Array ordering | RGA positions with immutable parents and deletion tombstones | Duplicate and out-of-order delivery converge deterministically. |
| Child identity | its integration operation's HLC tag | One parent only; aliases, cycles, and in-place moves are rejected. |
| Large content / permissions | `SubdocumentRef` metadata only | Content lives in a separately authenticated manifest and can be loaded lazily. |
| Existing groups | new TypeIDs `27/28`, semantic version `1` | Existing single-type manifests remain unchanged and no peer silently upgrades. |

Yjs follows the same high-level ownership constraint: an integrated shared type
can occur only once, and its subdocuments are independently synchronized by
providers. This implementation borrows that product boundary, not Yjs's wire
format or provider API. See [Yjs shared types](https://docs.yjs.dev/getting-started/working-with-shared-types) and [Yjs subdocuments](https://docs.yjs.dev/api/subdocuments).

## Correctness model

The state is the union of four immutable/monotonic record sets:

```text
root name --LWW creation tag--> Map | Array object
Map object --LWW key tag--> bytes | child object ID | subdocument ID
Array object --RGA position--> bytes | child object ID | subdocument ID
Array object --tombstone--> deleted RGA position
```

A child declaration embeds its exact owner: root name, map `{object,key}`, or
array `{object,position}`. The reference that installs it must carry the same
tag as the child ID. Consequently an existing child cannot be reinserted at a
second key or array position. Moving remains intentionally absent; model it as
copy/create plus deleting the old visible reference until a separately proven
move semantic is needed.

Received deltas can arrive before a root, target object, child declaration, or
array parent position. Those records are retained only within explicit pending
operation and byte limits. Complete state frames and snapshots reject any such
unresolved state; recovery cannot silently drop dependencies.

## Subdocuments

`SubdocumentRef{ID}` is replicated structural metadata. `Document.Subdocuments`
returns the currently reachable, de-duplicated IDs. `Registry` is local,
ephemeral lifecycle state:

1. call `Registry.Sync(parent)` after applying an accepted parent update;
2. call `Load(id)` to request an independently authorized provider/group;
3. call `Unload(id)` to release local bindings and memory.

Neither `Load` nor `Unload` enters parent frames. A subdocument ID is not an
access token, room authorization, proof of durable storage, or a mandate to
autoload. The host must authenticate the exact parent group and the separate
subdocument manifest, authorize both directions of the relation, apply quotas,
and persist each document's state/HLC/frontier/outbox atomically. A parent
reference can be deleted without deleting an independently retained child.

## Resource and security controls

`Options` bounds roots, objects, map records, array nodes/tombstones, depth,
keys, scalar values, subdocument IDs, pending operations, and pending bytes.
The decoder additionally enforces outer frame/payload/string/element/tag
limits, canonical field order, shortest varints, exact frame TypeID, empty
codec ID, and CRC-32C.

All validation occurs against a candidate state before a remote delta changes
the document or witnesses its HLC. It rejects conflicting tag reuse, wrong
container kind, a child reference whose owner does not match, duplicate array
position with different contents, cyclic object/array parents, malformed
values, and all capacity excess. CRC and TypeID are format checks only; they
never authenticate an attacker.

## Performance and operational trade-offs

The first implementation favors atomic validation and auditable canonical
records over a hidden mutable object graph. It has deterministic projections,
bounded pending work, no callback while holding a document lock, and a linear
three-colour graph validation pass. Candidate records use copy-on-write per
section/object target, so a card-field update does not copy unrelated cards.
Groups should still split large, independently accessed content into
subdocuments rather than use one unbounded parent. The included
`BenchmarkDocumentTreeKanbanFrame` measures a 128-card nested workboard and
framed application at a second replica; it is a development indicator, not a
production capacity promise.

The release test matrix covers public nested operations, reverse/duplicate
delivery, pending-parent recovery, atomic rejection, canonical frame
round-trip, fuzzing, snapshot recovery, race detection, and the workboard
benchmark. A production rollout still needs provider-level load tests with
real authorization, persistence, network loss, and tenant quotas.
