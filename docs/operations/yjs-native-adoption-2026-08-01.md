# Native Yjs adoption assessment — 2026-08-01

## Decision

Offer two deliberately separate collaboration paths:

1. **Yjs-native documents** use `Y.Doc`, a maintained editor binding, and the
   standard `y-websocket` provider through `extensions.YJSHandler`. They do
   not download Go/Wasm or negotiate a Go manifest.
2. **Go CRDT documents** keep the repository's manifest-bound frame protocol,
   Go/Wasm RGA, durable recovery, and Go-specific editor bindings.

The paths may share an authenticated application gateway, observability, and
product navigation. They must not exchange live updates, client IDs, cursor
positions, tombstones, or persistence records. A change of path is an offline,
one-way migration into a new identity and epoch.

This choice addresses the browser delivery concern directly: Yjs users get
the mature JavaScript editor/provider ecosystem; Go CRDT users retain the
protocol and persistence guarantees this library can actually validate.

## Evidence

- Yjs documents updates as commutative, associative, and idempotent and
  exposes state-vector diff APIs. Its provider ecosystem includes WebSocket
  and established editor integrations. See the [Yjs introduction](https://docs.yjs.dev/)
  and [document update API](https://docs.yjs.dev/api/document-updates).
- `y-websocket` is a conventional client/server provider that distributes
  document updates and awareness. It is a browser-native integration boundary,
  not a Go protocol. See the [provider documentation](https://docs.yjs.dev/ecosystem/connection-provider/y-websocket).
- The repository pins `yjs@13.6.31` and runs it only as the semantic store for
  an explicitly store-backed room. The Go relay keeps authentication,
  authorization, origin checks, queues, and client-facing resource bounds.

## Multi-dimensional assessment

| Dimension | Yjs-native path | Go CRDT path | Decision rule |
| --- | --- | --- | --- |
| Browser/editor delivery | Native JS, existing `y-*` bindings and providers; no Wasm startup or Go manifest. | Go/Wasm preserves this library's exact RGA semantics but needs runtime and protocol compatibility work. | Choose Yjs for mainstream editor adoption and provider reuse. |
| Correctness | Yjs state vectors, update format, shared types, and editor schema stay end-to-end in one engine. | Go frames, manifests, HLCs, and run-v2 semantics stay end-to-end in one engine. | Never translate live mutations across the two engines. |
| Security | Gateway authenticates before upgrade, authorizes read/write/presence, checks origin, disables compression, and bounds messages/queues. | Same application controls plus Go-specific manifest authorization. | Do not authenticate from a Yjs client ID; use cookies or a separately designed short-lived ticket. |
| Performance and capacity | Small local edits and provider fan-out are handled by Yjs; durable state-vector recovery adds one bounded sidecar call. | Avoids Node sidecar for Go documents; Wasm/download and framed-codec costs remain. | Measure each path at realistic document size and receiver count; do not infer capacity from codec microbenchmarks. |
| Operations | Store is loopback-only, single process per data directory; HA requires document partitioning or a store with cross-process serialization. | Existing Go persistence and replica-recovery operations apply only to Go documents. | Use Level 0 live relay when reconnect recovery is not required; enable Level 1 store only for an explicit recovery workload. |

## Safe native browser configuration

Use the shortest valid client path:

```ts
const document = new Y.Doc();
const provider = new WebsocketProvider("wss://collab.example/yjs", "notes", document);
```

This remains a native Yjs document. For CodeMirror plain text, the optional
[`@darkinno-tech/crdt-client/yjs` binding](../integration/yjs-native-editor-bindings.md)
adds bounded incremental projection without introducing Go frames, Go/Wasm, or
`native-ts-v1`; other editors can use their maintained Yjs binding directly.
The first production requirement is a same-origin Secure, HttpOnly session
cookie. Browser WebSocket APIs do not allow arbitrary authorization headers;
do not put a long-lived bearer credential in the provider query string. The
application must also keep rooms preconfigured, bind user/tenant/document
authorization at the gateway, and close connections when access is revoked.

`YJSStore` is not exposed to the browser. Its bearer token is restricted to
the trusted Go-to-loopback sidecar hop; V1 and V2 remain pinned per durable
document. A schema label fences the document identity but does not validate an
arbitrary ProseMirror, Quill, or custom schema.

## Validation matrix

| Scenario | What it proves | Gate |
| --- | --- | --- |
| Real direct sidecar V1/V2, merge, restart, malformed input | The maintained Yjs engine validates format, materializes snapshots, and preserves the last valid record. | `make yjs-store-test` |
| Real standard `y-websocket` provider through Go relay and Node store | Native client handshake, offline concurrent text/nested-type merge, awareness, and durable fresh-client recovery. `disableBc` prevents a same-process BroadcastChannel false positive. | `make yjs-store-test` |
| Go relay wire fuzz, race, and capacity tests | Invalid wrappers, duplicate/reordered opaque updates, awareness ownership, queues, and in-memory fan-out. | `go test -race ./extensions -run 'TestYJS'`; fuzz target in `extensions/yjs_test.go` |
| Controlled loopback store load at 1/4/16/64 receivers | Apply/diff/snapshot latency and average recovery bytes under bounded simulated receivers. It is not WAN, TLS, or production fan-out capacity. | `make yjs-store-benchmark` |

Before production, repeat the final row with the target CPU, Node memory limit,
document shapes, authenticated gateway, TLS terminator, and 1/4/16/64 real
browser clients. Record p50/p95/p99 reconnect and apply latency, CPU, heap,
queue drops, sidecar errors, and recovery-byte distribution. Local pass/fail
tests do not prove provider traffic through a production network.

## Controlled development measurements

On 2026-08-01, the matrix ran on an Apple M4 Pro with Node v26.5.0, a 4 KiB
initial `Y.Text`, 40 measured incremental edits after five warmups, loopback
HTTP, and the store's 1 MiB update / 16 MiB snapshot limits:

| Simulated receivers | Apply p50 / p95 (ms) | Diff p50 / p95 (ms) | Snapshot p50 / p95 (ms) | Mean diff bytes |
| --- | --- | --- | --- | --- |
| 1 | 10.267 / 11.203 | 1.933 / 2.189 | 1.818 / 2.070 | 22.875 |
| 4 | 10.084 / 11.183 | 1.781 / 2.041 | 1.787 / 1.918 | 22.875 |
| 16 | 9.672 / 10.836 | 1.691 / 1.931 | 1.711 / 1.953 | 20.875 |
| 64 | 10.547 / 19.722 | 1.646 / 2.224 | 1.691 / 2.266 | 22.875 |

The Go relay's local opaque-wrapper decode plus duplicate-aware admission
benchmark, constrained to one logical processor and run three times, measured
115.0–118.1 ns/op, 136 B/op, and 3 allocs/op. These measurements exclude TLS,
browser rendering, authentication database work, WAN behavior, and actual
WebSocket fan-out. They are a development baseline, not a production capacity
claim.

## Explicit non-goals

- No Yjs-to-Go-RGA, rich-text, awareness, or cursor conversion.
- No claim that a checksum, state vector, WebSocket write, or provider `sync`
  event authenticates a user, proves peer receipt, or creates a business
  transaction.
- No persistence of Yjs awareness or use of a Level 0 opaque history as a
  durable recovery log.
