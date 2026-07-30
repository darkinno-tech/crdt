# Durable WebSocket relay reference

`durable` is the production-oriented counterpart to the deliberately bounded
live-only [`extensions`](extensions.md) reference. It implements a
manifest-bound WebSocket relay with a persistent operation log, ordered replay,
and reconnect support. It is a reference for one process and one persistent
volume—not a clustered replication service.

Read the [design and operational boundary](../design/durable-transport.md)
before enabling it. In particular, a replay cursor is valid only after the
application has atomically persisted its concrete CRDT state and delivery
frontier.

## Guaranteed transport flow

```text
client local state + outbox --publish--> validate/authz --> durable append
                                                    |             |
                                                    |             +--> replay log
                                                    v
                                              live peer queues
                                                    |
                                        persist local state + cursor
                                                    |
                                      reconnect from durable cursor
```

The server stores an immutable canonical binding for every accepted
`(group, actor, counter)`. Identical retries return the original sequence;
conflicting retries fail. It broadcasts only after the append commits. A crash
between the commit and broadcast therefore causes replay rather than loss.

## Mount a group

The application supplies the manifest, authentication, independent write/read
authorization, and a concrete bounded decoder. The validator must not mutate
application state.

```go
store, err := durable.OpenStore("/var/lib/crdt/relay.db", durable.StoreConfig{
	MaxEvents: 1_000_000,
	MaxBytes:  4 << 30,
})
if err != nil {
	return err
}

group, err := durable.NewGroup(durable.GroupConfig{
	Manifest: manifest,
	Validate: func(encoded []byte) error {
		_, err := counter.UnmarshalGCounterDeltaWithLimits(encoded, receiveLimits)
		return err
	},
})
if err != nil {
	return err
}

handler, err := durable.NewHandler(durable.Config{
	Store:  store,
	Groups: []*durable.Group{group},
	Authenticate: func(r *http.Request) (durable.Peer, error) {
		return verifySession(r)
	},
	Authorize: func(peer durable.Peer, _ replica.Manifest, dot replica.Dot) error {
		if !actorBelongsToSubject(peer.ID, dot.Actor) {
			return durable.ErrUnauthorized
		}
		return nil
	},
	AuthorizeSubscription: func(peer durable.Peer, _ replica.Manifest) error {
		return mayReadGroup(peer.ID, manifest.GroupID)
	},
})
if err != nil {
	return err
}
mux.Handle("/crdt/durable/", http.StripPrefix("/crdt/durable", handler))
```

The server exposes `GET /ws` with the `crdt-durable-v1` subprotocol. Its
`bbolt` file is opened with an exclusive lock, but deployment must still ensure
that one active pod/process owns the persistent volume. Do not mount the same
file into multiple replicas. `durable.Config.Store` is a `durable.Log`, so a
highly available deployment can use the Redis or PostgreSQL implementations in
[the provider architecture guide](provider-architecture.md), provided it keeps
the same transaction contract. `MaxEvents` and
`MaxBytes` apply per configured replication group; put a fixed per-tenant
group quota in front of a multi-tenant service.

## Durable receive and reconnect

`ReconnectClient` reconnects with exponential backoff and replays from the
cursor returned by `Cursor()`. `OnEvent` must apply the event through a
manifest-compatible `replica.Inbox`, then atomically store the concrete CRDT
state, inbox frontier, outbox state, and `event.Sequence`. Return an error when
that transaction cannot commit; the client will not advance its cursor and the
same event will replay after reconnect.

```go
client, err := durable.NewReconnectClient(endpoint, manifest, durable.ClientConfig{
	Header: bearerHeader(accessToken),
	Cursor: loadDurableCursor(),
	OnEvent: func(event durable.Event) error {
		// applyAndCheckpoint persists CRDT state, inbox frontier, and event.Sequence
		// in one application-owned transaction.
		return applyAndCheckpoint(event)
	},
})
if err != nil {
	return err
}
go client.Run(ctx)

// First insert the change into the application outbox transaction, then queue
// its original canonical Dot and delta. Delete the outbox row only after a
// durable application receipt policy says it is safe.
if err := client.Publish(ctx, change); err != nil {
	return err
}
```

If replay exceeds the configured window, the client receives
`ErrReplayUnavailable`. It must bootstrap from a validated checkpoint instead
of resetting the cursor or accepting a partial event stream.

## Validation

```sh
go test ./durable
go test -race ./durable
go test -run='^$' -fuzz=FuzzWire -fuzztime=10s ./durable
go test -run='^$' -bench='Benchmark(DurableAppend|DurableReplay|Reconnect)' -benchmem ./durable
```

These checks include real loopback WebSockets, restart/replay, connection-drop
reconnect, duplicate/out-of-order simulation, and a local file-backed store.
They do not prove a live TLS deployment, an external identity provider, a
multi-node store, client checkpoint code, or a tombstone-GC policy.
