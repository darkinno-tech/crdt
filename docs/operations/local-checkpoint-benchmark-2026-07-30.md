# Local checkpoint benchmark — 2026-07-30

This is controlled development evidence for the `persistence` reference at the
current working revision. It measures a local bbolt file, not a clustered
database, a network, TLS, external identity, or production capacity.

## Method

- Host: Apple M4 Pro, `darwin/arm64`, Go 1.26.5.
- Fixture: an OR-Set checkpoint with three short items, HLC state/frontier,
  cursor `41`, and a 24-byte opaque outbox.
- `Save` replaces one named record in a real temporary local bbolt file;
  `Load` reads it and runs the concrete OR-Set validator.
- `SaveParallel` uses `RunParallel` against that same record. It deliberately
  shows the one-writer serialization cost rather than aggregate scalability.
- Three samples per setting, `-benchtime=2s`, `-benchmem`.

```sh
GOMAXPROCS=1 go test -run='^$' -bench='BenchmarkStore(Save|Load|SaveParallel)$' -benchmem -benchtime=2s -count=3 ./persistence
GOMAXPROCS=4 go test -run='^$' -bench='BenchmarkStore(Save|Load|SaveParallel)$' -benchmem -benchtime=2s -count=3 ./persistence
```

## Results

| Benchmark | GOMAXPROCS=1 mean | GOMAXPROCS=4 mean | Allocation |
| --- | ---: | ---: | --- |
| `Save` | 6.77 ms/op | 6.65 ms/op | 23.3 KB/op; 81 allocs/op |
| `Load` | 2.27 µs/op | 2.00 µs/op | 4,040 B/op; 44 allocs/op |
| `SaveParallel` | 6.51 ms/op | 6.68 ms/op | about 23.3 KB/op; 81 allocs/op |

The close `Save` and `SaveParallel` results are expected: bbolt permits one
read-write transaction at a time. `GOMAXPROCS=4` improves the serial `Load`
sample modestly; it does not turn the local store into a multi-writer system.
Repeat this measurement on the production disk and with representative state,
frontier, outbox, backup, and failure-injection workloads before choosing
quotas or latency objectives.

## Recovery simulation

`TestThreeReplicaCheckpointRecoverySimulation` uses a real temporary bbolt
file while one mobile OR-Set replica is partitioned. It persists state, HLC,
cursor, and exact outbox bytes; the process is reopened, a new local mutation
is made with the restored replica ID, a remote delta arrives, and all three
replicas converge. This validates CRDT recovery behavior, not remote relay,
identity, multi-volume, or backup acceptance.
