# Local multi-type undo/redo and version DAG

This design adds a local `history` layer without changing any replicated CRDT
frame, TypeID, manifest, HLC rule, or merge rule. It addresses three distinct
needs which must not be conflated:

1. a user-facing undo/redo stack spanning more than one local type;
2. durable recovery of that local stack after a restart; and
3. browseable, branchable document versions.

`history` is local metadata. It is neither a new CRDT nor a replacement for an
authenticated transport, application transaction, durable outbox, or a
type-specific CRDT merge.

## Evidence and decision

Yjs scopes `Y.UndoManager` to one or more shared types and their descendants;
it captures local transactions and replays compensating changes. Automerge
retains a content-addressed change DAG, exposing history, forks, and merges.
Neither model makes an existing CRDT tombstone safely reversible, and neither
checksum authenticates a peer.

| Requirement | Rejected shortcut | Chosen boundary |
| --- | --- | --- |
| Cross-type undo | One `UndoManager` per type plus a UI-level best effort stack | One local manager orders named scopes; each scope executor owns its concrete inverse semantics. |
| Correct redo | Re-send the original delta and revive old IDs | Executor returns the next compensating command after every apply, so redo can allocate fresh tags. |
| Restart | Serialize closures, pointers, or a CRDT's private maps | Persist a bounded, canonical command stack beside the matching CRDT checkpoint. |
| Version browsing | Treat a current state frame as a global log | Persist a content-addressed DAG of complete, named snapshots. |
| Branch merge | Byte-concatenate frames or let metadata guess conflict rules | Materialize the merge with each actual CRDT, then record the resulting snapshots with two parents. |

This keeps current wire compatibility intact. It also avoids claiming that a
generic library can atomically roll back independent CRDT instances after a
partial host failure.

## Architecture

```text
local typed mutation ----> scope Executor ----> CRDT compensating delta ----> outbox
       |                         |                       |
       |                         +-- reverse command ----+
       v
history.Manager (undo / redo stacks, bounded and serializable)
       |
       +---- same host transaction ----> CRDT snapshot + HLC + outbox + history bytes

materialized named snapshots ---> history.Repository ---> branch heads / version DAG
```

`history.Manager` receives an explicit scope and opaque semantic command. The
host executor validates the command, applies it to its list, tree, rich-text,
or another concrete CRDT, emits the canonical local delta, and returns the
command that reverses exactly that newly applied mutation. Remote application
must bypass `Manager.Execute`; otherwise a user could undo another actor's
work.

The manager intentionally serializes executions but does not hold its state
mutex while calling the executor. A host executor must satisfy its normal
CRDT atomicity contract: on an error it leaves its scope unchanged. One manager
entry is one scope operation. If a product needs an atomic business action over
several scopes, it must define a host transaction/outbox protocol before
capturing the individual operations; this library does not claim a distributed
multi-document transaction.

## Persistence contract

The `MarshalBinary` output of `history.Manager` and `history.Repository` is
canonical, SHA-256 damage-detected, versioned local metadata. It is not a
replication envelope and MUST NOT be sent to peers. SHA-256 detects accidental
damage only; it does not authenticate, authorize, encrypt, or redact a record.

Persist a CRDT's complete snapshot, HLC state/frontier, outbox, and matching
manager/repository bytes in one host transaction. On startup restore the CRDT
first, then restore the history object with the executor bound to the same
scopes. If their checkpoint generations differ, discard the local undo stack
and restore a consistent version snapshot rather than applying commands to an
unrelated state.

Version records hold one or more named `snapshot.Snapshot` values. The
repository validates the public frame envelope, state/codec identity,
frontier ordering, HLC presence, resource limits, canonical order, parent
existence, and content address before accepting persisted bytes. Concrete
payload validation remains the responsibility of the type-specific snapshot
constructor at the point the host creates or restores it.

## Safety, correctness, and retention

- Compensating undo/redo operations are new CRDT mutations. They do not rewind
  global state, erase remote changes, remove tombstones, or make an old tag
  live again.
- A scope executor must fail closed when an RGA predecessor, tree parent, or
  another structural dependency has been compacted. It must never substitute a
  current offset for the original stable intent.
- `history.Manager` bounds entries, scopes, commands, emitted bytes, and its
  serialized record before allocating retained history. `history.Repository`
  separately bounds versions, branch names, parents, named snapshots, state
  bytes, frontier entries, replica IDs, and the complete persisted record.
- Treat commands and stored metadata as sensitive application data: protect
  database/file permissions, encrypt at rest where required, authorize version
  reads, bound retained history, and purge according to product policy.
- Compacting a CRDT can invalidate old undo anchors. Clear the affected local
  undo manager at the same post-compaction checkpoint boundary; the version
  DAG may retain only snapshots whose application retention policy permits it.

## Performance model

Steady-state manager execution is `O(1)` stack work plus the executor's
type-specific mutation and delta encoding. Undo/redo retain only two bounded
commands per entry. Serializing a manager is `O(entries + command bytes)`.

Repository commit hashes canonical parents and snapshots. It is proportional to
the recorded snapshot bytes, not to all historical CRDT deltas. This favors
predictable browsing and bounded recovery, but a host choosing a commit for
every keystroke must budget disk and encode time. The repository deliberately
does not compact or deduplicate CRDT internals; use a product checkpoint and
retention policy after measuring real edit, branch, and restore traffic.

## Verification

The package tests cover cross-scope stack ordering, dynamically replaced undo
and redo commands, restart recovery, malformed/corrupt local records, bounded
allocation admission, executor panic containment, and concurrent callers.
The version test runs three RGA replicas through independent branch edits,
shuffled duplicate delivery, convergence, a two-parent merge commit, binary
repository restore, and concrete snapshot materialization.

```sh
go test ./history -count=1
go test -race ./history -count=1
go test -run=^$ -fuzz=FuzzManagerUnmarshal -fuzztime=250000x -parallel=1 ./history
go test -run=^$ -fuzz=FuzzRepositoryUnmarshal -fuzztime=250000x -parallel=1 ./history
go test -run='^$' -bench='Benchmark(ManagerExecuteUndo|RepositoryCommitSnapshot)$' -benchmem ./history
```

These are controlled library checks. They do not prove a browser refresh,
database transaction, production authorization, or real remote retention
policy; verify those in the consuming product.
