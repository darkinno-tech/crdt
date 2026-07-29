# WebSocket provider reference

`examples/websocket-provider/provider` is the repository's official WebSocket
transport reference. It is deliberately separate from the CRDT core and from
`cmd/crdt-sync-probe`: the probe remains a short-lived HTTP test tool, whereas
this package shows how an application can mount a manifest-bound WebSocket
endpoint and connect Go clients.

It is still a reference implementation, not a production replication service.
It has no durable operation log, snapshot store, reconnect loop, outbox,
anti-entropy protocol, member authority, or tombstone-GC policy. Its caller
owns TLS, identity, authorization, persistence, retry, monitoring, and
incident response.

## What the reference does

- Authenticates the HTTP request before the WebSocket upgrade.
- Requires the `crdt-sync-v1` subprotocol and rejects cross-origin browser
  requests unless `OriginPatterns` explicitly allows the origin.
- Exchanges an exact `replica.Manifest` before accepting any CRDT change.
- Accepts only bounded binary messages, validates every embedded canonical
  delta with `replica.NewChangeWithPolicy`, and lets a bounded
  `replica.Inbox` preserve a contiguous per-actor frontier.
- Requires an application `Authorize` callback that binds the authenticated
  peer to the proposed logical actor.
- Suppresses relay of a Dot that is already installed or buffered. This
  contains conflicting retries, but a production operation store must still
  persistently bind each actor/counter to its canonical payload.
- Uses a bounded per-peer write queue. A peer that cannot keep up is closed;
  it cannot make broadcast delivery retain unbounded memory.

The application-supplied `Apply` callback still has to decode the concrete
CRDT with limits that fit the replication group. A valid outer frame checksum
does not authenticate the source or validate the delta's type-specific
payload.

## Run the complete reference

```sh
go run ./examples/websocket-provider
go test -race ./examples/websocket-provider/...
```

The command starts an in-process HTTP/WebSocket endpoint, connects two Go
replicas, sends the second dot before the first, then retries the first dot.
Its expected output is:

```text
relay-value=5
left-value=5
right-value=5
frontier-operator-a=2
duplicate-and-out-of-order-safe=true
```

## Mount a handler

Construct the application state and its manifest first. The manifest must bind
one CRDT protocol, schema, codec, epoch, and semantic version. A production
restore must provide the `Frontier` saved atomically with that CRDT state.

```go
group, err := provider.NewGroup(provider.GroupConfig{
	Manifest:          manifest,
	Frontier:          restoredFrontier,
	MaxPendingChanges: 256,
	MaxPendingBytes:   1 << 20,
	Apply: func(encoded []byte) error {
		delta, err := counter.UnmarshalGCounterDeltaWithLimits(encoded, receiveLimits)
		if err != nil {
			return err
		}
		return sharedCounter.ApplyDelta(delta)
	},
})
if err != nil {
	return err
}

handler, err := provider.NewHandler(provider.Config{
	Groups: []*provider.Group{group},
	Authenticate: func(request *http.Request) (provider.Peer, error) {
		// Verify session/JWT/mTLS identity. Do not trust a client actor header.
		return provider.Peer{ID: authenticatedSubject(request)}, nil
	},
	Authorize: func(peer provider.Peer, _ replica.Manifest, dot replica.Dot) error {
		if !actorBelongsToSubject(peer.ID, dot.Actor) {
			return provider.ErrUnauthorized
		}
		return nil
	},
	OriginPatterns: []string{"app.example.com"},
})
if err != nil {
	return err
}
http.Handle("/crdt", handler)
```

The example imports this package as:

```go
import provider "github.com/DarkInno/crdt/examples/websocket-provider/provider"
```

The reference pins `github.com/coder/websocket` in the repository module. Its
v1.8.13 pin keeps the repository's Go 1.21 language minimum; revisit that pin
and its security posture when updating the provider or raising the supported Go
version.

## Connect and publish from Go

Each client owns its local concrete state plus a manifest-compatible inbox. The
provider invokes `OnChange` for each newly accepted broadcast. It does not echo
a Dot the relay already knows, so callers must apply their local CRDT change
and persist its outbox entry before calling `Publish`.

```go
inbox, err := replica.NewInbox(manifest, restoredFrontier, 256, 1<<20, applyDelta)
if err != nil {
	return err
}
client, err := provider.Dial(ctx, "wss://sync.example.com/crdt", manifest, provider.ClientConfig{
	Header: http.Header{"Authorization": []string{"Bearer " + accessToken}},
	OnChange: func(change replica.Change) error {
		_, err := inbox.Receive(change)
		return err
	},
})
if err != nil {
	return err
}
defer client.Close()

encoded, err := delta.MarshalBinary()
if err != nil {
	return err
}
change, err := replica.NewChange(manifest, replica.Dot{
	Actor: durableActorID, Counter: nextDurableSequence,
}, encoded)
if err != nil {
	return err
}
if err := client.Publish(ctx, change); err != nil {
	// Persist and retry from an application-owned outbox when appropriate.
	return err
}
```

`Actor` and `Counter` are delivery identities, not HLC tags. Allocate and
persist them with the outgoing mutation/outbox; an in-memory counter reused
after restart can collide with an earlier dot. Never use a client-selected
actor unless the authorization callback has verified that it belongs to the
authenticated principal.

## Wire contract

After the WebSocket handshake chooses `crdt-sync-v1`, the client sends one text
hello message and the server replies with one:

```json
{"version":1,"manifest":{"GroupID":"...","SchemaID":"...","Epoch":1,"Protocol":{"StateID":1,"DeltaID":2,"CodecID":"","SemanticsVersion":1}}}
```

The provider compares the decoded manifest with `Manifest.Compatible`; a
group, schema, epoch, codec, frame ID, or semantic-version mismatch is
rejected before data delivery.

Every later message is binary and uses this canonical envelope:

```text
1 byte      provider wire version (1)
uvarint     UTF-8 actor byte length
bytes       actor
uvarint     non-zero per-actor counter
uvarint     canonical CRDT delta-frame byte length
bytes       canonical CRDT delta frame
```

The envelope rejects non-canonical, truncated, oversized, empty, and
trailing-byte encodings. It is a transport envelope, not a replacement for
the library frame, CRDT-specific decoder, snapshot format, or membership
protocol.

## Controlled benchmark evidence

The controlled Linux/amd64 results for duplicate admission and end-to-end
loopback fan-out are recorded in the [2026-07-29 benchmark report](../operations/benchmark-2026-07-29.md).
Each fan-out operation waits for every observer to decode and install the
change; it is not a WAN latency, browser, TLS, durable-store, or production
capacity claim.

## Required production work

Before using this pattern outside a controlled integration environment, the
embedding service must add and verify all of the following:

| Boundary | Application responsibility |
| --- | --- |
| Transport security | Terminate TLS and use `wss`; set origin policy intentionally and apply ingress limits/timeouts. |
| Identity and authorization | Authenticate before upgrade; map every actor to a permitted subject/group; revoke or revalidate long-lived sessions as required. |
| Durable delivery | Persist mutation, actor counter, CRDT state, and delivery frontier in the required transaction; implement reconnect, outbox retry, deduplication, and recovery. |
| Bootstrap and anti-entropy | Send a validated snapshot/checkpoint to a new or rejoining member and repair gaps that a live WebSocket session cannot replay. |
| Decoder and retention limits | Set transport, frame, CRDT object, pending-inbox, queue, rate, and document limits from real workload and adversarial-input budgets. |
| Membership and GC | Install an authenticated authoritative membership view and use exact epoch-bound acknowledgements before any tombstone compaction. |
| Operations | Add observability, overload behavior, backpressure policy, backups, deployment/rollback procedures, and production failure testing. |

Passing the runnable example or its tests proves only this in-memory reference
flow. It does not prove browser/mobile compatibility, a real identity provider,
TLS deployment, durable transactions, recovery, or production capacity.
