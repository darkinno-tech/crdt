# Native TypeScript shared types: architecture decision

## Decision

Add a dependency-free `native-ts-v1` document layer for browser/WebView
structured state: `NativeDocument`, LWW `NativeMap`, and RGA `NativeArray`.
Keep it explicitly separate from the existing canonical Go frame and RGA
run-v2 contracts. This is a new opt-in client-to-client protocol, not a new Go
TypeID and not a transparent fallback for a Go replication group.

## Evidence and context

At the decision point, `clients/typescript/src` contained 797 lines: the
bounded common-frame decoder (`frame.ts`, 264 lines), the Go/Wasm RGA wrapper
(`wasm.ts`, 507 lines), and exports (`index.ts`, 26 lines). It had no native
merge engine or browser shared map/array abstraction.

The existing run-v2 specification binds TypeIDs 19/20, semantic version 2, an
authenticated manifest, canonical byte encoding, HLC recovery, tombstones, and
resource limits. Reinterpreting any of those fields in TypeScript would break
the contract. The existing Wasm route remains the correct choice for Go/RGA
interoperability.

The native API deliberately takes the useful interaction model from mature
shared-type clients: named maps/arrays, transactions, post-transaction
observers, transport-neutral updates, duplicate tolerance, and shuffled
delivery convergence. It does not copy their wire format or claim API
compatibility.

## Options considered

| Option | Startup/size | Cross-language compatibility | Correctness risk | Decision |
| --- | --- | --- | --- | --- |
| Keep only Go/Wasm RGA | Requires Wasm fetch/compile | Exact Go RGA | Lowest | Retained for RGA groups |
| Reimplement Go run-v2 in TS | Avoids Wasm | Possible only after full conformance work | High: frame, HLC, pending, tombstone, snapshot drift | Deferred |
| Add native TS structured protocol | No Wasm dependency | None by design | Contained by a distinct contract | Selected |
| Add a new Go frame TypeID immediately | Would need Go codec/manifest/policy/recovery work | Potentially yes | Too broad without a server-side structured schema | Not selected |

## Native protocol invariants

- An update has exact `version`, `actor`, and operation fields, is canonical
  UTF-8 JSON on byte transports, and is limited before parse/copy work.
- Map writes carry immutable `{ actor, counter }` IDs. The greatest counter,
  then bytewise UTF-8 actor ID, wins. Current equal-ID payload/key conflicts
  are rejected atomically.
- Array entries are immutable `(id, after, value)` nodes. The graph must be
  acyclic; rendering is deterministic depth-first traversal with descending
  sibling IDs. Delete tombstones win even when delivered before insertion.
- An incoming update is fully normalized, type-checked, conflict-checked, and
  capacity-checked before it mutates roots, map entries, nodes, tombstones,
  pending queues, or the local counter.
- Reusing a replica ID requires persisting and restoring root declarations,
  complete state updates, and the local counter in one atomic operation.

## Resource and security boundaries

Defaults bound a raw update to 1 MiB, operations to 10,000, root types to 128,
map entries to 10,000, array nodes/tombstones to 100,000 each, unresolved array
nodes to 10,000, values to 64 KiB, and nested values to depth 32/10,000 items.
The limits are deployment policy and must be equal or lower at every peer.

The protocol validates shape, resource use, and deterministic merge semantics;
it does not authenticate peers, authorize a document, encrypt data, stop a
replay at the transport level, or guarantee durable delivery. Hosts must bind
an authenticated group/schema/version, cap request bodies before allocation,
enforce access control, and own outbox/retry/replay retention.

Values are copied JSON values and are atomic. Nested JavaScript objects are not
live shared types; applications use separately named root maps/arrays for data
requiring independent concurrent merge.

## Performance model

`NativeMap` lookup/update is amortized O(1) plus value-copy cost. `NativeArray`
is optimized for batch append and convergence: resolving an index projects the
visible sequence, so `insert(index, ...)`, `get`, and `toArray` are O(n) in
retained nodes. Child lists are sorted only when projected, which avoids
re-sorting a batch of inserts. Large arrays should live in a Worker and edits
should be grouped with `transact`.

State export splits canonical JSON by incrementally counted bytes. It must not
serialize an ever-growing candidate update per array entry: that causes O(n²)
time and makes the 100,000-node retained-state limit unusable. The benchmark
exercises 4,096-item append/merge, middle insert/merge, shuffled duplicate
three-editor delivery, and state encode/recovery.

## Validation plan

- Unit tests: copied values, transactions/observers, LWW ties, RGA ordering,
  delete-before-insert, pending-parent resolution, type/tag/cycle conflicts,
  configured capacity rejection, canonical decoding, and snapshot counters.
- Robustness: 600 deterministic malformed-byte samples must either decode to a
  valid update or throw `NativeCRDTError`, never a runtime exception.
- Simulation: three offline editors make 180 deterministic divergent changes;
  shuffled delivery plus duplicate delivery must converge maps/arrays with no
  pending nodes. A recovered same-actor document must allocate a larger ID.
- Performance: run `make typescript-native-benchmark`; report the machine and
  runtime as controlled measurements, not mobile production capacity.
