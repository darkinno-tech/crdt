# Membership protocol reference

`membership` is a transport-independent reference protocol for running
`tombstonegc.Coordinator` safely. It is deliberately scoped to OR-Set
tombstone collection; it does not make RGA, LWW-Map, or OR-Tree tombstone GC
safe.

## Safety model

- A signed `membership.View` is the only authority for the active member set.
  Its `GroupID`, `Epoch`, predecessor hash, manifest hash, sorted members, and
  member incarnations are canonical and signed by an application-owned Ed25519
  authority key.
- `Gossip` heartbeats can mark a replica `Suspect`, but that is an
  observability signal only. A suspect replica remains in the GC member set.
  The authority must publish a successor View to retire or fence it.
- `View.Epoch` must equal `replica.Manifest.Epoch`; check
  `View.MatchesManifest` in the authenticated handshake before accepting
  state, delta, gossip, or receipt traffic. Old epochs are fenced.
- A retired replica must discard its pre-fence state and bootstrap from a
  current state checkpoint before it joins a later View. Keep its persisted HLC
  state, create a set with that HLC state, then install the post-compaction
  state so the next locally emitted tag remains unique.
- `Receipt` is signed by a member and binds group, epoch, view hash, member
  incarnation, sequence, checkpoint ID, and exact sorted tombstone tags.
  `GCBridge` rejects stale/replayed receipts before passing tags to the
  Coordinator.

This is a crash-fault protocol. Signature verification proves the receipt's
origin but cannot make a malicious member's claim of durable storage true.
Byzantine safety requires an external trusted-storage/attestation or consensus
design and is intentionally out of scope.

## Typical lifecycle

1. The authority creates and signs the initial View after creating the matching
   replication Manifest. Store it durably with `NewManager`.
2. Each member sends signed gossip heartbeats to peers selected by `Peers`.
   Export `Suspects` to monitoring; do not call `ReplaceMembership` from it.
3. A member persists its CRDT checkpoint, creates a signed exact-tag Receipt,
   and sends it through the chosen authenticated transport.
4. Receivers decode with `UnmarshalReceipt`, then call `GCBridge.Apply`.
   Persist the compacted OR-Set snapshot and HLC state before calling
   `PruneAcknowledgements`.
5. To retire a member, publish and durably install a signed direct successor
   View. `Manager.Install` fences old traffic and clears all old-epoch GC
   receipts. Current members must acknowledge again before compaction resumes.
6. To re-admit a retired member, require bootstrap first and issue another
   signed View with a new member incarnation.

## Wire and resource limits

`MarshalView`, `MarshalGossipMessage`, and `MarshalReceipt` provide canonical
binary messages. Decoders reject unsupported versions, truncation, trailing
data, overlong varints, invalid ordering, and messages over 1 MiB. Receipt
chunks are limited to 8,192 tags. Applications should impose stricter
transport-level message, rate, connection, and storage quotas for their own
workload.

## Performance progression

Start with exact tag chunks: they are straightforward to audit and preserve
the Coordinator's exact acknowledgement semantics. Record active-member count,
tombstone count/age, receipt bytes, acknowledgement coverage, compaction time,
and view churn. Only if these measurements show a real M×T bottleneck should a
future protocol add immutable batch roots and reconciliation; roots reduce
transfer but do not prove a member honestly persisted a tombstone.
