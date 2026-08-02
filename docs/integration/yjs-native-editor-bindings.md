# Native Yjs incremental editor binding

`@darkinno/crdt-client/yjs` is an opt-in browser binding for a **native Yjs
document**. It maps a `Y.TextEvent.delta` into a CodeMirror 6 change set, so a
remote one-character edit updates that range rather than replacing the whole
editor projection.

It is intentionally separate from `bindRGAPlainText`: Go RGA/run-v2 frames,
`text.Anchor`, and Go `awareness` are not Yjs documents, relative positions,
or y-protocols awareness messages. Do not put both protocols in one room or
try to convert live mutations between them.

## Compatibility contract

| Surface | Contract |
| --- | --- |
| Document and updates | Direct `Y.Doc` / `Y.Text`, V1 or V2 explicitly pinned per room. `Y.applyUpdate`, `encodeStateVector`, and state-vector differences retain native Yjs semantics. |
| Remote editor changes | `Y.TextEvent.delta` is translated to one or more CodeMirror UTF-16 changes. No remote `text.toString()` projection is used after initial attachment. |
| Relative positions | `createRelativePosition` / `resolveRelativePosition` expose bounded binary `Y.RelativePosition` values for comments, selections, and anchors. The awareness cursor field uses the same representation and resolves only against the exact local `Y.Text`. |
| Deep observation | `observeYjsDeep` exposes bounded changed paths and live target types for `Y.Map`, `Y.Array`, `Y.Text`, and XML descendants. It never retains raw lazy `Y.Event` objects or arbitrary values. |
| Undo/redo | `createUndoManager` tracks only `applyLocalReplacement` transactions from this binding. Undo/redo emit compensating local Yjs updates; remote edits are deliberately excluded. History is capped at 256 stack items by default. |
| Manual V1 sync | `createSyncProtocol` reads and writes exactly one bounded, unwrapped y-protocols SyncStep1/2 or update submessage. V2 continues to use state-vector/diff methods directly because y-protocols sync is V1. |
| Presence | `y-protocols/awareness` encode/apply APIs are used directly. Yjs client IDs are routing identifiers, never authenticated user identities. |
| Rich text | Not supported by this plain-text binding. A format or embed stops projection instead of silently flattening it. Use a schema-aware Yjs editor binding for rich content. |

The Go `extensions.YJSHandler` already supports the y-websocket/y-protocols
envelope and can relay these native bytes. A store-backed configured room adds
durable state-vector recovery. It still does not make a Go CRDT group a Yjs
room.

## Data flow and transport ownership

```text
CodeMirror local transaction
  -> Y.Text transaction (one native Yjs update)
  -> y-websocket-compatible provider OR explicit onLocalUpdate callback
  -> extensions.YJSHandler / YJSStore
  -> Y.applyUpdate on peer Y.Doc
  -> Y.TextEvent.delta
  -> exact CodeMirror changes + relative-position cursor resolution

Awareness.setLocalStateField(relative cursor)
  -> y-protocols awareness update
  -> relay (ephemeral only)
  -> applyAwarenessUpdate
  -> remoteCursors()
```

Choose exactly one transport owner for one document:

- Use a standard `y-websocket` provider with the same `Y.Doc` and `Awareness`.
  Do not configure `onLocalUpdate` or `onLocalAwarenessUpdate`.
- Or own the authenticated transport and wire its received binary payloads to
  `applyRemoteUpdate` / `applyRemoteAwarenessUpdate`, and its send path to the
  two `onLocal*` callbacks.

Combining both paths forwards the same idempotent data twice and complicates
backpressure, metrics, and authorization accounting.

## CodeMirror 6 setup

The application owns the Yjs provider, authentication, authorization, and
editor lifecycle. The binding owns neither the WebSocket nor the document.

```ts
import * as Y from "yjs";
import { Awareness } from "y-protocols/awareness.js";
import type { ViewUpdate } from "@codemirror/view";
import { bindYjsCodeMirrorPlainText, observeYjsDeep } from "@darkinno/crdt-client/yjs";

const document = new Y.Doc();
const text = document.getText("content");
const awareness = new Awareness(document);
let binding: ReturnType<typeof bindYjsCodeMirrorPlainText> | undefined;

// Add this callback to the actual EditorView updateListener.
function onCodeMirrorUpdate(update: ViewUpdate) {
  binding?.applyViewUpdate(update);
}

binding = bindYjsCodeMirrorPlainText(document, text, view, {
  updateFormat: "v1", // Match the configured YJSRoom / YJSStore format.
  maxUpdateBytes: 1 << 20,
  maxAwarenessBytes: 64 << 10,
  maxTextUTF16: 1 << 20,
  maxCursorBytes: 256,
}, awareness);

// Editor selections use CodeMirror/Y.Text UTF-16 offsets. The stored value is
// a Yjs relative position and survives edits before it.
binding.setLocalCursor({ anchor: 12, head: 18 });
renderRemoteCursors(binding.remoteCursors());
```

An initial editor/document mismatch is written once at attachment. Thereafter
remote transactions are range changes. A local single-range CodeMirror update
also remains incremental; legacy or multi-range local updates use a deliberate
atomic text fallback, never a partial Yjs transaction.

## Manual transport example

This is for a host that owns the authenticated WebSocket protocol. A standard
`y-websocket` provider already does this and should be preferred when its
reconnection behavior fits the product.

```ts
const binding = bindYjsCodeMirrorPlainText(document, text, view, {
  updateFormat: "v1",
  maxUpdateBytes: 1 << 20,
  maxAwarenessBytes: 64 << 10,
  maxTextUTF16: 1 << 20,
  maxCursorBytes: 256,
  onLocalUpdate: (update) => transport.sendYjsUpdate(update),
  onLocalAwarenessUpdate: (update) => transport.sendAwareness(update),
}, awareness);

transport.onYjsUpdate = (update) => binding.applyRemoteUpdate(update);
transport.onAwareness = (update) => binding.applyRemoteAwarenessUpdate(update);
```

State-vector recovery remains native: call `encodeStateVector()` for sync Step
1 and `encodeStateAsUpdate(peerVector)` for the V1/V2-pinned missing update.
Never feed V1 bytes to a V2 room or vice versa.

### Relative positions, deep views, and local undo

```ts
const commentStart = binding.createRelativePosition(12); // associates with the next character
const commentEnd = binding.createRelativePosition(18, -1); // associates with the prior character

// Resolve only against this exact Y.Text; a foreign or malformed position fails closed.
renderComment({ from: binding.resolveRelativePosition(commentStart), to: binding.resolveRelativePosition(commentEnd) });

const undo = binding.createUndoManager({
  captureTimeout: 500,
  maxStackItems: 256,
});
undo.stopCapturing(); // begin a new user-action group before the next editor change
if (undo.undo()) {
  // The binding's onLocalUpdate callback receives this compensating update.
}

const stopBoardView = observeYjsDeep(board, {
  maxEventsPerTransaction: 128,
  maxPathDepth: 16,
  onChanges(changes) {
    // Read `target` now. Do not retain raw Y.Event data beyond this callback.
    for (const { path, target } of changes) refreshPath(path, target);
  },
  onError(error) { detachBoardView(error); },
});
```

Undo history is local UI state: do not serialize it, infer authorization from
it, or expect it to roll back a shared document. A remote edit is never added
to this manager, and a successful `undo()` / `redo()` creates a normal new Yjs
update that needs the same authenticated transport, durable receipt, and retry
handling as any other local change. Destroy the manager before changing the
room, schema, or editor surface; destroying the binding does this automatically.
`maxStackItems` defaults to 256. Before a binding-owned edit would exceed that
cap, the binding clears the complete local undo/redo history through Yjs's own
release path, then records the new edit. This deliberately keeps the newest
operation undoable instead of splicing Yjs internal stack items, which could
retain deleted structs or violate its garbage-collection bookkeeping.

### Manual y-protocols V1 SyncStep1/2

Use this only when the application owns the authenticated transport. The helper
returns an **inner** y-protocols sync submessage: an existing y-websocket
provider already owns the outer `messageSync = 0` wrapper and must not be used
at the same time.

```ts
const sync = binding.createSyncProtocol({
  // Reserve the y-protocols varint/type envelope above the configured update cap.
  maxMessageBytes: (1 << 20) + 16,
});

transport.sendYjsSync(sync.encodeSyncStep1());
transport.onYjsSync = (message) => {
  const response = sync.receive(message); // SyncStep1 -> SyncStep2, otherwise no response
  if (response !== undefined) transport.sendYjsSync(response);
};
```

The room name, outer provider envelope, authentication, authorization,
backpressure, receipt, and retry policy remain transport responsibilities.
`receive` rejects an unknown type, trailing bytes, an oversized inner payload,
or a V2 binding before it mutates the Y.Doc. For V2 rooms, exchange the bounded
state vector with `encodeStateVector()` and use
`encodeStateAsUpdate(remoteStateVector)` / `applyRemoteUpdate(update)` in a
separately negotiated authenticated envelope.

## Safety and resource boundaries

- `maxUpdateBytes` and `maxAwarenessBytes` reject inbound bytes before their
  respective JavaScript protocol decoders. Set them no higher than the relay
  and store limits.
- `maxTextUTF16` prevents a local editor transaction from growing the text
  beyond the selected UI budget. It cannot make an already-decoded malicious
  Yjs update allocation-free; the authenticated server and store remain the
  public trust boundary.
- `maxCursorBytes` bounds the decoded relative-position payload. Invalid,
  foreign-text, stale, or malformed awareness cursor values are ignored.
- `maxStackItems` bounds local undo/redo retention. It is a UI-memory limit,
  not a durable history, authorization record, or remote-operation limit.
- `observeYjsDeep` has independent event-count and path-depth caps. On an
  overflow or application callback failure it unregisters itself after the
  current transaction instead of providing a partial or silently stale view.
- The SyncStep helper checks the full envelope before parsing; set
  `maxMessageBytes` to the exact authenticated transport cap and reserve enough
  bytes for the y-protocols varint/type envelope above `maxUpdateBytes`.
- Awareness remains ephemeral. Do not place it in YJSStore snapshots, Go CRDT
  frames, audit logs, or authorization decisions. Bind the relay's client ID
  to an authenticated connection at the server, as `YJSHandler` does.
- Treat a `YjsBindingError("unsupported_text")` as a rendering boundary. The
  underlying Yjs document remains valid; detach this plain-text view and attach
  a schema-aware rich-text surface rather than stripping formats or embeds.

## Validation and performance scope

```sh
make typescript-test
node --test clients/typescript/test/yjs.test.mjs
make typescript-yjs-core-benchmark
make typescript-yjs-bindings-benchmark
```

The focused suite uses a real CodeMirror 6 view under JSDOM for remote range
application, tests V1/V2, state-vector recovery, cursor/awareness forwarding,
formatted-text refusal, binding-scoped undo/redo, deep-observation bounds, V1
SyncStep1/2 convergence, capped undo-history reset, and a three-replica
delayed/duplicated/reordered update simulation. The benchmarks record local
process work and editor write shape;
they are not browser rendering, WebSocket, TLS, WAN, persistence, or service
capacity results. See the [recorded baselines](../operations/yjs-native-editor-bindings-2026-08-01.md).
