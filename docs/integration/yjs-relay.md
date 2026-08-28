# Yjs / y-protocols compatibility relay

`extensions.NewYJSHandler` is an opt-in, bounded WebSocket relay compatible
with the stable `y-websocket` / `y-protocols` message envelope. It is a
separate surface from the module's framed CRDT protocols: it **does not**
convert RGA, rich-text, snapshots, manifests, or Go `awareness` state into a
Yjs document.

That separation is intentional. Yjs updates contain Yjs-specific client IDs,
struct clocks, delete sets, and shared-type semantics. Treating an opaque Yjs
update as a run-v2 RGA delta would corrupt one side or falsely claim recovery
guarantees that do not exist.

## What is compatible

The handler accepts binary messages in the standard `y-websocket` layout:

| Top-level type | Inner meaning | Relay behavior |
| --- | --- | --- |
| `0` | sync Step 1 | Replies with a valid empty Step 2; retained updates are then sent as normal update messages. |
| `0` | sync Step 2 or update | Retains the bounded opaque Yjs update and fans it out. |
| `1` | awareness | Validates the y-protocols awareness wrapper and fans out latest ephemeral state. |
| `3` | awareness query | Returns the room's latest bounded awareness states. |

This has been exercised with `yjs@13.6.31`, `y-websocket@2.1.0`, and the
official y-protocols-aware provider: two Node clients completed initial sync,
replicated a `Y.Text` mutation, and propagated awareness. It remains a live
relay, not a proof that arbitrary untrusted Yjs update bytes are semantically
valid.

## Native browser path: no Go/Wasm or manifest negotiation

For a document that chooses Yjs as its collaboration contract, use the
standard Yjs client and an editor binding that works on that same `Y.Doc`.
The browser does not need a Go/Wasm runtime, frame decoder, or
`replica.Manifest`. It may use a maintained upstream binding directly, or the
optional native [`@darkinno-tech/crdt-client/yjs` CodeMirror binding](yjs-native-editor-bindings.md),
which still operates on Yjs updates and y-protocols awareness rather than the
repository's Go or `native-ts-v1` protocols.

```ts
import * as Y from "yjs";
import { WebsocketProvider } from "y-websocket";

const document = new Y.Doc();
const provider = new WebsocketProvider(
  "wss://collab.example/yjs",
  "notes",
  document,
);

const text = document.getText("shared");
// Bind `text` with the maintained adapter for the selected editor, for example
// y-prosemirror, y-quill, or a product-owned schema-preserving binding.
```

For a bounded incremental CodeMirror plain-text surface, use the optional
native binding with this same `document` and `Y.Text`; do not attach a second
transport owner to the document. Its limits, cursor model, and rich-text
boundary are documented in the [native editor binding guide](yjs-native-editor-bindings.md).

Mounting `YJSHandler` below `/yjs/` makes this connect to `/yjs/notes`; no
adapter protocol is required. Use a same-origin (or appropriately scoped)
Secure, HttpOnly session cookie for browser authentication. Browser WebSockets
cannot add an `Authorization` header, and a long-lived bearer token in
`WebsocketProvider` query parameters leaks too easily through URLs, proxy
logs, and diagnostics. If a cross-site deployment cannot use a cookie, issue
a very short-lived, room-bound, single-use connection ticket from an
authenticated HTTPS endpoint and redact it at every proxy; that ticket design
is application-owned and needs its own replay protection.

The handler's `Authenticate` callback still runs before upgrade, so cookie
verification remains ordinary application authentication:

```go
Authenticate: func(request *http.Request) (extensions.Peer, error) {
    session, err := authenticateSessionCookie(request)
    if err != nil {
        return extensions.Peer{}, extensions.ErrUnauthorized
    }
    return extensions.Peer{ID: session.Subject}, nil
},
```

Keep the provider URL and room selection in product configuration. Do not let
a page choose an unconfigured room, store identity, schema, epoch, or byte
limit.

## Mount one explicit room

Rooms are configured before serving. An untrusted URL cannot create retained
server state.

```go
room, err := extensions.NewYJSRoom(extensions.YJSRoomConfig{
    Name:                   "notes",
    MaxUpdateBytes:         1 << 20,
    MaxHistoryBytes:        8 << 20,
    MaxUpdates:             256,
    MaxAwarenessTombstones: 256, // clock-only metadata; zero selects this default
})
if err != nil { return err }

handler, err := extensions.NewYJSHandler(extensions.YJSConfig{
    Rooms: []*extensions.YJSRoom{room},
    Authenticate: func(request *http.Request) (extensions.Peer, error) {
        // Validate session, JWT, mTLS identity, or trusted proxy auth.
        // Do not derive identity from a Yjs client ID.
    },
    AuthorizeSubscription: func(peer extensions.Peer, room string) error {
        // Check tenant/document read access.
    },
    RevalidateSubscription: func(ctx context.Context, peer extensions.Peer, room string) error {
        // Recheck the authenticated peer against current read access. Return
        // an error for revocation, policy-backend failure, or timeout.
    },
    RevalidateInterval: 30 * time.Second,
    RevalidateTimeout:  2 * time.Second,
    Authorize: func(peer extensions.Peer, room string, kind extensions.YJSMessageKind) error {
        // Check document write/presence permission independently.
    },
    OriginPatterns: []string{"app.example.com"},
})
if err != nil { return err }

mux.Handle("/yjs/", http.StripPrefix("/yjs", handler))
// y-websocket connects to wss://host/yjs/notes.
```

The handler disables per-message compression, imposes read, message, queue,
history, update, and awareness-client limits, and drops a slow peer instead of
retaining an unbounded application queue. An active awareness client ID is
bound to its exact WebSocket, not merely the authenticated principal, so one
user's second browser tab survives the first tab disconnecting. The relay
accepts the standard equal-clock `null` removal and retains only bounded
clock/owner tombstones (no awareness JSON) to prevent a delayed pre-removal
state from resurrecting a ghost cursor. Equal or older non-null retransmits are
harmlessly ignored; a current competing state from another connection closes
that publisher.

`AuthorizeSubscription` controls the upgrade. If the product can revoke read
access while a WebSocket remains open, also configure
`RevalidateSubscription`: its callback receives the immutable authenticated
`Peer`, room, and a bounded context. A callback error or timeout closes the
subscriber and drops queued fan-out work; an already in-flight network write
cannot be recalled. The post-revocation window is bounded by
`RevalidateInterval + RevalidateTimeout`. With the callback present, a zero
interval selects one minute and a zero timeout selects five seconds (capped at
a shorter selected interval); providing a duration without the callback is
rejected as invalid
configuration. Recheck application authorization by peer and room, never by a
Yjs client ID, state vector, awareness payload, or stale cookie read.

## Persistence and recovery boundary

The room keeps complete update messages only until `MaxUpdates` or
`MaxHistoryBytes` is reached. It cannot parse, merge, compact, snapshot, or
authorize Yjs document internals. A full history is rejected rather than
silently evicted: eviction would make a reconnecting client appear synced
while missing causal data.

For Level 1 recovery, configure a room with the shipped Yjs-aware
[`YJSStore`](yjs-store.md) capability. A store-backed room uses a durable state
vector and semantic diff instead of this opaque history; it validates/applies
the selected V1/V2 update encoding, creates a merged snapshot/update, and
retains a recovery cursor. Persisted document lifecycle, subdocuments, access
revocation, rate limits, TLS, backup/restore, and abuse protection remain host
responsibilities.

The Go [`awareness`](../protocol/awareness-v1.md) package is a different,
authenticated Go-provider protocol. Do not mix its updates with Yjs awareness
bytes or persist either as a CRDT document update.

There is intentionally no generic Go-awareness ↔ Yjs-awareness switch on
`YJSHandler`. The protocols have separate authenticated identities
(`actor` versus client ID), independent monotonic clocks, and potentially
different cursor schemas. Reusing either identity or clock would allow an
equal-clock conflict, a competing-client ownership failure, or a reconnection
to overwrite presence. A product that really needs federation must implement
an application gateway with an explicit tenant/room/epoch binding, an
injective external-client-ID allocation, target-specific monotonic clocks,
per-direction authorization, bounded fan-out, and a loss policy for cursor
metadata. Treat it as a new presence capability with real client
interoperability tests, not as a transparent relay option.

## gRPC is already native, not a Yjs transport

The repository's existing [`extensions.Relay`](extensions.md#native-grpc-relay)
is a generated bidirectional gRPC service for manifest-bound Go CRDT changes.
It exchanges an exact `replica.Manifest` followed by canonical change
envelopes, with mandatory authentication, read/write authorization, bounded
queues, telemetry, and client/server tests. It deliberately does not carry
Yjs update bytes, because its recovery and authorization contract differs.

## Verification and performance scope

Run the focused contract checks:

```sh
(cd extensions && go test . -run 'TestYJS|FuzzUnmarshalYJSMessages')
(cd extensions && go test -race . -run 'TestYJS')
(cd extensions && go test -run '^$' -bench='BenchmarkYJSWireDecodeAndAdmission$' -benchmem -benchtime=1s .)
```

The benchmark measures local wire decoding and duplicate-aware in-memory
admission only. It is not a browser, TLS, WAN, durable-store, or service
capacity claim. Repeat it with production CPU, limits, document sizes, and a
Yjs-aware persistence implementation before choosing deployment limits.
