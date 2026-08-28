# Awareness / presence v1

`awareness` is a bounded, ephemeral protocol for information such as online
state, display name, colour, and a relative text cursor. It is intentionally
**not** a CRDT document protocol: it does not appear in `ProtocolPolicy`, a
`replica.Manifest`, `replica.Frontier`, state snapshots, anti-entropy, or
tombstone-GC checkpoints. Persisting it would turn a liveness hint into stale
application data.

The Go package is [`awareness`](../../awareness). The reference WebSocket
provider exposes it only with the opt-in `crdt-sync-v3` subprotocol; v1 and v2
CRDT change envelopes remain unchanged.

## Contract

- One authenticated actor owns a strictly increasing unsigned `clock`.
- An update carries the actor's entire JSON object, rather than a field-level
  merge. A newer clock replaces all of that actor's visible state.
- A `remove` update has no state. The store retains its actor/clock tombstone
  in memory so a late lower-clock packet cannot revive the actor.
- Equal actor/clock/state is an idempotent duplicate. Equal actor/clock with a
  different state is rejected; accepting either would make UI presence depend
  on packet order.
- A state is live only while a newer heartbeat has arrived within the configured
  TTL (30 seconds by default). A client should publish a new clock before TTL;
  graceful close should publish `Remove`.

For an unchanged local state, call `Store.Heartbeat(actor, now)` rather than
calling `Set` with the same JSON again. It reuses the retained canonical object
but still creates a strictly newer update for the transport. It rejects an
unknown or removed actor: use `Set` to establish a new online state first.

The protocol makes no statement about peer identity, membership, permission,
or confidentiality. The transport must authenticate the connection and bind
the update actor to that authenticated peer before relay. State JSON should be
minimal and must not contain credentials, access tokens, or sensitive profile
data.

## Local application observation

`Store.Subscribe` and `Store.SubscribeAt` provide a bounded, latest-state
mailbox for UI presence lists and cursors. A subscriber immediately receives a
sorted snapshot, then receives a fresh snapshot after a local update, an
accepted remote update, or an explicit expiry. Each subscription has one
pending event, so a slow renderer skips superseded states and receives the
newest complete snapshot with `Event.Coalesced > 0`; network and CRDT mutation
paths never wait for a callback.

Callbacks run outside the Store lock and receive a shared immutable snapshot;
a panic stops only the failing subscription. `MaxSubscribers` defaults to
1,024 and bounds this process-local UI resource. `Store.Expire(now)` makes
timeout transitions observable. Applications that want a lifecycle-bound
helper can call `Store.StartExpiry(ctx, interval)`; its only goroutine stops
when `ctx` is cancelled. Choose an interval no greater than the awareness TTL
when the UI must reflect expiry promptly. Expiry neither sends a removal nor
deletes the actor clock: only a strictly newer heartbeat can make that actor
live again.

These events are deliberately local. `Event.Version`, `Origin`, and
`Coalesced` must not be transmitted, persisted, used for authorization, or
treated as a CRDT causal frontier.

## awareness-v1 binary update

All unsigned integers use the repository's shortest-form uvarint encoding.
The enclosing WebSocket message supplies the message boundary.

| Field | Encoding | Rule |
| --- | --- | --- |
| version | 1 byte | `0x01` |
| actor | uvarint byte length + UTF-8 bytes | non-blank, bounded |
| clock | uvarint | non-zero, monotonically increasing per actor |
| status | 1 byte | `0x00` removal; `0x01` online state |
| state | uvarint byte length + bytes | only for `0x01`; a bounded JSON object |

Online JSON is decoded and re-marshaled before retention, which makes object
key order deterministic. A top-level array, scalar, `null`, malformed JSON,
trailing bytes, non-canonical varints, unknown status, and every overflow are
rejected. The defaults cap actor bytes at 128, one state at 16 KiB, and one
store at 16,384 actors; applications should lower them for their group.

## WebSocket provider v3

`crdt-sync-v3` adds exactly one binary-message discriminator without changing
the delta or batch formats:

```text
0x03 | awareness-v1 update
```

The existing `0x01` change and `0x02` change-batch envelopes retain their v1/v2
meaning under v3. The relay sends `0x03` only to connections that negotiated
v3, so connected v1/v2 peers continue receiving CRDT changes without being
disconnected by an unknown envelope. A server enables awareness by putting an
`*awareness.Store` on `provider.GroupConfig` and supplying
`Config.AuthorizeAwareness`. Both the server and client reject awareness
operations unless v3 was actually negotiated. New v3 peers receive the group's
non-expired in-memory states after the normal manifest handshake. There is no
durable replay.

```go
store, _ := awareness.NewStore(awareness.DefaultOptions())
group, _ := provider.NewGroup(provider.GroupConfig{
	Manifest: manifest,
	Apply: applyDelta,
	MaxPendingChanges: 256,
	MaxPendingBytes: 1 << 20,
	Awareness: store,
})
handler, _ := provider.NewHandler(provider.Config{
	Groups: []*provider.Group{group},
	Authenticate: authenticate,
	Authorize: authorizeDelta,
	AuthorizeAwareness: func(peer provider.Peer, _ replica.Manifest, update awareness.Update) error {
		if peer.ID != update.Actor {
			return provider.ErrUnauthorized
		}
		return nil
	},
})
```

For a text cursor, put an application-defined encoding of `text.Anchor` in the
JSON object. Validate it and call `text.ResolveAnchor` only after the presence
actor has been authorized. An unknown or compacted anchor must clear the cursor
instead of guessing a visible offset.

## Non-goals

- Yjs awareness wire compatibility. im10furry uses authenticated Go provider
  envelopes and stable `text.Anchor` positions; translating between protocols
  belongs in an application gateway with a separately reviewed identity model.
- Durable last-seen history, audit logs, or membership. Those require their
  own retention, privacy, authorization, and clock policy.
- Automatic reconnect, outbox, or reliable delivery. Presence is deliberately
  best-effort; a future heartbeat supersedes missing state.
