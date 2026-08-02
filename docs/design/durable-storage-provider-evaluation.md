# Durable storage provider evaluation

## Decision

Add `providers/mssql` as an opt-in Microsoft SQL Server/Azure SQL durable-log
implementation. It reuses the existing `database/sql` relay core, but retains
provider-owned DDL, identifier validation, locking statements, and transaction
isolation. This is a durable relay log only: it is not a client checkpoint,
identity provider, authorization system, or high-availability topology.

## Non-negotiable contract

Each new append must atomically bind:

```text
Dot -> canonical envelope SHA-256 -> group-local sequence -> retained-count/byte budget
```

An identical retry returns its original sequence; a changed payload for the
same Dot returns `durable.ErrConflictingDot`. Replay returns a complete
contiguous suffix that is decoded again against the caller's manifest/policy,
or fails closed. A storage candidate that cannot preserve all four writes and
the replay snapshot in one bounded transaction is not an implementation of
`durable.Log`.

## Candidate assessment

| Candidate | Correctness and transaction fit | Performance/operations | Security/dependency boundary | Decision |
| --- | --- | --- | --- | --- |
| Microsoft SQL Server / Azure SQL | Row/range locks and serializable transactions can atomically create a group, bind a Dot, allocate a sequence, and update capacity; a serializable read observes one suffix. | Fits existing enterprise pools; one group is intentionally serialized. Long replays hold locks, so replay remains bounded. | Standard `database/sql`; driver, TLS, identity, migration credentials, and runtime grants remain application-owned. | Implement now. |
| MongoDB | A separate document/transaction design is required for a Dot index, sequence allocation, byte budget, and a consistent replay query. | Collection/index layout and transaction cost need a dedicated target benchmark. | Adds a driver and operational assumptions not shared by SQL relay. | Defer until a target workload requires it. |
| Cassandra/Scylla | Conditional writes and paged reads need a new proof that Dot binding, sequence allocation, capacity, and snapshot-like replay cannot diverge. | Partition/key design and multi-region latency dominate; no safe adapter is available by renaming SQL statements. | New driver, consistency-level, repair, and credential policy boundary. | Defer. |
| NATS JetStream / Kafka | A stream append alone does not atomically bind a Dot digest and retained budget with the event. | Excellent delivery systems but retention/consumer cursors are not this log's exact retry contract. | Would introduce a control-plane and authorization model outside the library. | Do not represent as `durable.Log` without a dedicated transactional metadata design. |
| Object/blob storage | Conditional object writes and listings do not supply a bounded, contiguous transactional suffix. | High list latency and compaction coordination are unsuitable for hot relay writes. | Bucket policy and encryption are host concerns but do not repair the atomicity gap. | Reject. |

The SQL Server key choice is intentionally conservative. Microsoft documents a
900-byte clustered-index key limit; the provider uses a 350-unit `NVARCHAR`
group ID, a 90-unit actor ID, and an 8-byte `BIGINT` counter, for at most 888
bytes in the composite primary key. It validates UTF-16 units rather than
assuming UTF-8 byte length predicts the database index size. [SQL Server index
limits](https://learn.microsoft.com/en-us/sql/t-sql/statements/create-index-transact-sql)

## SQL Server design

- Append uses a `SERIALIZABLE` transaction. The group creation query and the
  group/Dot reads use fixed `UPDLOCK, HOLDLOCK` statements; serializable
  transactions take key-range protection, avoiding a first-group insert race.
  [SQL Server isolation and key-range locks](https://learn.microsoft.com/en-us/sql/t-sql/statements/set-transaction-isolation-level-transact-sql)
- Replay is a short serializable transaction with a fixed read-only statement
  set. This favors a complete result over a lower-latency but statement-by-
  statement view; an over-budget or unavailable replay must bootstrap rather
  than accept a prefix. The Go SQL Server driver rejects `database/sql`'s
  `ReadOnly` transaction option, so it is intentionally not requested.
- Identifiers must be valid UTF-8, have no leading/trailing Unicode whitespace
  or NUL, and fit the provider's UTF-16 limits. Binary collation keeps case and
  Unicode code-point identity explicit within SQL Server's comparison rules.
- All DDL table names are fixed and all group, actor, sequence, digest, and
  envelope values are parameters. The provider contains no user-controlled SQL
  construction.
- `EnsureSchema` is an explicit migration operation. Runtime roles need only
  table rights; schema migration, TLS/certificate validation, encryption at
  rest, backup/restore, tenant access control, failover, and database resource
  governance belong to the host.

## Validation matrix

| Layer | Evidence | What it proves | What it does not prove |
| --- | --- | --- | --- |
| Deterministic unit simulation | `providers/internal/sqlrelay` scripted pool | SQL statement order, commit/rollback, duplicate/conflict, capacity, gap rejection, provider-specific ID guards | SQL Server engine/driver behavior, disk latency, TLS, or HA durability |
| Provider boundary | `providers/mssql` tests | SQL Server DDL/locking/transaction selection and UTF-16 boundary cases | Live DDL privileges or database collation configuration |
| Race | `go test -race` across all workspace modules | Go memory safety in library/test execution | Database lock contention semantics |
| Live acceptance | Application-owned driver plus a disposable SQL Server/Azure SQL database | Driver parameter binding, migration, concurrent first group, restart/replay, TLS rejection | Target-region production capacity or replica/power-loss durability |
| Performance | Simulated benchmark plus target-service p50/p95/p99 append/replay tests | Local code allocation baseline and deployment-specific latency | A universal throughput or capacity promise |

The current implementation therefore makes only the first two claims from
repository tests. A live environment must run the acceptance matrix before it
is used for product data.
