# Local bbolt checkpoint reference

`persistence` is a local CRDT recovery reference. One bbolt transaction saves
one complete `snapshot.Snapshot`, its durable-relay cursor, and an
application-owned opaque outbox. It fills the local-checkpoint gap between
CRDT state objects and the [`durable`](durable-provider.md) relay; it is not a
multi-node database.

Use it when one process owns one protected local volume and one concrete CRDT
state codec. The executable OR-Set restart flow is:

```sh
go run ./examples/persistent-replica
# recovered=true cursor=41 outbox_bytes=24
```

## Recovery boundary

```text
local mutation -> SnapshotCurrentState -> Save(state, frontier, HLC, cursor, outbox)
                                                    |
                                                    +--> one bbolt commit

restart -> Load + concrete validation -> NewFromSnapshot -> retry exact outbox bytes
```

`Save` returning `nil` means that its local state, frontier, HLC, cursor, and
outbox committed together. It does not acknowledge a peer, a remote relay, or
another application database. Persist a newly created local delta and its
outbox representation before publishing it according to your retry policy.

For a durable relay receive callback, put in `Cursor` only the final sequence
whose effects are already in `Snapshot`. A lower cursor causes safe duplicate
replay; a cursor ahead of the state can lose a change. The store does not guess
or silently advance this application invariant.

## Configure a typed store

The validator is deliberate: a frame checksum proves neither a concrete
schema/codec nor safe decoding. One `Store` has one concrete validation
function, so use a separate store (or an explicit migration) for a different
CRDT state type or element codec.

```go
store, err := persistence.Open("/var/lib/myapp/tasks.db", persistence.Config{
	MaxRecordBytes:     1 << 20,
	MaxStateBytes:      512 << 10,
	MaxFrontierEntries: 4 << 10,
	MaxReplicaIDBytes:  256,
	MaxOutboxBytes:     64 << 10,
	MaxNameBytes:       128,
	Validate: func(data []byte) error {
		candidate, err := set.NewORSet("validation", taskCodec{})
		if err != nil {
			return err
		}
		limits := frame.DefaultLimits()
		limits.MaxFrameBytes = 512 << 10
		limits.MaxPayload = 512 << 10
		return candidate.UnmarshalBinaryWithLimits(data, limits)
	},
})
if err != nil {
	return err
}
defer store.Close()
```

## Record-format compatibility and migration

`Config.Format` is the single policy for the local checkpoint envelope. New
stores write `RecordFormatV2` by default and, by default, can read the
immediately preceding v1 envelope. This compatibility applies only to the
outer bbolt value; it does not make CRDT frame versions, TypeIDs, codecs, or
Manifests interchangeable.

For an envelope-only upgrade, enable transactional rewrite after a verified
backup. The v1 and v2 checkpoint payloads have the same semantics, so no
custom transform is needed:

```go
config.Format = persistence.FormatConfig{
	Version:       persistence.RecordFormatV2,
	Compatibility: persistence.CompatibilityCurrentAndPrevious,
	MigrateOnLoad: true,
}
```

`Load` first checks the record digest, all configured byte/count limits, the
source validator, and the CRDT frame before calling a migration. It then
validates the replacement with `Config.Validate` and rewrites it in the same
bbolt write transaction. A failed transform returns `persistence.ErrMigration`
and leaves the source record unchanged. Set `CompatibilityCurrentOnly` once
the rollback window closes to reject legacy files.

When a CRDT schema or codec also changes, supply one `Migration` for the old
record version. Its optional `Validate` validates the source format and its
`Transform` must return a complete target checkpoint accepted by
`Config.Validate`. Keep the transform bounded and deterministic; do not use
it to infer identity, authorize input, or rewrite live remote CRDT traffic.

The parent directory must already exist and be host-protected; the database is
opened as `0600`. bbolt holds an exclusive process lock, so run exactly one
active process for a path. Keep databases per local replica/schema instead of
mounting the same path into multiple pods.

## Save and restore

```go
saved, err := tasks.SnapshotCurrentState() // OR-Set state, frontier, and HLC
if err != nil {
	return err
}
if err := store.Save("tasks", persistence.Checkpoint{
	Snapshot: saved,
	Cursor:   durableSequence,
	Outbox:   canonicalPendingPayloads,
}); err != nil {
	return err
}

checkpoint, found, err := store.Load("tasks")
if err != nil || !found {
	return err
}
tasks, err = set.NewORSetFromSnapshot(checkpoint.Snapshot, taskCodec{})
if err != nil {
	return err
}
```

HLC-based protocols (OR-Set, LWW, RGA, OR-Tree, list RGA, and rich text)
cannot omit HLC state. `persistence.Save` rejects that shape before writing,
and `Load` rejects it as corruption. This prevents a restarted process from
reusing an older mutation tag; the recovery factory remains the final
type/codec guard.

`Outbox` is intentionally a bounded opaque byte slice. It lets the application
keep exact canonical pending payloads in the same bbolt transaction without
this package inventing transport identity, manifest, or authorization
semantics. Retry original bytes after an ambiguous send; do not regenerate a
new mutation tag.

## Security and operations

| Boundary | Reference behavior | Host responsibility |
| --- | --- | --- |
| Record format | Versioned deterministic record with SHA-256 damage detection; malformed, non-canonical, oversized, and unknown-version data fail closed. | Protect databases and backups from modification; the digest does not authenticate an attacker. |
| CRDT state | Concrete validation runs before commit and after load; HLC state is mandatory for HLC protocols. | Supply schema-specific decoder limits and call the matching `NewFromSnapshot`. |
| Resource use | Required bounds cover record, state, frontier, replica IDs, outbox, and name before allocation. | Choose quotas for real documents, actor counts, and retry load. |
| Atomicity | One `Update` commits state, frontier, HLC, cursor, and outbox. | Coordinate business rows in the same database/transaction or use an application outbox protocol. |
| Availability | One process and one local volume. | TLS, identity, encryption at rest, backups, multi-node failover, membership, and tombstone-GC policy. |

Frame checksums and the record digest detect accidental damage, not hostile
peers. Authenticate a manifest and validate concrete remote input before it
reaches local state. A checkpoint is also not a tombstone-compaction permit:
retain the current epoch/exact acknowledgements, a durable post-compaction
snapshot, and old-delta retirement policy.

bbolt has serializable ACID transactions but one writer. Keep `Save`
transactions small; do not wait on a network call inside a transaction, and do
not expect parallel saves to increase write throughput. Back up and
restore-test a closed database file or use a host-managed consistent volume
snapshot.

```sh
go test ./persistence ./examples/persistent-replica
go test -race ./persistence
go test -run='^$' -fuzz=FuzzUnmarshalCheckpoint -fuzztime=20s -parallel=1 ./persistence
go test -run='^$' -bench='BenchmarkStore(Save|Load|SaveParallel|LoadLegacyMigration)$' -benchmem -benchtime=2s ./persistence
```

These checks cover local restart, corruption rejection, concurrent access, and
fuzzed decoding. They do not prove host backups, full-disk behavior, TLS,
external identity, a multi-node store, or production capacity.
