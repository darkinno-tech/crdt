# Core receiver layered CPU benchmark — 2026-08-02

## Decision and boundary

This record covers four independent CPU/allocation changes rebased onto beta
commit `0446d0b` and measured before this document was added. The intervening
beta changes affected only TypeScript Yjs code and documentation; none touched
`tree`, `replica`, `merkle`, or `awareness` paths.

The goal was to remove work only when it had already been proved redundant:

| Module | Change | Proof boundary retained |
| --- | --- | --- |
| OR-Tree | Skip generic graph-state allocation for a closed delta forest whose every non-root edge points to an earlier in-batch tag; pre-size maps for an empty receiver's first accepted sync | Incomplete, cross-batch, or non-monotonic deltas use the original combined graph walk; capacity, conflict, clock and atomicity checks precede mutation |
| Replica Inbox | Mark a `NewChange*` frame privately validated after it has been copied and decoded once | Every Receive still checks Dot and manifest compatibility; package-internal/zero-value changes still fully decode and validate |
| Merkle | Snapshot key/digest pairs as a detached slice and reuse leaf/inner hash buffers | Read lock duration stays limited to snapshot copying; lexical key order and the existing leaf/inner domain-separator bytes are unchanged |
| Awareness | Treat an exact byte match to retained canonical online state as a heartbeat | Clock, TTL, expiry recovery, version and local event still advance; new, offline, or non-identical JSON uses full canonicalization |

No frame type, semantic version, public API, authorization decision, durable
receipt behavior, or resource limit was relaxed.

## Controlled measurement

Measurements ran with Go `go1.26.5`, darwin/arm64 (Apple M4 Pro), and
`GOMAXPROCS=4`. Each listed sample uses `-benchmem`, five one-second samples
unless noted. These are local comparison baselines, not a production capacity
claim.

| Scenario | Before | After | Interpretation |
| --- | --- | --- | --- |
| OR-Tree initial 100,000-node linear delta | 26.0–28.4 ms/op, about 40 MB/op, 816–817 alloc/op | 10.6–11.3 ms/op, about 12.6 MB/op, 262 alloc/op | The graph proof removes the generic cycle-state map; empty first-sync map sizing removes repeated map growth |
| Inbox installed duplicate | 159–162 ns/op, 8 B/op, 1 alloc/op | 130–131 ns/op, 0 B/op, 0 alloc/op | Private construction validation avoids reparsing a copied immutable frame |
| Inbox buffered 4 KiB duplicate | 786–919 ns/op, 4,096 B/op, 1 alloc/op | 191–192 ns/op, 0 B/op, 0 alloc/op | Same validation cache, while byte equality remains the dot-conflict test |
| Merkle root after one write, 128 entries | 26.8 us/op, 31,646 B/op, 143 alloc/op | 23.8–24.4 us/op, 16,253 B/op, 14 alloc/op | Detached compact snapshot and hash-buffer reuse preserve the canonical root vector |
| Awareness `Set` with exact retained canonical state | 1.22–1.36 us/op, 2,192 B/op, 35 alloc/op | 47–49 ns/op, 80 B/op, 1 alloc/op | Full JSON parse/marshal is skipped only for an exact stored canonical byte sequence; the remaining allocation is the caller-owned return copy |

With 64 local observers, awareness update cost remained about 15–16.5 us/op
and 8.57 KB/op. That fan-out/callback-snapshot path is intentionally separate
from the no-observer local heartbeat optimization.

## Validation matrix

| Layer | Evidence | Result |
| --- | --- | --- |
| Package correctness | `go test ./tree ./replica ./merkle ./awareness` after rebase | pass |
| Concurrency safety | `go test -race ./tree ./replica ./merkle ./awareness` | pass |
| Untrusted decode fuzzing | 15 s each: OR-Tree `FuzzORTreeUnmarshal` (about 3.48M executions), Inbox `FuzzInboxHandlesUntrustedChangesWithoutPanic` (about 3.07M), awareness `FuzzUnmarshalUpdate` (about 4.49M) | pass |
| Model/integration | OR-Tree three-editor shuffled delivery/recovery, Inbox duplicated out-of-order simulation, Merkle sync CLI tests, awareness TTL/observer tests | pass |
| Workspace gate | `make verify`, including generated TypeID check and all Go workspace modules | pass |

The existing two-host RGA probe is not reused as proof for these changes:
`crdt-sync-probe` has no OR-Tree, Inbox, Merkle, or awareness endpoint. A
future cross-host acceptance test must add those protocol-specific endpoints
and retain the same loopback-only/token-cleanup boundary.

## Deferred candidates

`document.DocManager` profiling delegated nearly all cost to MoveRGA's visible
projection and move merge. That path has deterministic concurrent-cycle repair
and identity-preserving suffix-splice semantics, so it was deliberately left
unchanged pending a separate equivalence model and concurrency benchmark.
Similarly, OR-Tree `Nodes` retains caller-owned deep copies by contract; its
read allocations are not treated as removable cache overhead.
