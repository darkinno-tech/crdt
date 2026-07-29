# Durable transport reference design

## Decision

`extensions` remains the deliberately limited, bounded live-relay adapter. It
must not grow hidden storage, retries, or a listener. This reference adds a
separate `durable` package for a **single-writer, single-storage-volume
WebSocket relay** with an append-only operation log, bounded replay, and a
reconnecting client.

The operation log—not an in-memory CRDT or a best-effort peer queue—is the
authority for transport recovery. A committed record has one server sequence
and an immutable binding from `(group, actor, counter)` to the canonical
transport bytes. A retry with the same bytes is idempotent; a retry with
different bytes is rejected permanently. The server broadcasts only after the
log transaction commits.

The reference deliberately does not turn a CRDT relay into an authority for
business invariants. The embedding application still owns concrete CRDT state,
atomic state/frontier/cursor checkpoints, identity lifecycle, membership, and
tombstone retirement.

## Scope and deployment boundary

The implementation is a deployable reference for one process owning one
`bbolt` file on a local persistent volume. `bbolt` provides serializable ACID
transactions and an exclusive file lock; it is therefore suitable for this
small, single-writer reference, but is not a clustered log or a replacement
for a replicated database. Run exactly one active writer for a data path. For
high availability, replace the `Store` with a durable consensus/database
adapter that preserves the same append and exact-dot-binding contract.

The server does not start a listener. It returns an `http.Handler` for a host
application to mount behind its own TLS termination, request limits, metrics,
tracing, and lifecycle management.

## Durable protocol

`crdt-durable-v1` is a new WebSocket subprotocol; it does not change
`extensions`' `crdt-sync-v1` live-relay wire contract.

1. The client connects over `wss`, authenticates in the HTTP upgrade, and
   sends one bounded JSON hello containing the exact `replica.Manifest` and
   the last server event sequence whose local state and cursor were made
   durable.
2. The server verifies the manifest and independent read permission, atomically
   snapshots its high-water sequence and registers the peer, then returns the
   compatible manifest and high-water sequence.
3. It replays every committed event after the supplied cursor in ascending
   server-sequence order, then drains live events from the same ordered peer
   queue. Registration before the hello response closes the replay-to-live
   gap.
4. Clients publish a canonical `replica.Change` envelope. The server applies
   authentication, actor authorization, manifest/policy/frame validation, and
   application-supplied bounded semantic validation before it appends.
5. A committed append is broadcast as a sequenced event. The reconnecting
   client invokes its durable receive callback first; only after it succeeds
   may it advance the resume cursor.

The server rejects a resume request whose missed history exceeds configured
event or byte limits. It never truncates a replay to make it fit. The client
must then obtain an application-authorized, validated checkpoint and resume
from the checkpoint's recorded server sequence.

## Store transaction and correctness invariants

One `bbolt` update transaction performs all of the following:

- checks the exact `(actor, counter)` binding;
- returns the original sequence for an identical retry;
- rejects a different canonical payload for an existing dot;
- checks maximum retained operations and bytes;
- allocates the next strictly increasing server sequence;
- stores the canonical event and its SHA-256 binding; and
- updates high-water, count, and byte metadata.

The transaction result is the linearization point for successful publication.
A process crash before commit acknowledges nothing; a crash after commit but
before live fan-out is recovered by replay. The log is intentionally retained
until an operator performs a separately designed checkpoint/epoch migration:
blind deletion would make an offline replica unable to repair a gap and could
resurrect data after CRDT tombstone compaction.

Replay is delivery recovery, not a proof that a consumer applied a CRDT update.
The consumer must persist its concrete CRDT state, delivery frontier, durable
outbox, and resume cursor in its own transaction before considering an event
installed. The reconnecting client exposes that ordering through its callback
instead of claiming an unsafe generic transaction across application storage.

## Security and abuse controls

| Boundary | Reference behavior | Host responsibility |
| --- | --- | --- |
| Transport | Requires a negotiated subprotocol, bounded control/data frames, deadlines, disabled compression, and an explicit origin allow-list. | TLS certificates, ingress timeouts, DDoS/WAF policy. |
| Identity | Authenticates before upgrade; separates write authorization from replay subscription authorization. | Session/JWT/mTLS validation, revocation, tenant lookup, audit identity. |
| Data | Canonical manifest/frame checks, semantic validator, exact-dot digest binding, no-store response headers. | CRDT-specific decoder limits, value authorization, encryption at rest, backup access controls. |
| Capacity | Bounds message bytes, actors, connected-peer queues, replay result size, retained log count, and log bytes. | Rate limits, per-tenant quotas, alerting, storage expansion, retention/checkpoint operations. |
| Recovery | Fails closed on corrupted storage or an unavailable replay window. | Checkpoint distribution, restore drill, incident response, membership authority and GC epochs. |

Neither a frame checksum nor a stored digest authenticates an untrusted peer;
they detect conflicting or damaged stored protocol bytes after authentication.

## Performance model

The hot write path performs one bounded frame validation, one serialized local
store transaction, and fan-out into bounded per-peer queues. The chosen store
has one writer, so adding server instances against the same file is unsafe and
does not improve write throughput. Use batching only when the application can
preserve per-change acknowledgement and replay identity; this v1 reference
uses one durable event per change to keep failure semantics inspectable.

Benchmark claims are limited to local loopback and local durable-store cost.
They measure neither WAN latency, TLS termination, a browser/mobile runtime,
identity provider, a multi-node database, nor production capacity. The test
matrix includes restart replay, an ambiguous publish retry, a forced connection
drop/reconnect, out-of-order actor dots, conflicting retry rejection, slow-peer
backpressure, storage limits, and concurrent appends.

## Non-goals and promotion gates

This is not multi-region replication, a Raft log, a generic CRDT checkpoint
service, automatic tombstone collection, or an authorization implementation.
Before adopting it beyond the reference deployment shape, prove: atomic client
checkpoint semantics; restore/backup drills; metric and alert coverage; tenant
quotas; bounded retention migration with an authenticated membership epoch;
and a load/fault test on the actual storage, TLS, and identity stack.
