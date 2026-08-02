# Tombstone collection modes

`tombstonegc.Coordinator` remains the default tombstone collector. It is the
only mode suitable for replicated or durable CRDT data: every active member
must acknowledge each exact tag in the current authenticated membership epoch,
the compacted state must be durably checkpointed, and old deltas must be
retired before a member can rejoin.

`tombstonegc.SimpleCollector` is a deliberate local-only exception for
disposable, single-authority state, such as a recommendation cache or a
server-derived default that is rebuilt from current source data. It removes
receipt bookkeeping, membership traffic, and per-member acknowledgement
storage. It does not weaken the replicated protocol or change any frame.

## Select a mode

| Concern | `Coordinator` (default) | `SimpleCollector` (explicit opt-in) |
| --- | --- | --- |
| Appropriate data | Shared, durable, offline-capable, or business-critical CRDT state | Disposable local caches and rebuildable server-owned derived state |
| Evidence before collection | Authenticated exact tags from every active member in one epoch | The host proves that no delayed operation, outbox item, backup restore, or rejoining replica can merge this state |
| Correctness after delayed delivery | Delayed adds cannot resurrect a collected delete | A delayed add can resurrect; this is why replicated state is forbidden |
| Security boundary | Host authenticates membership and receipts; the coordinator fences old epochs | No membership or receipt authentication exists; it conveys no peer trust or authorization |
| Per-cleanup resource shape | Exact acknowledgement state grows with member/tag coverage until checkpoint pruning | No acknowledgement state; `MaxBatch` caps selected compacted identities, although target enumeration is still target-defined |
| Structural CRDTs | Uses an eligible target compactor after exact proof | Uses the same structural checks, so pending/non-leaf anchors remain retained; that does not create replication proof |

Do not choose the simple mode merely because the active member list happens to
contain one replica. A durable outbox, a future second device, a replayable
log, or a backup that can restore older CRDT state is enough to require the
strict mode.

## Local-only workflow

First establish and document the local-only lifecycle: all older operations
are discarded with the target, and recovery rebuilds from the authoritative
source rather than merging a historical CRDT delta. Then select an explicit
bounded policy.

```go
collector, err := tombstonegc.NewSimpleCollector(tombstonegc.DefaultSimplePolicy())
if err != nil {
	return err
}

// recommendationCache is local, disposable state. It never accepts remote or
// replayed CRDT deltas, and it is rebuilt rather than merged after restart.
removed, err := collector.Collect(recommendationCache)
if err != nil {
	return err
}
metrics.RecordTombstonesCollected(removed)
```

`DefaultSimplePolicy` retains 256 tombstone identities and selects at most 64
per call. `MinRetained` is a count, not a time-based deletion-retention
guarantee: canonical CRDT tag order does not universally equal the time a
delete was observed. `MaxBatch` may be set from 1 through 8,192; it bounds the
compaction request but cannot avoid the target's own `TombstoneTags()`
enumeration work.

Run collection on a bounded maintenance path and monitor retained tombstone
count, removed count, call duration, errors, and rebuilds. If a target reports
an unresolved or non-leaf structural tombstone, preserve it and retry only
after the target's local state can make safe structural progress.

## Strict workflow remains unchanged

Use `Coordinator` whenever a CRDT crosses a process, device, durable log, or
backup boundary. The application must authenticate the membership view and
every receipt, persist the post-compaction checkpoint before pruning receipt
state, retire old deltas, and require a retired member to bootstrap from that
checkpoint. The [membership protocol](../protocol/membership.md) is the
transport-independent reference for that workflow.

Neither mode authenticates CRDT frames, encrypts data, establishes business
authorization, or turns a checksum/frontier/Merkle root into a deletion
receipt.
