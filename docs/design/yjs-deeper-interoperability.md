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
| 1 | State-vector diff, update merge, V1/V2 handling, durable compaction | Implemented as an opt-in `YJSStore` sidecar capability. A store-backed `YJSRoom` delegates semantic operations to it; an opaque room remains Level 0. |
| 2 | Yjs `Y.Text` / Quill / ProseMirror semantics and renderer schema | Use Yjs shared types end-to-end, or rich-text v1 end-to-end. Do not bridge live mutations. |
| 3 | Cross-CRDT migration | Offline, one-way export/import with a frozen source snapshot, explicit loss report, new replica identities, and cut-over epoch. |

Yjs documents its state-vector diff API (`encodeStateAsUpdate` with the remote
vector), binary update merge/diff helpers, V1/V2 conversion, and relative
positions. These are engine operations, not properties that can be safely
recreated by parsing only the outer WebSocket envelope. See the [Yjs update
API](https://github.com/yjs/yjs#document-updates) and [sync internals](https://github.com/yjs/yjs/blob/main/INTERNALS.md).

## Browser Yjs core capability boundary

The optional `@darkinno/crdt-client/yjs` layer now fills the browser-side
integration gaps that matter for a plain-text surface, while keeping the Yjs
engine authoritative. It is not a second document model.

| Yjs core surface | Integration decision | Boundary |
| --- | --- | --- |
| Shared `Y.Map` / `Y.Array` / `Y.Text` / XML types | Direct native Yjs API | No Go/native-ts translation or schema claim. |
| Relative positions | Bounded `createRelativePosition` / `resolveRelativePosition` for the exact bound `Y.Text` | Position bytes are presence/UI metadata, not identity, authorization, or an RGA anchor. |
| `observeDeep` | Bounded path + live-target projection with event/path caps and fail-closed callback handling | Do not retain raw lazy `Y.Event` objects or use observer output as a durable log. |
| Selective undo/redo | Binding-scoped, capped `Y.UndoManager`, tracking only local editor-origin transactions | Undo creates a compensating shared update; it never rewinds a server log or silently undoes remote work. At its cap the binding safely clears complete local history before recording the new edit. |
| Incremental synchronization | Standard V1 y-protocols SyncStep1/2 helper for manual transports; direct V1/V2 state-vector diff APIs remain available | y-websocket owns its outer envelope; room identity, auth, receipt, and retry stay above the helper. A throwing local manual callback latches its outbound path after the already-committed Yjs transaction, so the application must recover its outbox and resync rather than issue another edit. |
| Rich text / editor schema | Use the maintained matching Yjs binding such as y-prosemirror, y-quill, or y-codemirror | The plain-text binding stops on formats/embeds rather than flattening them. |

The V1 sync helper intentionally rejects V2. `y-protocols` SyncStep1/2 calls
the V1 update APIs; a V2 room must use the format-pinned state-vector/diff
methods in an explicitly negotiated outer protocol. This avoids silently
coercing an update format merely to reuse a transport helper.

## Architecture for Level 1

The safe future extension is an explicitly negotiated Yjs document service,
not a hidden enhancement to Go CRDT rooms:

```text
authenticated Yjs room
  -> transport-size and rate limits
  -> YJSStore (Yjs-aware runtime, V1/V2 mode pinned per room,
               bounded active requests + receive deadline)
       Apply(update)
       StateVector()
       Diff(remoteVector)
       SnapshotOrMergedUpdate()
       RecoveryCursor()
  -> atomic durable update/snapshot + authorization audit/outbox
```

`YJSStore` is implemented by the pinned `yjsstore/runtime` Node sidecar using
`yjs@13.6.31`. `extensions.NewYJSStore` is a bounded Go client, not a second
Yjs parser. A configured store-backed `YJSRoom` starts sync with the durable
state vector, obtains a semantic diff for a client Step 1, and submits every
Step 2/update to `YJSStore.Apply` before live fan-out. The store materializes a
fresh `Y.Doc` with Yjs GC enabled, writes the resulting merged snapshot and
state vector through an fsync + rename transaction, and advances its recovery
cursor only after that write succeeds.

The bundled runtime is a loopback-only, single-process sidecar for one data
directory. Its request lock has process scope, so an HA deployment must assign
each document directory to one writer or provide a different store with
cross-process serialization. Before a request body is collected, the runtime
admits only a configured bounded number of semantic requests; an excess request
returns `unavailable`, and incomplete headers/bodies expire under its receive
deadline. This constrains local heap and lock pressure but does not replace
gateway rate limits or a Node heap/container ceiling. The Go client never
follows a redirect from the configured bearer-token endpoint, and a handler
permits one store-backed room per exact durable document identity; both rules
prevent trust-boundary drift or live fan-out split-brain.

Tenant, room, epoch, schema label, and V1/V2 format form the immutable durable
identity. The schema label is a fencing/version field, not a claim that the
sidecar understands an application's ProseMirror, Quill, or custom schema.
Application schema validation and authorization remain at the authenticated
gateway. See the [store integration guide](../integration/yjs-store.md) for
the concrete configuration and recovery contract.

## Performance, security, and correctness gates

- **Correctness:** verify an official Yjs client can sync from a state vector,
  reconnect from a compacted snapshot/update, replay duplicates, reorder
  updates, and converge across `Y.Text`, nested shared types, and the selected
  editor binding. Test V1 and V2 only when the room explicitly negotiates
  them.
- **Security:** authenticate identity independently of Yjs client IDs; bind
  room/tenant/epoch/schema; cap raw frame, decoded update, state vector,
  document, active request, queue, and fan-out work; enforce short nonzero
  request/header receive deadlines and read/write/presence authorization
  continuously; disable unsafe compression at the public boundary; audit
  update admission without logging document contents.
- **Performance:** measure state-vector diff bytes, update merge/compaction
  latency, memory per active room, durable flush time, reconnect p50/p95/p99,
  slow-peer behavior, capped local-history reset cost, and fan-out at
  1/4/16/64 receivers. The current relay microbenchmark measures only
  outer-wrapper decode/admission and cannot set these production limits.
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
