# Browser, WebRTC, Redis, PostgreSQL, MySQL, and SQLite provider architecture

This library keeps CRDT merge semantics separate from delivery and storage.
The browser RGA runtime is Go compiled to Wasm; it does not replace manifest
negotiation, authorization, an outbox, or a durable receipt. The durable relay
now accepts a `durable.Log`, so bbolt remains the local reference while Redis,
PostgreSQL, MySQL, and SQLite can provide the same replay contract.

## Decision matrix

| Need | Use | Correctness boundary | Do not claim |
| --- | --- | --- | --- |
| Browser RGA with Go interop | `make wasm`, `@darkinno/crdt-client/wasm` | One explicit run-v2 or v1 manifest; state + frontier + HLC clock persist together | A Wasm frame or CRC authenticates its peer |
| Nearby peer-to-peer live delivery | `providers/webrtc` around an already authenticated, ordered/reliable DataChannel | Canonical bounded envelopes are validated against one manifest before `OnChange` | DataChannel `Send` is a durable receipt, or WebRTC supplies replay/history |
| Low-latency shared operation log | `providers/redis` | One Lua script atomically binds Dot, canonical bytes, sequence, and counters | Default Redis persistence/replication is automatically durable enough for product data |
| PostgreSQL-backed operation log and multi-process relay | `providers/postgres` | One group row lock serializes append; replay uses a repeatable-read snapshot | A relay event cursor replaces the application's state/frontier/outbox checkpoint |
| MySQL-backed operation log and multi-process relay | `providers/mysql` | One InnoDB group row lock serializes append; replay uses a read-only repeatable-read snapshot | A relay event cursor replaces the application's state/frontier/outbox checkpoint |
| Embedded durable operation log | `providers/sqlite` | The first write serializes SQLite writers; replay validates one transaction snapshot | SQLite is HA storage, or a relay cursor replaces the application's state/frontier/outbox checkpoint |

## Dependency boundary

`github.com/DarkInno/crdt` is the dependency-free published core. Durable
storage, transports, and every provider below are independent opt-in modules:

```sh
go get github.com/DarkInno/crdt/durable@latest
go get github.com/DarkInno/crdt/providers/redis@latest
go get github.com/DarkInno/crdt/providers/postgres@latest
go get github.com/DarkInno/crdt/providers/mysql@latest
go get github.com/DarkInno/crdt/providers/sqlite@latest
go get github.com/DarkInno/crdt/providers/webrtc@latest
```

The MySQL and SQLite providers use Go's standard `database/sql` API and never
register a driver. Selecting a MySQL/SQLite driver is an application decision,
so those provider modules add no driver dependency; the application imports
and owns the selected driver, pool, credentials, and TLS policy.

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
The handler does not close the log because a PostgreSQL/MySQL/SQLite/Redis client pool may
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

### MySQL

Use MySQL 8.0+ with InnoDB. The provider uses only `database/sql`; the
application imports its driver, chooses its pool, keeps TLS and credentials out
of source, and runs the explicit schema migration with a migration role:

```go
import (
	"database/sql"

	mysqlprovider "github.com/DarkInno/crdt/providers/mysql"
	// This is an application dependency, not a crdt module dependency.
	_ "github.com/go-sql-driver/mysql"
)

database, err := sql.Open("mysql", dsn)
if err != nil {
	return err
}
store, err := mysqlprovider.New(database, mysqlprovider.Config{
	MaxEvents: 1_000_000,
	MaxBytes:  4 << 30,
})
if err == nil {
	err = store.EnsureSchema(ctx)
}
```

The fixed schema uses binary group and actor keys, a `LONGBLOB` envelope, and
separate statements so the DSN need not enable `multiStatements`. Group IDs
are bounded to 1,024 bytes and actor IDs to 255 bytes to fit the documented
InnoDB primary-key budget. Append locks one group row at `READ COMMITTED`;
replay validates the complete suffix under a read-only `REPEATABLE READ`
snapshot. As with PostgreSQL, `BIGINT` caps dot counters and durable sequences
at `MaxInt64`; rotate/bootstrap before that boundary.

Select `max_allowed_packet`, transaction durability
(`innodb_flush_log_at_trx_commit`), backup/restore, TLS, least-privilege runtime
grants, and failover policy for the actual deployment. A committed SQL
transaction is not, by itself, a claim about a remote replica or a client's
checkpoint transaction.

### SQLite

`providers/sqlite` is a local embedded operation log, not an HA backend. Like
the MySQL provider, its database integration uses only `database/sql`; the host
selects and imports an SQLite driver, then owns file permissions, encryption,
backup/restore, and the driver-specific busy timeout:

```go
import (
	"database/sql"

	sqliteprovider "github.com/DarkInno/crdt/providers/sqlite"
	// Add a blank import for the application-selected SQLite driver.
)

database, err := sql.Open(sqliteDriverName, sqliteDataSourceName)
if err != nil {
	return err
}
store, err := sqliteprovider.New(database, sqliteprovider.Config{
	MaxEvents: 100_000,
	MaxBytes:  256 << 20,
})
if err == nil {
	err = store.EnsureSchema(ctx)
}
```

The first `INSERT OR IGNORE` in Append acquires SQLite's writer reservation, so
writes are serialized across the database rather than independently per group.
Keep append transactions short, configure a bounded busy timeout in the chosen
driver, and retry only the host operation after a lock/busy error. The provider
uses signed SQLite `INTEGER` values and therefore stops dot counters and
durable sequences at `MaxInt64`. A local SQLite commit is not a peer receipt,
multi-process HA design, or a substitute for the client's checkpoint
transaction.

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
make race

# Controlled local Redis loopback baseline (not AOF/TLS/cluster performance).
(cd providers/redis && go test -run='^$' -bench='BenchmarkStore(Append|Replay)Loopback' -benchmem .)

# Explicit external-service acceptance. These do not run accidentally in CI.
(cd providers/redis && CRDT_REDIS_ADDR=127.0.0.1:6379 go test -run TestRedisStoreAcceptance .)
(cd providers/postgres && CRDT_POSTGRES_DSN='postgres://crdt:crdt@127.0.0.1:5432/crdt?sslmode=disable' go test -run TestPostgresStoreAcceptance .)

# MySQL and SQLite drivers are host-owned. Run equivalent append/retry/conflict/
# replay checks from the application after importing its selected driver.

# Browser/Wasm contract and duplicate/reorder/snapshot simulation.
make wasm-test
```

The controlled provider tests prove the library contracts named above. They do
not prove browser/device compatibility, NAT traversal, a live identity system,
remote fsync semantics, a production checkpoint transaction, or capacity on a
target workload. Measure p50/p95/p99 append/replay latency, Redis persistence
lag, PostgreSQL/MySQL lock wait and connection-pool saturation, DataChannel buffered
amount, browser main-thread time, and storage-quota failures before selecting
product limits.
