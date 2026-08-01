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
| Cursor | The awareness JSON field contains encoded `Y.RelativePosition` values; every display resolves them against the exact local `Y.Text`. |
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
import { bindYjsCodeMirrorPlainText } from "@darkinno/crdt-client/yjs";

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
make typescript-yjs-bindings-benchmark
```

The focused suite uses a real CodeMirror 6 view under JSDOM for remote range
application, tests V1/V2, state-vector recovery, cursor/awareness forwarding,
formatted-text refusal, and a three-replica delayed/duplicated/reordered update
simulation. The benchmark records local process work and editor write shape;
it is not a browser rendering, WebSocket, TLS, WAN, persistence, or service
capacity result. See the [recorded baseline](../operations/yjs-native-editor-bindings-2026-08-01.md).
