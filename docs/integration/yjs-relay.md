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

## Mount one explicit room

Rooms are configured before serving. An untrusted URL cannot create retained
server state.

```go
room, err := extensions.NewYJSRoom(extensions.YJSRoomConfig{
    Name:            "notes",
    MaxUpdateBytes:  1 << 20,
    MaxHistoryBytes: 8 << 20,
    MaxUpdates:      256,
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
retaining an unbounded application queue. The latest awareness client ID is
bound to the authenticated connection; equal or older relayed states from
other connections remain accepted because `y-websocket` intentionally
re-broadcasts them. A newer competing state closes the publisher.

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
go test ./extensions -run 'TestYJS|FuzzUnmarshalYJSMessages'
go test -race ./extensions -run 'TestYJS'
go test -run '^$' -bench='BenchmarkYJSWireDecodeAndAdmission$' -benchmem -benchtime=1s ./extensions
```

The benchmark measures local wire decoding and duplicate-aware in-memory
admission only. It is not a browser, TLS, WAN, durable-store, or service
capacity claim. Repeat it with production CPU, limits, document sizes, and a
Yjs-aware persistence implementation before choosing deployment limits.
