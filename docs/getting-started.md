# Developer getting-started guide

[English](getting-started.md) | [简体中文](getting-started.zh-CN.md)

This is the shortest safe path from a new checkout to a replicated feature. It
starts with a stable CRDT and an executable example, then points to the
additional work required at a real network boundary. It does not turn the
library or its examples into a hosted synchronization service.

## 1. Run one complete, safe delivery flow

The module language baseline is Go 1.21. In an existing Go application, add the
latest stable module first:

```sh
go get github.com/im10furry/crdt@latest
```

Then inspect and run the smallest complete reference from a local checkout:

```sh
git clone https://github.com/im10furry/crdt.git
cd crdt
go version
(cd examples && go run ./getting-started)
(cd examples && go test ./getting-started)
```

Expected output:

```text
left=5
right=5
converged=true
```

This example uses the stable G-Counter protocol only. It keeps the local
mutation, encoded outbox record, bounded receive decoder, and `ApplyDelta`
separate so the application boundary is visible. It intentionally delivers one
record twice; the two replicas still converge. The small limits are teaching
values, not a production capacity recommendation.

If you are changing the library rather than integrating it, validate the whole
checkout separately:

```sh
go test ./...
```

## 2. Choose the data type before writing transport code

Choose the merge rule that already matches the business fact. A CRDT makes a
declared merge rule converge; it cannot make an unsuitable rule enforce a
business invariant.

| Need | Start with | Important rule |
| --- | --- | --- |
| A value that only increases, such as completed jobs | `counter.GCounter` | No decrement or reset. Each replica contributes its own component. |
| An eventually consistent signed total | `counter.PNCounter` | It supports increments and decrements, but does not prevent a concurrent overspend. Keep balances and reservations authoritative. |
| A fact that is never deleted | `set.GSet[T]` | The element codec is part of the wire contract and must be deterministic. |
| Membership with offline add/remove operations | `set.ORSet[T]` | Concurrent add and remove are add-wins. Persist its HLC-bearing snapshot before reusing an ID. |
| A field whose concurrent values must remain visible | `register.MVRegister` | Read `Values()` and resolve concurrent values in the product; a wall clock does not choose a winner. |
| A balance, exclusive booking, workflow transition, or access decision | An authoritative service | Do not treat eventual CRDT convergence as an invariant or authorization mechanism. |

Start with an in-memory merge while defining business semantics. The root
[`example_test.go`](../example_test.go) contains executable minimal references
for `ExampleGCounter`, `ExamplePNCounter`, `ExampleORSet`, `ExampleGSet`, and
`ExampleMVRegister`. Package documentation also contains focused Examples for
the bounded G-Counter receive path, experimental RGA delivery, and a manifest:

```sh
go test -run '^Example(GCounter|PNCounter|ORSet|GSet|MVRegister)$' .
go test ./counter ./text ./replica
```

After the minimal flow, the collaborative-board example is the recommended
realistic scenario. It uses bounded G-Counter and OR-Set decoders, deliberately
delivers duplicates and an out-of-order update, models an add-wins conflict
during a partition, then restores an OR-Set with the same replica ID from a
snapshot:

```sh
(cd examples && go run ./collaborative-board)
```

Its expected final state is:

```text
completed-inspections=5
open-tasks=[close-shift inspect-pump replace-filter]
```

For a framed G-Set and MV-Register flow, including duplicate delivery and
recovery, run:

```sh
(cd examples && go run ./warehouse-replication)
```

## 3. Follow the local-mutation and remote-delivery split

Every replicated mutation has two distinct responsibilities:

1. Mutate the local CRDT and retain the returned typed delta.
2. Encode that delta for the outbox, and at a receiver decode it with a bounded
   type-specific decoder before calling `ApplyDelta`.

For example, the local G-Counter mutation is intentionally small:

```go
local, err := counter.NewGCounter("warehouse-a")
if err != nil {
	return err
}
delta, err := local.Increment(1)
if err != nil {
	return err
}
encoded, err := delta.MarshalBinary()
if err != nil {
	return err
}
```

At the receive boundary, use limits derived from the transport and product
budget, not an unbounded allocation. The concrete decoder validates the full
canonical frame before `ApplyDelta` can mutate state:

```go
limits := frame.DecoderLimits{
	MaxFrameBytes:  64 << 10,
	MaxPayload:     60 << 10,
	MaxCodecID:     128,
	MaxElements:    1024,
	MaxTags:        2048,
	MaxStringBytes: 1024,
}
received, err := counter.UnmarshalGCounterDeltaWithLimits(encoded, limits)
if err != nil {
	return err // reject without changing local CRDT state
}
if err := remote.ApplyDelta(received); err != nil {
	return err
}
```

The returned frame checksum detects accidental corruption only. Authenticate
and authorize the sender before this step, impose the HTTP/WebSocket body limit
before allocating the byte slice, and persist a mutation/outbox record before
retrying it. The [end-to-end integration tutorial](integration/overview.md)
shows the complete local delivery exercise; the [WebSocket provider
reference](integration/websocket-provider.md) shows a bounded, manifest-bound
Go integration pattern. The complete compilable G-Counter version of this
split is [`examples/getting-started`](../examples/getting-started); copy its
shape, not its example limits.

## 4. Persist the complete recovery record

An encoded CRDT state is not always enough to safely restart a replica with the
same ID. Store the complete snapshot together with the surrounding replication
frontier and application-owned delivery metadata in one durable transaction.

| Type | Persist and restore | Why |
| --- | --- | --- |
| `set.ORSet` | `SnapshotCurrentState()` and `set.NewORSetFromSnapshot` | The snapshot includes the HLC state needed to create a new unique mutation tag. |
| `register.MVRegister` | `Snapshot()` and `register.NewMVRegisterFromSnapshot` | The causal version vector prevents an ID from reusing an existing dot. |
| HLC-backed LWW, RGA, OR-Tree, attachment reference | Their `SnapshotCurrentState()` equivalent and matching `New...FromSnapshot` constructor | State, clock, and retained tombstone metadata are one recovery unit. |

Do not reconstruct an OR-Set or another HLC-backed type by restoring only
`MarshalBinary()` bytes and then reuse its replica ID. A fresh logical replica
needs a new globally unique ID; a same-ID restart needs its saved clock/context.

## 5. Select one protocol per replication group

Bind a replication group to an authenticated `replica.Manifest`: group, schema,
epoch, concrete state/delta frame IDs, codec ID, and semantic version must all
match before a peer can send data. `ProtocolPolicy` is a local capability list,
not a global switch and not an authentication mechanism.

New Go RGA groups need special attention:

- `crdt.DefaultRGAFrameType()` selects compact run-v2 frames 19/20.
- The frame type alone does not change an encoder: use `Delta.MarshalRunBinary`
  for deltas and `RGA.MarshalRunBinary` / `RGA.SnapshotRunCurrentState` for
  complete state and recovery.
- The default browser/JavaScript Wasm artifact accepts run-v2 frames 19/20 and
  semantics version 2. `make wasm-v1` remains available only for an explicitly
  negotiated legacy-v1 migration group.
- A manifest binds one exact frame pair. Native clients without Wasm must follow
  the [RGA run-v2 wire protocol](protocol/rga-run-v2.md) and its canonical
  vectors; never silently reinterpret a v1 frame as run-v2 or vice versa.

LWW-Set, LWW-Map, legacy RGA v1, and generic RGA list are stable and bind their
own exact frame pair and semantics version at every manifest, change, inbox,
checkpoint, and session boundary. Stable observed-remove tree v1 binds `tree.SemanticsVersion`
and an exact node-value schema under the zero-value policy; it supports only
immutable-parent add/remove, not in-place moves. Read the
[collection extension design](design/crdt-extension.md) and
[OR-Tree v1 protocol](protocol/or-tree-v1.md) before choosing one. The
[TypeScript/Wasm client guide](../clients/typescript/README.md) has its browser
deployment, persistence, and CSP requirements.

## 6. Use the appropriate next guide

| Goal | Next step |
| --- | --- |
| Learn two-replica delivery, recovery, and anti-entropy | [End-to-end integration tutorial](integration/overview.md) |
| Add a WebSocket endpoint and Go client | [WebSocket provider reference](integration/websocket-provider.md) |
| Replicate immutable media/file references | [Attachment reference integration](integration/attachment.md) |
| Build browser or WebView local RGA merge | [TypeScript/Wasm client guide](../clients/typescript/README.md) |
| Implement a non-Wasm RGA client | [RGA run-v2 wire protocol](protocol/rga-run-v2.md) |
| Design a new collection or assess protocol maturity | [Collection extension design](design/crdt-extension.md) |
| Work on the library itself | [Contributing guide](../CONTRIBUTING.md) |

## 7. Before calling an integration production-ready

Passing the examples proves library behavior at this revision. It does not
prove a deployment. Before production, independently verify:

- TLS, identity, authorization, tenant/group isolation, rate limits, and
  request/body limits;
- durable mutation/outbox, actor counters, CRDT state, frontier, snapshots,
  retries, and bootstrap/anti-entropy recovery;
- a trusted membership authority and exact epoch-bound acknowledgements before
  any tombstone compaction; and
- duplicate, reordered, offline, reconnect, overload, restore, and
  adversarial-input behavior against the limits selected for the real workload.

Run the checks that match the scope of your change. The library's local gates
are documented in the [contributing guide](../CONTRIBUTING.md); `make verify`
also needs the pinned static-analysis tools available on `PATH`.
