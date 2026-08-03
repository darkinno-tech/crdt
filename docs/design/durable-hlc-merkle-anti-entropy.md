# Durable HLC/Merkle anti-entropy

## Decision

`crdt-durable-v3` is the opt-in recovery protocol for applications that keep a
durable event inventory and do not want to transmit a `replica.Frontier` state
vector. It is enabled only when the durable log implements `durable.MerkleLog`
and explicitly persists a relay HLC. The bundled bbolt Store enables it with a
stable `StoreConfig.HLCReplicaID`.

```go
store, err := durable.OpenStore("/srv/crdt/relay.db", durable.StoreConfig{
	MaxEvents:    100_000,
	MaxBytes:     256 << 20,
	HLCReplicaID: "relay-eu-1", // stable for this store file
})
```

The relay allocates its HLC tag in the same bbolt transaction as the event
sequence, Dot binding, capacity accounting, HLC-to-sequence index, and next
HLC state. It is a **relay event identity**, not an application mutation tag:
the tag does not alter CRDT merge semantics or replace the HLC/causal metadata
inside an HLC-backed delta.

`crdt-durable-v1` cursor replay and the legacy optional
`crdt-durable-v2` state-vector path remain available for compatible existing
clients. A group that contains log entries written before relay HLC persistence
cannot safely advertise a complete v3 history; it must use v1/v2 or bootstrap
from a validated checkpoint.

## Two-phase, no-state-vector session

```text
persisted local event inventory root ── merkle hello(root) ──> relay
                                                        │ authenticate + authorize
                                  snapshot HLC/root/high-water H; register peer
                                                        │
client <───────────── merkle welcome(root,H) ──────────┘
  │ root equal: persist boundary, begin live H+1 …
  │ root differs:
  │
  ├─ receive complete, bounded remote leaf inventory
  ├─ compare durable inventories; reject local-only/digest-conflicting leaves
  ├─ request only absent sorted relay-HLC identities
  ├─ install returned events and rebuild the local root
  └─ verify root; atomically persist boundary(root,H), then begin live H+1 …
```

The relay adds the peer while holding the group lock that snapshots `H`. A
later append enters the peer's bounded queue and is sent only after the
`complete(root,H)` boundary. Thus a repair does not have a replay-to-live gap.
The client does not advance its cursor to `H` until `OnMerkleCatchUp` succeeds.

`MerkleIndex` is an in-process implementation aid for leaf comparison. It is
not persistence. Hosts must persist an equivalent inventory beside their
concrete CRDT state, delivery frontier/outbox, and any local CRDT HLC state;
rebuild the index from that durable event inventory before reconnecting.

```go
client, err := durable.NewReconnectClient(endpoint, manifest, durable.ClientConfig{
	MerkleRoot:      persistedIndex.Root,
	ReconcileMerkle: persistedIndex.Reconcile,
	OnEvent: func(event durable.Event) error {
		// In one application transaction: validate/apply the concrete delta,
		// persist the CRDT state/frontier and the relay-HLC leaf identity.
		// Then update the in-memory mirror only after that transaction commits.
		return installAndPersist(event)
	},
	OnMerkleCatchUp: func(boundary durable.MerkleBoundary) error {
		// Atomically record root, high-water cursor, and the completed state.
		return checkpointMerkleBoundary(boundary)
	},
})
```

Do not combine `StateVector` and the three Merkle callbacks in one client
configuration. A client configured for the Merkle callbacks offers **only**
v3; it fails closed when a server cannot select v3 rather than silently
downgrading the recovery proof to a cursor. A server that selects v3 but cannot
return a complete inventory fails with `ErrAntiEntropyUnavailable` rather than
returning a partial repair. v1/v2 callers continue to use their established
compatibility paths.

## Correctness and security boundaries

- A root is a SHA-256 integrity commitment over the relay HLC identity and
  canonical durable change envelope. It detects a mismatch; it is neither peer
  authentication, membership proof, receipt, authorization, nor tombstone-GC
  permission.
- Authentication and `AuthorizeSubscription` run before any inventory is
  returned. Write authorization still binds the authenticated peer to the
  CRDT Dot. TLS, tenant isolation, rate limits, and retention policy remain
  host responsibilities.
- All controls have the existing 16 KiB frame cap and strict JSON parsing.
  Inventory and request controls are chunked; global `MaxMerkleLeaves` and
  `MaxMerkleBytes` limits are validated before allocation. Replay event/byte,
  actor-byte, queue, and whole-message limits still apply.
- The relay reserves the HLC envelope before accepting a v3-capable durable
  publish. It therefore cannot retain a valid v1-sized event that later cannot
  fit in a v3 message.
- A client whose durable inventory has a local-only leaf or a matching HLC
  with another digest receives `ErrMerkleDiverged`. It must investigate or
  bootstrap; it must never delete history merely to make the roots equal.
- A matching event inventory means only that deterministic CRDT replay has
  the same inputs. Application checkpoint validation, CRDT decoder bounds,
  authentication, and the normal durable-state transaction remain mandatory.

## Performance model

The equal-root network path is one hello, one welcome, and one completion; it
does not transmit actor-count-sized state vectors or historical event payloads.
For a mismatch, the current deliberately auditable baseline compares a
complete bounded flat leaf inventory and transfers only missing events. Its
network inventory cost is `O(N)` leaves and its in-memory comparison is `O(N)`;
the bbolt reference rebuilds its root from validated retained events, so the
first implementation also has an `O(N log N)` local root construction cost.

This is not a claim that v3 dominates a compact state vector for every group.
Measure actor cardinality, retained event count, mismatch rate, event size,
disk latency, and RTT. Add paged subtree/multiproof exchange only when those
measurements demonstrate that the bounded flat inventory is the bottleneck.

## Validation

| Dimension | Evidence |
| --- | --- |
| Correctness | Atomic relay-HLC restart test; root/inventory reconstruction; missing-event repair followed by live delivery; root verification before boundary persistence. |
| Security/resources | Strict v3 control/event decoding, canonical ordering, bounded chunks and requests, HLC envelope reservation, and fail-closed local-only/digest-conflict tests. |
| Concurrency/recovery | Group-lock registration before live fan-out, reconnect client checkpoint boundary, `go test -race ./durable`. |
| Simulation | Existing three-replica partition/recovery tests plus v3 loopback missing-history repair. |
| Performance | `BenchmarkMerkleInventoryReconcile` (equal and sparse 256/4096 leaves) and `BenchmarkMerkleLoopbackSession` (real bbolt + WebSocket equal-root/sparse-repair). |

Run focused validation from the repository root:

```sh
(cd durable && go test .)
(cd durable && go test -race .)
(cd durable && go test -run='^$' -fuzz=FuzzWire -fuzztime=250000x -parallel=1 .)
(cd durable && go test -run='^$' -bench='Merkle' -benchmem -count=3 -cpu=1,4 .)
```

These are controlled local checks. They do not prove a production TLS/identity
deployment, mobile/browser persistence transaction, WAN loss behaviour,
external store configuration, or production capacity.
