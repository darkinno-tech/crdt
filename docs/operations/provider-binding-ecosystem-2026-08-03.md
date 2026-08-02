# Provider and binding ecosystem decision — 2026-08-03

## Decision

Do not chase a provider-count metric by translating every Yjs package into a
Go CRDT backend. Instead, make the native Yjs path useful for the high-value
interoperability surfaces, keep Go CRDT providers as their own protocol family,
and expose a bounded editor adapter where Yjs has a real editor contract.

This change adds the missing native `Y.Text` / Quill Delta binding. It preserves
approved formats and one-key embeds, and it works with the existing
y-websocket-compatible `extensions.YJSHandler` or an application-owned native
Yjs transport. It does not make the Go RGA, `richtext` v1, `native-ts-v1`,
Redis/SQL logs, or the WebRTC bridge wire-compatible with Yjs.

## Evidence and inventory

The [Yjs project overview](https://github.com/yjs/yjs#overview) explicitly
separates core shared types from network and editor modules. On 2026-08-03 its
provider list called itself incomplete while listing at least 22 connection
choices, six persistence entries, and 20 editor/state binding rows. That is a
useful discovery index, not a compatibility or security certification.

| Surface | Current repository capability | Yjs ecosystem class | Decision |
| --- | --- | --- | --- |
| Live WebSocket | Bounded Go live relay plus `extensions.YJSHandler` for y-websocket/y-protocols | y-websocket, Hocuspocus, y-sweet, hosted backends | `YJSHandler` remains native-yjs-envelope compatible; use a vendor/provider directly with `Y.Doc`, never through RGA frames. |
| Peer-to-peer | Ordered/reliable Go CRDT DataChannel bridge | y-webrtc, libp2p, Matrix, Nostr, ATProto, Dat | Keep Go bridge protocol-specific. P2P identity, signaling, replay, durable receipt, relay policy, and NAT behavior are host-owned. |
| Server durability | bbolt reference; Redis, PostgreSQL, MySQL, and SQLite `durable.Log` modules | y-indexeddb, y-leveldb, Mongo/PostgreSQL/Firestore, y-sweet storage | Go durable logs are for Go envelopes only. `YJSStore` is the semantic Yjs sidecar path and binds tenant/room/epoch/schema/format before snapshot recovery. |
| Local offline cache | Go/Wasm checkpoints and native TypeScript stores | y-indexeddb, React Native SQLite | Keep storage format-bound. A Yjs room uses native Yjs state vectors; RGA groups keep state/frontier/HLC atomically. |
| Plain text editors | RGA adapters for Quill, CodeMirror, Monaco, ProseMirror/Tiptap ports, Slate, Lexical | y-quill, y-codemirror, y-monaco, y-prosemirror, Slate and framework bindings | Existing RGA adapters stay profile-bound. Native Yjs has CodeMirror plain text and now a bounded Quill Delta binding. |
| Rich text editors | Go/Wasm richtext profiles for Quill, Tiptap/ProseMirror, and BlockNote | y-quill, y-prosemirror, native editor integrations | Add only a native Delta surface where the editor contract is reversible. Arbitrary schemas, DOM/HTML, and vendor extensions remain rejected. |

## Architecture and correctness boundary

```text
Quill 2 Delta (approved schema)
        <-> YjsQuillRichTextBinding
        <-> Y.Text / Y.Doc (V1 or V2 pinned per room)
        <-> exactly one transport owner
             -> standard y-websocket-compatible provider -> extensions.YJSHandler -> optional YJSStore
             -> application outbox / authenticated native Yjs transport

Go RGA / richtext / native-ts groups
        <-> their own Manifest, state/frontier/HLC and durable.Log path
```

The two branches deliberately never join below the editor. A successful
WebSocket callback, DataChannel send, Redis script, SQL commit, or Yjs update
callback is not a durable client receipt or proof that another peer rendered a
change.

`YjsRichTextBinding` validates every copied Delta before local mutation and
before forwarding a remote editor projection:

- exactly one `insert`, `retain`, or `delete` action per operation;
- bounded operation count, text length, format keys/values, and embed JSON;
- an explicit set of allowed format names and single-key embed names;
- V1/V2 update format pinned by room, never auto-detected;
- local callback failure latches further binding-owned edits because Yjs has
  already committed the triggering transaction;
- unknown remote format/embed or editor callback failure freezes the projection
  without stripping content or rolling back the shared document.

The Go relay/store still performs connection authentication, independent
publish/subscribe authorization, origin checks, queue bounds, document identity
binding, update/body limits, and durable-before-fan-out. A client-selected Yjs
client ID remains a routing value, never an authenticated user identity.

## Multi-dimensional assessment

| Dimension | Finding | Consequence |
| --- | --- | --- |
| Implementation | Most Yjs providers are alternative transports or hosted products, not a shared provider ABI. | Add adapters only where the wire, identity and recovery contract can be tested; do not create empty Go wrappers for every catalog entry. |
| Design | Go CRDT and Yjs use distinct state, recovery and editor semantics. | Native Yjs rooms stay opaque/semantic-Yjs; RGA, richtext and collections stay manifest-negotiated Go protocols. |
| Correctness | Full-string conversion loses formatting, embeds, positions and transaction intent. | Carry approved Quill Deltas directly through `Y.Text`; reject instead of flattening unsupported content. |
| Security | A provider list says nothing about authentication, tenant isolation, payload amplification, encryption or retention. | Keep per-room authorization, pre-decode bounds, explicit schema policy and sidecar/store admission mandatory. |
| Performance | Editor whole-document replacement has different costs from native Delta application. | Measure incremental versus full projection on the chosen editor/browser; do not infer WAN, TLS, persistence or paint capacity from local benchmarks. |
| Operations | Hosted/federated/P2P providers change failure domains and support obligations. | Evaluate each selected provider for release cadence, data residency, backup/restore, rate limits, webhook/idempotency, observability and exit path before adoption. |

## Adoption order

1. Use the existing y-websocket-compatible relay and YJSStore for a bounded,
   native Yjs room where Yjs interoperability is required.
2. For Quill 2, use `bindYjsQuillRichText` and declare the exact document
   schema's allowed formats and embeds. Give the binding either a standard
   provider or a synchronous outbox callback, never both.
3. Select third-party providers as external dependencies after their identity,
   data residency, persistence, recovery and operational contracts have been
   accepted. Do not label a catalog entry as supported merely because it can
   carry bytes.
4. Add editor-specific adapters only after real schema fixtures cover local
   change, remote merge, duplicate/reorder/reconnect recovery, unsupported
   content, callback failure, resource exhaustion, and browser/device behavior.

The source-level integration guide is [Native Yjs editor bindings](../integration/yjs-native-editor-bindings.md).
