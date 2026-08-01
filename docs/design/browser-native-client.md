# Browser native client: persistence and transport boundary

## Decision

Add an opt-in browser facade for the existing `native-ts-v1` protocol:
`openNativeBrowserDocument`, IndexedDB append-log persistence, a retryable
outbox, and an injectable authenticated transport. Preserve the Go/Wasm RGA
path for groups that use framed Go RGA semantics.

This is deliberately not a WebSocket client for `crdt-sync-v1` or
`crdt-durable-v1`. Those relays validate a `replica.Manifest`, `replica.Dot`,
and a concrete Go frame. `native-ts-v1` has a separate canonical JSON update
contract and no Go FrameType. Pretending that it could join the same group
would break the protocol and authorization boundary.

## Evidence and gap

The TypeScript package already supplied:

- `NativeDocument`, LWW maps, RGA arrays, transactions, bounded canonical
  updates, duplicate/reordered merge, and complete in-memory snapshots;
- a bounded Go/Wasm RGA runtime for exact run-v2 interoperability; and
- low-level `onUpdate`/`applyEncodedUpdate` hooks.

It intentionally supplied no browser persistence, local outbox, recovery
retry, transport lifecycle, or cross-tab adapter. A new browser user therefore
had to reimplement counter-safe restore, storage transactions, outbox ordering,
and bounded receive handling before a simple local-first application could be
trusted.

## Selected architecture

```text
local Map/Array mutation
        |
        v
NativeDocument validates and merges
        |
        +--> canonical native-ts-v1 bytes --> IndexedDB append + metadata (one transaction)
        |                                       |
        |                                       +--> pending local outbox
        |                                                  |
        v                                                  v
UI observers <---------------- authenticated NativeBrowserTransport receipt

authenticated, document-bound incoming bytes
        |
        v
bounded native decoder -> atomic merge -> same append log
```

`NativeDocument.persistenceMetadata()` exposes only copied root declarations
and the local counter. The browser store appends canonical update bytes with
that metadata instead of repeatedly serializing full state. A same-actor
restart restores a compacted base followed by the log; the saved counter is
never allowed to move backwards. If an edit changes memory before its queued
storage write runs, storing a later counter is safe: it may skip a value but
cannot reuse an immutable `{ actor, counter }` ID.

When there is no pending local outbox and the array graph is complete, the
store replaces its base and deletes the log in one transaction. The complete
base preserves state while bounding future recovery cost. It never compacts a
document with an unresolved parent because such a snapshot is intentionally
incomplete. The native error is explicit (`incomplete_state`), rather than
being silently translated into a state snapshot.

## Alternatives considered

| Alternative | Result |
| --- | --- |
| Full snapshot on every browser edit | Rejected: safe but O(total document state) per keystroke and causes avoidable allocation/UI pressure. |
| Persist only local counter plus state bytes | Rejected: no incremental crash recovery/outbox, and callers can accidentally split the unit across writes. |
| Reuse one durable actor ID in multiple tabs | Rejected: independently running counters can collide. Each active tab/Worker receives a fresh actor ID. |
| Embed a universal WebSocket URL/envelope | Rejected: native updates cannot be passed to Go frame relays, and authentication/receipt semantics belong to the application. |
| Browser facade + bounded append log + injectable adapter | Selected: few-line local-first API without hiding protocol or production boundaries. |

## Safety and security properties

- The same 1 MiB native update limit and full canonical decoder run before a
  received update mutates state. Hosts must impose an equal/lower network body
  cap before creating a `Uint8Array`.
- IndexedDB transactions atomically add one update and its current recovery
  metadata. Browser shutdown/storage quota can still abort or evict data;
  `flush()` is a completed browser transaction, not a hardware durability
  guarantee.
- The local outbox remains until `NativeBrowserTransport.send` resolves. The
  adapter defines that receipt; resolve only after the application's required
  durable server acknowledgement, not merely a WebSocket enqueue.
- Limits default to compaction at 128 updates/1 MiB and rejection at
  10,000 updates/32 MiB. Rejection is surfaced as `persistence_limit` rather
  than dropping a successful in-memory mutation.
- `BroadcastChannelNativeTransport` is a `NativeBrowserLiveTransport`: it
  copies bytes and publishes only after the local append, but can never
  acknowledge an outbox entry. Same-origin messaging is not authentication and
  is intentionally volatile; it cannot bootstrap a later tab or repair a
  partition.
- `documentID` is a storage/adapter routing key, not access control. A server
  adapter must authenticate identity and bind a permitted group, schema,
  semantic version, and selected limits before accepting a message.

## Verification plan

- Unit tests cover outbox retry, receipt-only compaction, storage limit errors,
  incomplete-parent logging, restore, and three offline editors with reverse
  and duplicate delivery.
- A real browser harness runs Go/Wasm RGA merging, IndexedDB persistence and
  restart, plus same-origin BroadcastChannel delivery while asserting that the
  sender's durable outbox remains pending.
- Controlled benchmarks measure append/flush/recovery in a deterministic
  memory store and in an actual browser IndexedDB run. They are development
  measurements, not a storage latency or mobile SLA.
