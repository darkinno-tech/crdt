# Opt-in WebSocket and HTTP/SSE relay reference

[English](EXTENSIONS.md) | [简体中文](EXTENSIONS.zh-CN.md)

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
authorization callbacks—is in [examples/extensions-provider](examples/extensions-provider):

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

## Feature switch and surfaces

`Config.Features` is a construction-time attack-surface switch, not a mutable
global setting. Create a new handler during deployment/config reload if a
surface must change; do not turn a live listener into a dynamic authorization
mechanism.

| Feature | Surface | Default |
| --- | --- | --- |
| `FeatureWebSocket` | `GET <mount>/ws`, subprotocol `crdt-sync-v1` | Disabled |
| `FeatureHTTP` | `POST` changes plus `GET` Server-Sent Events | Disabled |

For a mount of `/crdt/`, the HTTP routes are:

| Method | Path | Meaning |
| --- | --- | --- |
| `POST` | `/crdt/http/groups/{base64url(group-id)}/changes` | Publish one canonical change envelope (`application/octet-stream`). |
| `GET` | `/crdt/http/groups/{base64url(group-id)}/events` | Subscribe to a live SSE stream (`text/event-stream`). |

Use `ConnectHTTP` rather than building group paths in clients. It derives the
base64url path from the supplied `replica.Manifest`. `DialWebSocket` and
`ConnectHTTP` validate the exact manifest before accepting live data.

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

The reference intentionally adds WebSocket and HTTP/SSE before a gRPC adapter:
they exercise browser and ordinary HTTP integration without inventing an
unnegotiated gRPC streaming contract. A future gRPC transport should be a
separate manifest-bound feature with equivalent deadline, backpressure,
authorization, duplicate/reorder, and recovery tests.

## Design review matrix

| Dimension | Decision | Evidence / consequence |
| --- | --- | --- |
| Correctness | Exact manifest plus closed `ProtocolPolicy`; bounded `Inbox`; accepted-dot-only fan-out. | Mismatched schema/epoch/protocol and malformed frames fail before application mutation; retries and reordering converge. |
| Security | Default-off handler; app-owned authentication and separate read/write authorization; strict host patterns; compression off. | No anonymous endpoint appears merely by importing the package; cross-origin policy is shared across transports. |
| Performance | Fixed maximum frames and queues; slow peers are disconnected; HTTP publishing is a single bounded request. | Memory use is capped per peer; callers must benchmark their target workload before raising limits. |
| Availability | No hidden listener, storage, retry, or reconnect loop. | Host deployment controls lifecycle and can use its existing HTTP/TLS/observability stack. |
| Compatibility | New transport envelope uses subprotocol `crdt-sync-v1` and manifest negotiation, separate from CRDT frame versions. | Unknown transport versions and incompatible groups fail closed instead of being guessed. |

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
go test -run='^$' -fuzz=FuzzWireDecoders -fuzztime=10s ./extensions

# Loopback transport cost on the current machine; do not treat it as an SLA.
go test -run='^$' \
  -bench='Benchmark(GroupReceive|WebSocketPublish|HTTPPublish)Loopback$' \
  -benchmem ./extensions
```

The WebSocket and HTTP benchmarks wait for the publishing client to observe
its accepted live change before sending the next one. That measures a bounded
end-to-end loopback path; an unacknowledged firehose is intentionally subject
to the slow-peer queue limit and may be disconnected.

`make test-unit`, `make fuzz`, and `make coverage` include the extension
package. The repository coverage gate remains 90% per package.
