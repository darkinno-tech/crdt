# crdt

[中文](README.zh-CN.md)

> A bounded Go CRDT library for convergent state, delta replication, recovery, and explicit protocol boundaries.

`crdt` provides deterministic CRDT primitives and framed binary codecs for replicas that must converge despite duplicate, reordered, or delayed delivery. It is a library, not a complete collaboration service: the host owns identity, authorization, storage, transport, membership, retention, and product invariants.

## Start in three minutes

Requires Go 1.21 or later.

```sh
go get github.com/DarkInno/crdt@latest
```

```go
package main

import (
	"fmt"
	"log"

	"github.com/DarkInno/crdt/counter"
)

func main() {
	left, err := counter.NewGCounter("left")
	if err != nil {
		log.Fatal(err)
	}
	right, err := counter.NewGCounter("right")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := left.Increment(3); err != nil {
		log.Fatal(err)
	}
	if err := right.Merge(left); err != nil {
		log.Fatal(err)
	}
	value, err := right.Value()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(value) // 3
}
```

For a checkout:

```sh
git clone https://github.com/DarkInno/crdt.git
cd crdt
go test ./...
```

## What is included

- G-Counter, PN-Counter, G-Set, add-wins OR-Set, and causal MV-Register.
- Bounded canonical state/delta frames, deterministic snapshots, recovery plans, and persisted HLC state for reusable replica identities.
- A local, bounded multi-type undo/redo command stack plus a content-addressed snapshot version DAG for browser history and branches; both remain outside replication frames and are host-persisted metadata.
- RGA collaborative text with stable run-v2 frames by default; stable bounded rich-text and observed-remove tree protocols; plus list and XML-fragment layers.
- Delta batching, Merkle anti-entropy, exact-acknowledgement tombstone-GC coordination, and manifest-bound replica/inbox recovery helpers.
- A bounded live WebSocket provider, a separate bbolt-backed durable relay, Redis/PostgreSQL durable-log implementations, a bounded WebRTC DataChannel bridge, and local bbolt/file checkpoint Store references.
- Optional, manifest-negotiated [compression-aware outer frame v2](docs/protocol/frame-v2.md) with explicit v1 conversion; it does not change CRDT TypeIDs or semantics.
- [RGA diagnostic obfuscation](docs/integration/debug-obfuscation.md) that replaces text content while retaining an isolated debug timeline structure.

All implemented frame pairs are stable and use the zero-value `ProtocolPolicy`. LWW-Set/Map, scalar RGA v1, list RGA, run-v2 RGA, rich-text v1, and observed-remove tree v1 still require an authenticated exact manifest: a frame type alone is never a negotiated protocol, authenticated peer, or permission to compact tombstones.

## Choose a path

| Goal | Read or run |
| --- | --- |
| Learn the basic APIs | [Getting started](docs/getting-started.md) and [runnable examples](examples) |
| Build a complete client flow | [End-to-end integration](docs/integration/overview.md) |
| Survive local restarts safely | [Local checkpoint Store references](docs/integration/local-checkpoint.md) and `go run ./examples/persistent-replica` |
| Add replay and reconnect | [Durable relay reference](docs/integration/durable-provider.md) |
| Choose browser, WebRTC, Redis, or PostgreSQL boundaries | [Provider architecture](docs/integration/provider-architecture.md) |
| Use a bounded live relay | [WebSocket provider reference](docs/integration/websocket-provider.md) |
| Connect stable Yjs/y-websocket clients | [Yjs / y-protocols compatibility relay](docs/integration/yjs-relay.md) |
| Attach media without CRDT byte replication | [Attachment integration](docs/integration/attachment.md) |
| Implement run-v2 outside Go/Wasm | [RGA run-v2 protocol and vectors](docs/protocol/rga-run-v2.md) |
| Use the native Rust, Python, Swift, or C++ RGA runtime | [Multilanguage RGA decision](docs/design/native-multilanguage-rga.md) |
| Implement stable formatting or trees | [Rich-text v1](docs/protocol/richtext-v1.md) and [observed-remove tree v1](docs/protocol/or-tree-v1.md) |

The [documentation index](docs/README.md) separates getting-started, integration, protocol/design, and operational material. Detailed performance evidence and deployment runbooks live there instead of making this entry page a manual.

## Persistence and recovery

State bytes alone are not a recoverable replica for HLC-backed CRDTs. Persist the state frame, HLC state, and application delivery frontier/outbox atomically before reusing a replica ID. The `persistence.Store` contract has bbolt and dependency-free file references for one typed CRDT schema and one active process; both validate concrete state before saving and on every load.

```sh
go run ./examples/persistent-replica
# recovered=true cursor=41 outbox_bytes=24
```

It is not a clustered database, authenticated transport, or generic business transaction manager. The host still owns encryption at rest, backup/restore, remote authorization, tenant isolation, membership, and tombstone lifecycle.

The `durable` package intentionally persists a relay operation log and replay cursor. Clients must persist their concrete CRDT checkpoint before advancing that cursor; read the [local checkpoint](docs/integration/local-checkpoint.md) and [durable relay](docs/integration/durable-provider.md) references together.

## Package map

| Package | Purpose |
| --- | --- |
| `counter`, `set`, `register` | Counter, set, and register CRDTs. |
| `lww`, `tree`, `text`, `list`, `xml`, `richtext` | HLC-backed and ordered collaborative structures. |
| `encoding`, `delta`, `snapshot`, `clock` | Framing, bounded batches, snapshots, and HLC state. |
| `replica`, `membership`, `tombstonegc`, `merkle` | Delivery continuity, membership, safe GC coordination, and anti-entropy. |
| `persistence` | Local bounded bbolt and file CRDT checkpoint Store references. |
| `history` | Local multi-scope undo/redo command stack and content-addressed snapshot version DAG. |
| `config`, `telemetry` | Explicit layered host configuration and bounded payload-free operational telemetry. |
| `durable`, `extensions`, `awareness`, `observe` | Durable relay, bounded live relay, ephemeral presence, and process-local observation. |
| `attachment` | Immutable media-reference metadata; never raw media bytes. |

## Verify and measure

Run focused checks while changing one package:

```sh
go test ./persistence ./examples/persistent-replica
go test -race ./persistence
go test -run='^$' -fuzz=FuzzUnmarshalCheckpoint -fuzztime=20s -parallel=1 ./persistence
go test -run='^$' -fuzz=FuzzUnmarshalFileRecords -fuzztime=20s -parallel=1 ./persistence
go test -run='^$' -bench='Benchmark((File)?Store(Save|Load|SaveParallel|Delete|LoadLegacyMigration)|(File)?ConfigFromLoader)$' -benchmem -benchtime=2s ./persistence
```

Repository gates:

```sh
go test ./...
go test -race ./...
go vet ./...
make coverage
make verify
```

`make verify` also runs bounded fuzzing, static analysis, linting, integration, and extreme scenarios. `make benchmark` is a controlled development measure, not a production capacity promise—repeat focused benchmarks on the target disk, CPU, Go version, network, and workload before selecting limits.

For host wiring of layered configuration, structured error codes, and bounded durable-relay telemetry, see [production readiness](docs/operations/production-readiness.md).

## Boundaries that matter

- CRC-32C, SHA-256, and a frame type detect format damage; they do not authenticate a peer. Bind exact manifests and protocol policies during an authenticated handshake.
- A greatest observed tag is not proof of contiguous delivery or permission to retire tombstones. Use the relevant frontier, inbox, and membership contracts.
- Both checkpoint backends require one active process and are not HA storage. bbolt has an exclusive file lock; the file reference has no inter-process lock and must never be shared by active pods.
- The library does not enforce business invariants. Validate identity, tenant, value permissions, rate limits, retention, and backup access in the host.

## Contributing and releases

Contributions should include focused tests, preserve canonical encoding, bound untrusted input before allocation or mutation, and update the closest relevant documentation. Review [CONTRIBUTING.md](CONTRIBUTING.md); keep beta changes on the reviewed beta-to-main release path and do not manually move published tags.

## License

SPDX-License-Identifier: MIT. See [LICENSE](LICENSE).
