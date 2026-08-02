# Document-tree v2: fully nested collaboration within one boundary

`documenttree` is the Go framed CRDT for a bounded collaborative object graph.
Version 2 intentionally makes the tree complete: every reachable Map and Array
uses the same replication contract and is present in a complete state frame.
It is a new protocol, not an adapter for
`lww.Map`, `list.RGA`, TypeScript `native-ts-nested-v1`, Yjs, or document-tree
v1.

## Decision

| Requirement | Decision | Why |
| --- | --- | --- |
| Nested mutable structures | One bounded object table of Map and Array objects | A child converges independently without recursive-JSON replacement. |
| Child identity | The creation operation's HLC tag | An object has one owner; aliases, cycles, and in-place moves are rejected. |
| Loading and snapshot | Full reachable tree in one group | A received state/checkpoint is complete; no metadata can claim content that is absent. |
| Authorization and retention | One boundary for the whole tree | A pointer must never smuggle a separately authorized group across an authenticated manifest. |
| Protocol evolution | TypeIDs `31/32`, semantic version `2` | Published v1 `27/28` keeps its old meaning and no peer silently upgrades. |

Yjs has a different product feature: providers may use a subdocument GUID to
lazy-load independently synchronized content. The [Yjs subdocument
documentation](https://docs.yjs.dev/api/subdocuments) confirms that providers
own that sync behavior and that unloaded content is initially empty. This
protocol deliberately does not adopt that behavior: it borrows only the
single-integration ownership lesson from [Yjs shared
types](https://docs.yjs.dev/getting-started/working-with-shared-types), not the
Yjs wire format, provider API, or document identity model.

## Correctness and security model

```text
root name --LWW creation tag--> Map | Array object
Map object --LWW key tag--> bytes | owned object ID
Array object --RGA position--> bytes | owned object ID
Array object --tombstone--> deleted RGA position
```

A child declaration embeds its exact owner: root name, map `{object,key}`, or
array `{object,position}`. The reference that installs it carries the same tag
as the child ID. An existing child therefore cannot be inserted at a second key
or array position. A move is deliberately represented as creating a new child
and deleting the visible old reference until a separately versioned move proof
exists.

The decoder validates a candidate state before it mutates the receiver or
witnesses its HLC. It bounds transport bytes, roots, objects, map records,
array nodes/tombstones, depth, scalar bytes, pending operation count, and
pending bytes; it also requires canonical framing, field order, shortest
varints, exact TypeID, empty codec ID, and CRC-32C. It rejects conflicting tag
reuse, wrong container type, an owner mismatch, duplicate RGA contents, cycles,
and capacity excess. CRC and a TypeID do not authenticate a sender.

One manifest authorization covers every object in the tree. A product that
needs per-page permissions, separate history/retention, or on-demand memory
loading must use distinct groups and explicitly authorize their application
relationship. It must not add an opaque identifier inside a v2 value and claim
that the target content is part of this snapshot.

## Performance and operational trade-offs

The full-tree decision removes provider lifecycle and cross-group synchronization
round trips, and makes snapshot/recovery atomic and auditable. Its cost is that
the configured group retains and transfers every reachable object. `Options`
is therefore a safety contract, not merely a tuning suggestion; choose it from
the largest permitted complete business object, not an average document.

Candidate updates remain copy-on-write by section/object target, so a card
field update does not copy unrelated cards. `BenchmarkDocumentTreeKanbanFrame`
models a 128-card, deeply nested workboard and measures a local field update,
canonical frame encoding, decode, and application at a second offline replica.
It is a development indicator, not a provider or production capacity claim.

The release matrix covers nested three-replica duplicate/reordered delivery,
pending-parent recovery, type/alias rejection without mutation, v1 cutover
rejection and offline migration, canonical round trips, fuzzing, snapshot
recovery, race detection, and the workboard benchmark. A production rollout
still needs real provider load tests with its authorization path, persistence,
network loss, reconnect, tenant quotas, and the actual maximum tree shape.
