# Opt-in WebSocket and HTTP/SSE relay reference

[English](extensions.md) | [简体中文](extensions.zh-CN.md)

`extensions` is an official, manifest-bound live-relay reference for this Go
module. It is deliberately opt-in: the zero feature set exposes no endpoint,
does not call authentication, and starts no listener or background work.

It lowers the first integration step to an application-owned mux plus one
feature expression:

```go
handler, err := extensions.NewHandler(extensions.Config{
	Features: extensions.FeatureWebSocket | extensions.FeatureHTTP,
	Groups:   []*extensions.Group{group},
	// Authenticate, Authorize, and AuthorizeSubscription are required here.
	// Telemetry is optional and records bounded, payload-free local events.
})
if err != nil {
	return err
}
if err := handler.Mount(mux, "/crdt/"); err != nil {
	return err
}
```

`group` is a manifest-bound `extensions.Group` whose `Apply` callback belongs
to the host application. The complete, runnable setup—including both required
authorization callbacks—is in [examples/extensions-provider](../../examples/extensions-provider):

```sh
go run ./examples/extensions-provider
```

Expected output:

```text
websocket_to_http=2
http_to_websocket=5
relay=5
```

The example uses `httptest` so it is self-contained. A production host mounts
the same handler in its own `http.Server`, configures TLS, and uses `wss://`
from browsers.

## Local operational telemetry

Set `Config.Telemetry` to an explicitly constructed bounded
`telemetry.Reporter` when the host needs local operational signals. The relay
records `handshake`, `append`, and `append_batch` outcomes, elapsed time, and a
stable error code. Events never contain CRDT payloads, group IDs, actor IDs, or
credentials.

Reporting is asynchronous and intentionally lossy under sink pressure. It is
not a CRDT protocol message, delivery receipt, audit log, or recovery source.
For queue sizing, sink behavior, and production examples, see
[production readiness](../operations/production-readiness.md).

## Feature switch and surfaces

`Config.Features` is a construction-time attack-surface switch, not a mutable
global setting. Create a new handler during deployment/config reload if a
surface must change; do not turn a live listener into a dynamic authorization
mechanism.

| Feature | Surface | Default |
| --- | --- | --- |
| `FeatureWebSocket` | `GET <mount>/ws`, subprotocol `crdt-sync-v1` | Disabled |
| `FeatureWebSocketBatch` | negotiated `crdt-sync-v2` batch envelope over WebSocket | Disabled |
| `FeatureHTTP` | `POST` changes plus `GET` Server-Sent Events | Disabled |

For a mount of `/crdt/`, the HTTP routes are:

| Method | Path | Meaning |
| --- | --- | --- |
| `POST` | `/crdt/http/groups/{base64url(group-id)}/changes` | Publish one canonical change envelope (`application/octet-stream`). |
| `GET` | `/crdt/http/groups/{base64url(group-id)}/events` | Subscribe to a live SSE stream (`text/event-stream`). |

Use `ConnectHTTP` rather than building group paths in clients. It derives the
base64url path from the supplied `replica.Manifest`. `DialWebSocket` and
`ConnectHTTP` validate the exact manifest before accepting live data.
A successful client return is also the live-subscription linearization point:
the relay registers the peer before it sends that confirmation, so a caller may
publish immediately without a post-handshake event window.

## Opt-in WebSocket batch

`FeatureWebSocketBatch` is disabled by default and is valid only together with
`FeatureWebSocket`. When both the relay and a client explicitly enable it,
their WebSocket handshake can negotiate `crdt-sync-v2`; otherwise the client
falls back to `crdt-sync-v1` and `PublishBatch` returns `ErrBatchUnsupported`.

A batch is transport coalescing, not an atomic CRDT or storage transaction.
Every item retains its own dot and canonical v1 envelope. The relay validates
and authorizes every item before the first Inbox mutation, then admits items in
order. If an application callback or pending-state limit rejects a later item,
the relay forwards every earlier accepted item before closing the publishing
connection. The caller must retain each original item in its durable outbox and
retry independently after an ambiguous result.

Batch messages are bounded by `MaxMessageBytes` and `MaxBatchChanges`. The
default is 16 items and it cannot exceed `MaxQueuedMessages`, so a v1 WebSocket
or SSE peer receives all queued item messages or is disconnected without an
application-level partial queue insertion. The client and relay each enforce
their configured item bound; keep them compatible (the defaults match).
Batch-capable WebSocket peers receive one batch envelope; HTTP and SSE stay on
their documented single-change v1 envelope.

## What the reference guarantees

Each connection first negotiates a versioned manifest. Each accepted change is
bounded, decoded as a canonical CRDT frame, checked against that manifest and
its `ProtocolPolicy`, then authorized before `Group.Apply` mutates state.
`replica.Inbox` supplies bounded duplicate and out-of-order handling. A relay
forwards an accepted dot once; an installed or already-buffered duplicate is
not amplified to live peers.

`GroupConfig.Apply` is called while per-group delivery ordering is held. It
must use a bounded concrete decoder, leave application state unchanged on an
error, and must not re-enter `Group` or block on a transport callback.

The implementation separates read and write authorization:

- `Authenticate` establishes an application identity before an upgrade or body
  read.
- `Authorize` receives that identity, the exact manifest, and proposed dot.
  At minimum, bind the authenticated identity to the CRDT actor.
- `AuthorizeSubscription` controls access to live events independently of
  write access.

Default limits are intentionally conservative: a 1 MiB message, 128-byte
actor ID, 16 queued messages or 4 MiB per peer, and 10-second handshake and
write deadlines. A full peer queue is closed and removed rather than allowed
to grow. WebSocket compression is disabled. Tune limits only after measuring
the real message mix and retaining the same bounded failure behavior.

For browser-originated requests, the request host is allowed by default.
`OriginPatterns` may additionally contain case-insensitive **host** glob
patterns such as `app.example` or `*.example.internal`; it must not contain a
scheme, path, query, fragment, blank value, or `*`. The same host-pattern rule
is enforced by HTTP/SSE and WebSocket so their cross-origin behavior cannot
drift. Non-browser clients without an `Origin` header still need normal
authentication and authorization.

## Deliberate production boundary

This is a live relay reference, not a durable replication service and not a
replacement for `crdt-sync-probe`'s explicitly non-production diagnostics. It
has no operation log, snapshot store, persistent outbox, replay endpoint,
automatic reconnection, anti-entropy loop, membership authority, TLS listener,
or token/session implementation. HTTP/SSE publishes one change and streams
only future live events; it does not recover missed events.

The host application must therefore own all of the following:

1. Durable CRDT state plus the corresponding `replica.Frontier` in the same
   transaction, and a durable outbox/receipt policy.
2. Startup recovery from a checkpoint plus state/Merkle anti-entropy for
   missed history.
3. TLS, authentication/session lifecycle, per-tenant group lookup, rate
   limits, abuse controls, observability, and capacity planning.
4. Authorized membership, checkpoint distribution, replica retirement, and
   tombstone lifecycle.

## Native gRPC relay

`extensions.Relay` is a native bidirectional gRPC service for Go services and
other gRPC clients. Its generated schema is
[`extensions/relay.proto`](../../extensions/relay.proto). It does not reuse the
WebSocket subprotocol or introduce another CRDT envelope: the first message in
each direction is the exact encoded `replica.Manifest`, and every later message
contains the existing canonical change envelope.

```go
server, relay, err := extensions.NewGRPCServer(extensions.GRPCConfig{
	Groups: []*extensions.Group{group},
	Authenticate: func(ctx context.Context) (extensions.Peer, error) {
		// Derive identity from mTLS, a trusted interceptor, or authenticated metadata.
	},
	Authorize:             authorizeWrite,
	AuthorizeSubscription: authorizeRead,
	Telemetry:             reporter, // Optional bounded, payload-free local events.
})
_ = relay
_ = server // Serve with the application's listener and TLS credentials.
```

For an application-owned shared `grpc.Server`, build `NewGRPCRelay`, pass
`relay.ServerOptions()` when creating that server, then call
`extensions.RegisterRelayServer(server, relay)`. This preserves gRPC's native
interceptors, TLS/mTLS, health service, observability, and shutdown ownership.
`GRPCAuthenticate` receives `context.Context`, so metadata must be treated as
untrusted until the host validates its credentials; it must never use the CRDT
actor as an identity.

Go applications can use the managed `OpenGRPC` client after dialing their own
credentialed `grpc.ClientConn`. It validates the local and remote manifests,
sets the same bounded message limits as the relay, serializes sends, and
delivers validated changes to the callback:

```go
streamContext, cancel := context.WithCancel(context.Background())
defer cancel() // also cancel on application shutdown or reconnect.

client, err := extensions.OpenGRPC(
	streamContext,
	extensions.NewRelayClient(connection), // connection owns TLS/mTLS and credentials
	manifest,
	extensions.GRPCClientConfig{OnChange: func(change replica.Change) error {
		_, err := inbox.Receive(change)
		return err
	}},
)
if err != nil { /* handle failed live handshake */ }
defer client.Close() // does not close connection
```

The stream context is deliberately the lifetime and deadline control for the
whole RPC. Do not pass a short handshake-only timeout to `OpenGRPC`, because
gRPC would cancel the established subscription when that deadline expires.
`Publish` checks its supplied context before queuing behind another send, but a
currently blocked HTTP/2 send is released by the stream context; choose that
deadline from realistic network and shutdown constraints.

The first response is sent only after the subscription is registered. As a
result, the completed client handshake is the live-subscription linearization
point, exactly as it is for WebSocket. `Group` still supplies manifest/policy
validation, authorisation, bounded duplicate/out-of-order `Inbox` processing,
and accepted-dot-only fan-out. Per-stream application queues remain bounded;
gRPC HTTP/2 flow control is helpful transport backpressure but is not a memory
retention policy. A full queue disconnects the slow stream.

`GRPCConfig.Telemetry` uses the same bounded reporter as the HTTP/WebSocket
handler. It records only `handshake` and `append` outcome, duration, and stable
error code; it never includes manifests, peer identities, metadata, or CRDT
payloads.

Use explicit, realistic RPC deadlines and stop work when the stream context is
cancelled. A successful `Send` only means gRPC accepted the message for its
transport pipeline, not that the peer durably stored it. Keep a durable outbox,
recover from snapshots/frontiers, and use anti-entropy after reconnect exactly
as with the WebSocket/HTTP relay.

## Design review matrix

| Dimension | Decision | Evidence / consequence |
| --- | --- | --- |
| Correctness | Exact manifest plus closed `ProtocolPolicy`; bounded `Inbox`; accepted-dot-only fan-out. | Mismatched schema/epoch/protocol and malformed frames fail before application mutation; retries and reordering converge. |
| Security | Default-off handler; app-owned authentication and separate read/write authorization; strict host patterns; compression off. | No anonymous endpoint appears merely by importing the package; cross-origin policy is shared across transports. |
| Performance | Fixed maximum frames and queues; slow peers are disconnected; HTTP publishing is a single bounded request. | Memory use is capped per peer; callers must benchmark their target workload before raising limits. |
| Availability | No hidden listener, storage, retry, or reconnect loop. | Host deployment controls lifecycle and can use its existing HTTP/TLS/observability stack. |
| Compatibility | WebSocket uses `crdt-sync-v1`; gRPC uses generated `Relay.Sync`; both negotiate the same exact Manifest and carry the same CRDT envelope. | Unknown transport versions and incompatible groups fail closed instead of being guessed. |

## Validation commands

The suite distinguishes local loopback integration from deterministic network
simulation; neither claim proves a browser, external identity provider, or a
production database transaction.

```sh
# Unit, real loopback WS/HTTP/SSE, duplicate/reorder, concurrency, and sample.
go test ./extensions ./examples/extensions-provider

# Shared-state and connection lifecycle races.
go test -race ./extensions

# Bounded parser robustness.
go test -run='^$' -fuzz=FuzzWireDecoders -fuzztime=250000x ./extensions

# Loopback transport cost on the current machine; do not treat it as an SLA.
go test -run='^$' \
  -bench='Benchmark(GroupReceive|WebSocket(Batch)?Publish|HTTPPublish)Loopback$' \
  -benchmem ./extensions
```

The WebSocket and HTTP benchmarks wait for the publishing client to observe
its accepted live change before sending the next one. That measures a bounded
end-to-end loopback path; an unacknowledged firehose is intentionally subject
to the slow-peer queue limit and may be disconnected.

`make test-unit`, `make fuzz`, and `make coverage` include the extension
package. The repository coverage gate remains 90% per package.
