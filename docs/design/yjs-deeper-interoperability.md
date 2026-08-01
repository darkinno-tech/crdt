# Yjs deeper interoperability decision

## Decision

Keep `extensions.YJSHandler` as a bounded, authenticated `y-websocket` /
`y-protocols` envelope relay. Do **not** translate a Yjs update into a Go RGA
or rich-text frame, and do not describe the existing relay as a full Yjs
document engine.

The current relay is compatible with the live y-protocols wrapper used by
standard Yjs clients: sync messages, update messages, awareness, and awareness
queries. It retains bounded opaque update history, fans it out, and keeps
awareness ephemeral. It intentionally cannot interpret a Yjs state vector,
validate a Yjs update's shared types, compute a missing update, merge update
history, compact GC state, or create a semantically validated durable Yjs
snapshot.

## Why frame conversion is incorrect

Yjs updates identify structs with Yjs client/clock pairs, carry YATA ordering
and delete sets, and contain shared-type integration semantics. Go rich-text
uses a private run-v2 RGA plus LWW registers keyed by Go HLC positions. These
identity spaces, deletion lifetimes, conflict rules, block conventions, and
snapshot recovery contracts are different.

Even when both documents render the same text, a byte-level or operation-level
conversion loses at least one side's stable cursor/anchor, concurrent insert
intent, deleted structural anchor, formatting conflict, or recovery history.
It would make both protocols look synchronized until a reconnect, duplicate,
or concurrent edit exposed the divergence. A checksum only detects corruption;
it is not semantic validation or authorization.

## Interoperability levels

| Level | Contract | Status / safe next step |
| --- | --- | --- |
| 0 | y-websocket envelope, opaque v1 updates, awareness fan-out | Implemented by `YJSHandler`; bounded live compatibility. |
| 1 | State-vector diff, update merge, V1/V2 handling, durable compaction | Requires a Yjs-aware engine/store. Add it behind a separate `YJSStore` capability, never in `YJSRoom`. |
| 2 | Yjs `Y.Text` / Quill / ProseMirror semantics and renderer schema | Use Yjs shared types end-to-end, or rich-text v1 end-to-end. Do not bridge live mutations. |
| 3 | Cross-CRDT migration | Offline, one-way export/import with a frozen source snapshot, explicit loss report, new replica identities, and cut-over epoch. |

Yjs documents its state-vector diff API (`encodeStateAsUpdate` with the remote
vector), binary update merge/diff helpers, V1/V2 conversion, and relative
positions. These are engine operations, not properties that can be safely
recreated by parsing only the outer WebSocket envelope. See the [Yjs update
API](https://github.com/yjs/yjs#document-updates) and [sync internals](https://github.com/yjs/yjs/blob/main/INTERNALS.md).

## Architecture for Level 1

The safe future extension is an explicitly negotiated Yjs document service,
not a hidden enhancement to Go CRDT rooms:

```text
authenticated Yjs room
  -> transport-size and rate limits
  -> YJSStore (Yjs-aware runtime, V1/V2 mode pinned per room)
       Apply(update)
       StateVector()
       Diff(remoteVector)
       SnapshotOrMergedUpdate()
       RecoveryCursor()
  -> atomic durable update/snapshot + authorization audit/outbox
```

`YJSStore` can be implemented by a maintained Yjs runtime in a dedicated
service or sidecar. Its authenticated room identity, tenant, epoch, update
format, schema policy, replay cursor, storage transaction, and backup/restore
contract must be explicit. The Go relay may route to it but must not claim that
it validates Yjs semantics itself.

## Performance, security, and correctness gates

- **Correctness:** verify an official Yjs client can sync from a state vector,
  reconnect from a compacted snapshot/update, replay duplicates, reorder
  updates, and converge across `Y.Text`, nested shared types, and the selected
  editor binding. Test V1 and V2 only when the room explicitly negotiates
  them.
- **Security:** authenticate identity independently of Yjs client IDs; bind
  room/tenant/epoch/schema; cap raw frame, decoded update, state vector,
  document, queue, and fan-out work; enforce read/write/presence authorization
  continuously; disable unsafe compression at the public boundary; audit
  update admission without logging document contents.
- **Performance:** measure state-vector diff bytes, update merge/compaction
  latency, memory per active room, durable flush time, reconnect p50/p95/p99,
  slow-peer behavior, and fan-out at 1/4/16/64 receivers. The current relay
  microbenchmark measures only outer-wrapper decode/admission and cannot set
  these production limits.
- **Migration:** stop writes, take one source snapshot at a known cursor,
  validate/render it, export under explicit supported-schema rules, import to
  a new group and epoch, compare presentation plus allowed metadata, then
  cut over. Never mix converted updates into either source history.

## Product direction

For applications that need the Yjs ecosystem, run Yjs end-to-end and use the
relay now, then add a `YJSStore` only when durable state-vector recovery is a
real workload. For applications that need this repository's authenticated Go
CRDT persistence, exact manifest controls, and the new Quill rich-text
binding, use rich-text v1 end-to-end. The two products can coexist under a
gateway but must be separately named, negotiated, persisted, and observed.
