# Browser, WebRTC, Redis, and PostgreSQL provider architecture

This library keeps CRDT merge semantics separate from delivery and storage.
The browser RGA runtime is Go compiled to Wasm; it does not replace manifest
negotiation, authorization, an outbox, or a durable receipt. The durable relay
now accepts a `durable.Log`, so bbolt remains the local reference while Redis
and PostgreSQL can provide the same replay contract.

## Decision matrix

| Need | Use | Correctness boundary | Do not claim |
| --- | --- | --- | --- |
| Browser RGA with Go interop | `make wasm`, `@darkinno/crdt-client/wasm` | One explicit run-v2 or v1 manifest; state + frontier + HLC clock persist together | A Wasm frame or CRC authenticates its peer |
| Nearby peer-to-peer live delivery | `providers/webrtc` around an already authenticated, ordered/reliable DataChannel | Canonical bounded envelopes are validated against one manifest before `OnChange` | DataChannel `Send` is a durable receipt, or WebRTC supplies replay/history |
| Low-latency shared operation log | `providers/redis` | One Lua script atomically binds Dot, canonical bytes, sequence, and counters | Default Redis persistence/replication is automatically durable enough for product data |
| SQL-backed operation log and multi-process relay | `providers/postgres` | One group row lock serializes append; replay uses a repeatable-read snapshot | A relay event cursor replaces the application's state/frontier/outbox checkpoint |

WebRTC data channels can be ordered and reliable, but those properties are
chosen when the channel is created. Use them for CRDT operation traffic; keep
lossy/unordered presence separate. Signal, authenticate, authorize the exact
document/manifest, and provision STUN/TURN outside this provider.

## Durable Log contract

Every `durable.Log` implementation must make a new append atomic:

```text
Dot -> canonical envelope digest -> group-local sequence -> capacity metadata
```

An identical retry returns its original sequence; a different payload for the
same Dot returns `durable.ErrConflictingDot`. `Replay` must return a contiguous
complete suffix or `durable.ErrReplayUnavailable`, never a convenient prefix.
The handler does not close the log because a PostgreSQL/Redis client pool may
be shared by multiple groups and handlers.

### Redis

Construct the client with the deployment's TLS, ACL, and timeout policy, then
give the provider a narrowly scoped prefix:

```go
client := redis.NewClient(&redis.Options{Addr: redisAddress, TLSConfig: tlsConfig})
store, err := redisprovider.New(client, redisprovider.Config{
	Prefix: "myapp:crdt",
	MaxEvents: 100_000,
	MaxBytes:  256 << 20,
})
```

The provider hashes each group into a Redis Cluster hash tag and executes one
bounded `EVAL`; all keys for a group therefore share a slot. Redis Lua uses
floating-point numbers, so this provider explicitly stops at `2^53-1` instead
of silently losing a sequence. Rotate/bootstrap a group before this limit.
Select and verify AOF/fsync, replica acknowledgement, encryption, backup, and
ACL policies separately; a successful `EVAL` alone is not a power-loss claim.

### PostgreSQL

Run `EnsureSchema` as an explicit migration with a migration role, then use a
restricted runtime role and a pool configured with TLS and sane connection
limits:

```go
pool, err := pgxpool.New(ctx, dsn)
store, err := postgresprovider.New(pool, postgresprovider.Config{
	MaxEvents: 1_000_000,
	MaxBytes:  4 << 30,
})
if err == nil {
	err = store.EnsureSchema(ctx)
}
```

Append obtains `SELECT ... FOR UPDATE` on the group row before allocating its
sequence. Replay uses a read-only repeatable-read transaction, validates every
stored envelope under the requested manifest/policy, and rejects a gap. The
schema uses `BIGINT`; both dot counters and group sequences are intentionally
limited to `MaxInt64` and must rotate/bootstrap before that range ends.

## WebRTC bridge

`providers/webrtc` accepts a tiny adapter interface so browser and Go WebRTC
stacks can remain optional dependencies. Its DataChannel must be created as
ordered and reliable. It bounds queued callback work and closes the channel on
malformed input, callback failure, or queue overflow.

```go
bridge, err := webrtcprovider.New(webrtcprovider.Config{
	Channel: orderedReliableChannel, // application adapter around WebRTC
	Manifest: manifest,
	MaxMessageBytes: 1 << 20,
	MaxActorBytes: 128,
	MaxQueuedMessages: 64,
	MaxQueuedBytes: 4 << 20,
	OnChange: applyToInbox,
})
```

Persist the local mutation and its outbox before `Publish`. Keep it until the
product's server-side receipt transaction succeeds. On reconnect, bootstrap or
repair through a durable relay; do not infer history from a WebRTC channel.

## Verification and performance boundaries

```sh
# Deterministic simulation: canonical bytes, conflicts, bounds, replay,
# queue overload, and SQL transaction ordering.
go test -race ./durable ./providers/redis ./providers/postgres ./providers/webrtc

# Controlled local Redis loopback baseline (not AOF/TLS/cluster performance).
go test -run='^$' -bench='BenchmarkStore(Append|Replay)Loopback' -benchmem ./providers/redis

# Explicit external-service acceptance. These do not run accidentally in CI.
CRDT_REDIS_ADDR=127.0.0.1:6379 go test -run TestRedisStoreAcceptance ./providers/redis
CRDT_POSTGRES_DSN='postgres://crdt:crdt@127.0.0.1:5432/crdt?sslmode=disable' go test -run TestPostgresStoreAcceptance ./providers/postgres

# Browser/Wasm contract and duplicate/reorder/snapshot simulation.
make wasm-test
```

The controlled provider tests prove the library contracts named above. They do
not prove browser/device compatibility, NAT traversal, a live identity system,
remote fsync semantics, a production checkpoint transaction, or capacity on a
target workload. Measure p50/p95/p99 append/replay latency, Redis persistence
lag, PostgreSQL lock wait/connection-pool saturation, DataChannel buffered
amount, browser main-thread time, and storage-quota failures before selecting
product limits.
