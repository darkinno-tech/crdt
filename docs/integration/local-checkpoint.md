# Local checkpoint Store references

`persistence` is a local CRDT recovery reference in the opt-in
`github.com/darkinno-tech/crdt/persistence` module. Its `Store` contract saves
one complete `snapshot.Snapshot`, its durable-relay cursor, and an
application-owned opaque outbox as one durability boundary. `BoltStore` uses a
bbolt transaction; `FileStore` uses a private file replacement. It fills the
local-checkpoint gap between CRDT state objects and the
[`durable`](durable-provider.md) relay; it is not a multi-node database.

Use it when one process owns one protected local volume and one concrete CRDT
state codec. The executable OR-Set restart flow is:

```sh
(cd examples && go run ./persistent-replica)
# recovered=true cursor=41 outbox_bytes=24
```

## Recovery boundary

```text
local mutation -> SnapshotCurrentState -> Save(state, frontier, HLC, cursor, outbox)
                                                    |
                                                    +--> one Store durability boundary

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

## Configure a typed Store

The validator is deliberate: a frame checksum proves neither a concrete
schema/codec nor safe decoding. One `Store` has one concrete validation
function, so use a separate store (or an explicit migration) for a different
CRDT state type or element codec.

```go
config := persistence.Config{
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
}
var store persistence.Store
store, err := persistence.Open("/var/lib/myapp/tasks.db", config) // bbolt
if err != nil {
	return err
}
defer store.Close()
```

### Load bounded settings explicitly

Host applications can build the same typed configuration from the immutable
layered `config.Loader`; constructors still do not read environment variables
themselves. Every capacity is deliberately required, while the bbolt lock
timeout and format policy have documented defaults. For an environment source
with prefix `CRDT_`, provide `CRDT_PERSISTENCE_MAX_RECORD_BYTES`,
`CRDT_PERSISTENCE_MAX_STATE_BYTES`, `CRDT_PERSISTENCE_MAX_FRONTIER_ENTRIES`,
`CRDT_PERSISTENCE_MAX_REPLICA_ID_BYTES`, `CRDT_PERSISTENCE_MAX_OUTBOX_BYTES`,
and `CRDT_PERSISTENCE_MAX_NAME_BYTES`.

```go
environment, err := config.NewEnvironment("CRDT_")
if err != nil {
	return err
}
loader, err := config.New(environment)
if err != nil {
	return err
}
config, err := persistence.ConfigFrom(loader, validateTasks)
if err != nil {
	return err
}
store, err := persistence.Open("/var/lib/myapp/tasks.db", config)
```

For `FileStore`, also require `CRDT_PERSISTENCE_MAX_STORE_BYTES` and call
`persistence.FileConfigFrom`. `PERSISTENCE_FORMAT_VERSION`,
`PERSISTENCE_FORMAT_COMPATIBILITY` (`current` or `current-and-previous`), and
`PERSISTENCE_MIGRATE_ON_LOAD` are optional. Validators and executable
migrations remain code-owned arguments to `ConfigFrom`/`FileConfigFrom`; never
encode or source them from environment data.

## Record-format compatibility and migration

`Config.Format` is the single policy for the local checkpoint envelope. New
stores write `RecordFormatV2` by default and, by default, can read the
immediately preceding v1 envelope. This compatibility applies only to the
local checkpoint record; it does not make CRDT frame versions, TypeIDs, codecs,
or Manifests interchangeable.

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
validates the replacement with `Config.Validate` and rewrites it through the
same bbolt transaction or atomic file replacement. A failed transform returns
`persistence.ErrMigration` and leaves the source record unchanged. Set
`CompatibilityCurrentOnly` once the rollback window closes to reject legacy
files.

When a CRDT schema or codec also changes, supply one `Migration` for the old
record version. Its optional `Validate` validates the source format and its
`Transform` must return a complete target checkpoint accepted by
`Config.Validate`. Keep the transform bounded and deterministic; do not use
it to infer identity, authorize input, or rewrite live remote CRDT traffic.


To use the dependency-free file reference instead, set an explicit total-file
budget. It validates each stored record at open and load, writes a `0600` temp
file, syncs it, atomically renames it, then syncs the parent directory:

```go
store, err = persistence.OpenFile("/var/lib/myapp/tasks.store", persistence.FileConfig{
	Config:        config,
	MaxStoreBytes: 4 << 20,
})
```

The parent directory must already exist and be host-protected. Both backends
require one active process per path. bbolt additionally holds an OS file lock;
the file reference uses an in-process mutex and therefore cannot detect a
second process. Keep stores per local replica/schema instead of mounting the
same path into multiple pods.

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

## Retire a local checkpoint

`Delete` is idempotent: it returns `found=false, nil` if the named checkpoint
is already absent. It atomically removes only that local recovery boundary;
use it after the host's retention/rejoin policy confirms the replica will not
need to recover from it. It does **not** acknowledge a peer, retire a durable
relay event, or make CRDT tombstones eligible for collection.

```go
deleted, err := store.Delete("retired-device")
if err != nil {
	return err
}
if deleted {
	// The application may now remove only its matching local metadata.
}
```

HLC-based protocols (OR-Set, LWW, RGA, OR-Tree, list RGA, and rich text)
cannot omit HLC state. `persistence.Save` rejects that shape before writing,
and `Load` rejects it as corruption. This prevents a restarted process from
reusing an older mutation tag; the recovery factory remains the final
type/codec guard.

`Outbox` is intentionally a bounded opaque byte slice. It lets the application
keep exact canonical pending payloads in the same Store durability boundary without
this package inventing transport identity, manifest, or authorization
semantics. Retry original bytes after an ambiguous send; do not regenerate a
new mutation tag.

## Security and operations

| Boundary | Reference behavior | Host responsibility |
| --- | --- | --- |
| Record format | Versioned deterministic record with SHA-256 damage detection; malformed, non-canonical, oversized, and unknown-version data fail closed. | Protect databases and backups from modification; the digest does not authenticate an attacker. |
| CRDT state | Concrete validation runs before commit and after load; HLC state is mandatory for HLC protocols. | Supply schema-specific decoder limits and call the matching `NewFromSnapshot`. |
| Resource use | Required bounds cover record, state, frontier, replica IDs, outbox, and name before allocation. | Choose quotas for real documents, actor counts, and retry load. |
| Atomicity | `BoltStore` commits one bbolt `Update`; `FileStore` writes/syncs/replaces a complete private file. Both commit state, frontier, HLC, cursor, and outbox together. | Coordinate business rows in the same database/transaction or use an application outbox protocol. |
| Availability | One process and one local volume; file storage has no inter-process lock. | TLS, identity, encryption at rest, backups, multi-node failover, membership, and tombstone-GC policy. |

Frame checksums and the record digest detect accidental damage, not hostile
peers. Authenticate a manifest and validate concrete remote input before it
reaches local state. A checkpoint is also not a tombstone-compaction permit:
retain the current epoch/exact acknowledgements, a durable post-compaction
snapshot, and old-delta retirement policy.

bbolt has serializable ACID transactions but one writer. `FileStore` rewrites
the complete bounded file on each save, so prefer bbolt for larger checkpoint
sets or high write rates. Keep saves small; do not wait on a network call in a
durability boundary, and do not expect parallel saves to increase write
throughput. Back up and restore-test a closed database file or use a
host-managed consistent volume snapshot.

```sh
(cd persistence && go test .)
(cd examples && go test ./persistent-replica)
(cd persistence && go test -race .)
(cd persistence && go test -run='^$' -fuzz=FuzzUnmarshalCheckpoint -fuzztime=250000x -parallel=1 .)
(cd persistence && go test -run='^$' -fuzz=FuzzUnmarshalFileRecords -fuzztime=250000x -parallel=1 .)
(cd persistence && go test -run='^$' -bench='Benchmark((File)?Store(Save|Load|SaveParallel|Delete|LoadLegacyMigration)|(File)?ConfigFromLoader)$' -benchmem -benchtime=2s .)
```

These checks cover local restart, corruption rejection, concurrent access, and
fuzzed decoding. They do not prove host backups, full-disk behavior, TLS,
external identity, a multi-node store, or production capacity.
