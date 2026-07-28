# crdt

`crdt` is a small, dependency-free Go library for composable state-based CRDTs.
It provides deterministic binary state and delta frames so replicas can converge
despite duplicate delivery, reordering, and temporary partitions.

> Status: pre-release. The first public module release will be `v0.1.0`; APIs
> may change before `v1.0.0`.

## Features

- State-based **G-Counter** with joinable, type-isolated deltas.
- Add-wins observed-remove **OR-Set** with a caller-defined element codec.
- Hybrid logical clock (HLC) tags and a persistable clock state for replica
  restarts.
- Canonical, checksummed binary frames with bounded decoding and deterministic
  encoding.
- Delta batching/coalescing, versioned snapshots, and Merkle digests for
  anti-entropy workflows.
- Optional exact-acknowledgement tombstone collection with membership epochs.
- Safe concurrent access for the provided CRDT implementations.

## Scope

This library provides CRDT data types and wire primitives. It deliberately does
not choose a network transport, membership protocol, authentication scheme,
storage backend, or retry policy. `tombstonegc.Coordinator` performs safe
automatic collection only after the application supplies an authoritative,
authenticated active-membership view. It does not discover, authenticate, or
persist that view. A checksum
detects accidental frame corruption; it is not an authenticity or encryption
mechanism.

## Requirements

- Go 1.21 or later

## Install

After the first public release tag is available:

```sh
go get github.com/darkinno/crdt@v0.1.0
```

Until then, use a local checkout for development:

```sh
git clone https://github.com/darkinno/crdt.git
cd crdt
go test ./...
```

## Quick start

### G-Counter

Each replica increments only its own component. `Merge` takes the per-replica
maximum, so it is commutative, associative, and idempotent.

```go
package main

import (
	"fmt"
	"log"

	"github.com/darkinno/crdt/counter"
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

	if _, err := left.Increment(2); err != nil {
		log.Fatal(err)
	}
	if _, err := right.Increment(3); err != nil {
		log.Fatal(err)
	}
	if err := left.Merge(right); err != nil { // Delivery order does not matter.
		log.Fatal(err)
	}

	value, err := left.Value()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(value)
	// Output: 5
}
```

### OR-Set delta replication

An OR-Set uses a stable codec ID and stable encoded element bytes to identify a
set's element type across replicas.

```go
package main

import (
	"fmt"
	"log"

	"github.com/darkinno/crdt/set"
)

type stringCodec struct{}

func (stringCodec) ID() string                            { return "example.com/string/v1" }
func (stringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (stringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

func main() {
	codec := stringCodec{}
	left, err := set.NewORSet("left", codec)
	if err != nil {
		log.Fatal(err)
	}
	right, err := set.NewORSet("right", codec)
	if err != nil {
		log.Fatal(err)
	}

	delta, err := left.Add("item")
	if err != nil {
		log.Fatal(err)
	}
	if err := right.ApplyDelta(delta); err != nil {
		log.Fatal(err)
	}

	fmt.Println(right.Contains("item"))
	// Output: true
}
```

For a remove to be observed by other replicas, send the returned remove delta
or merge the state. An add concurrent with a remove that did not observe its
tag remains present (add-wins semantics).

## Correct use in a distributed system

- Give every live logical replica a globally unique, non-blank replica ID.
- Persist an OR-Set snapshot atomically with its HLC state. Use
  `ORSet.SnapshotCurrentState()` when the set's own frontier is sufficient, or
  `ORSet.Snapshot(frontier)` when the replication layer has a broader
  acknowledgement frontier; restore with `NewORSetFromSnapshot`. Do not
  restore a same-ID OR-Set from bytes alone.
- For automatic tombstone collection, create a coordinator with a stable
  replication-group ID. Each active member reports its exact
  `ORSet.TombstoneTags()` under that ID and the current
  `tombstonegc.Coordinator` membership epoch; pass both values to
  `AcknowledgeAndCompact` for every received report. Do not derive
  acknowledgements from `Frontier()` when delta delivery can be out of order:
  a maximum tag does not prove that prior tombstones were received. Removing a
  member requires retiring it from replication; a rejoining member must
  bootstrap from a post-compaction snapshot.
- `ORSet.Compact` remains available only for transports that independently
  prove a gap-free causal prefix for every supplied frontier.
- Keep `ElementCodec.ID`, `Marshal`, and `Unmarshal` deterministic and safe for
  concurrent calls. Encoded values must round-trip canonically.
- Treat received bytes as untrusted. Use `UnmarshalBinaryWithLimits` and
  `Unmarshal*DeltaWithLimits` with limits appropriate to the transport.
- Authenticate, authorize, encrypt, retry, and persist messages in the
  surrounding application. CRDT convergence does not provide those guarantees.

## Packages

| Package | Purpose |
| --- | --- |
| `crdt` | Common contracts, state summaries, and mutation tags. |
| `clock` | Hybrid logical clock and persisted HLC state. |
| `counter` | G-Counter and its delta codec. |
| `set` | Add-wins OR-Set and element-codec contract. |
| `encoding` | Versioned bounded binary frames. |
| `delta` | Bounded delta batches and coalescers. |
| `snapshot` | Immutable state snapshots and recovery plans. |
| `merkle` | Deterministic digests for anti-entropy. |
| `tombstonegc` | Exact tombstone acknowledgement and epoch-scoped GC coordination. |

## Development and verification

```sh
go test ./...
go test -race ./...
go vet ./...
make coverage
```

`make verify` additionally runs fuzzing, `staticcheck`, and `golangci-lint`.
Those two tools must be installed on `PATH` locally; GitHub Actions installs
pinned versions. To reproduce the coverage gate in Docker:

```sh
make docker-test
```

### Diagnostic and synchronization probes

`crdt-analyze` verifies one bounded frame before emitting JSON metadata (type,
codec, payload size, and SHA-256 fingerprint):

```sh
go run ./cmd/crdt-analyze -file ./state.frame
```

`crdt-sync-probe` is a short-lived HTTP test utility for exercising duplicate
delta delivery across hosts. It is not a production replication service. Its
default listener is loopback-only; a non-empty token is required for every
endpoint. Prefer `-token-file` (mode `0600`) over `-token`, and bind a public
address only for a controlled test window.

```sh
# On each receiver.
go run ./cmd/crdt-sync-probe -mode serve -replica receiver -token-file ./probe.token

# Generate one delta and send that same byte sequence to every target.
go run ./cmd/crdt-sync-probe -mode send \
  -target http://receiver-a:49511,http://receiver-b:49511 \
  -replica sender -token-file ./probe.token -duplicates 3
```

Use `make test-unit` to run packages independently and `make test-integration`
for the three-replica, recovery, batching, encoding, and anti-entropy flow.

The CI workflow enforces formatting, unit tests, race detection, vet, decoder
fuzzing, static analysis, per-package coverage of at least 90%, and a Go 1.26
container verification.

### Quality and performance snapshot — 2026-07-28

This pre-release snapshot was collected on Go 1.26.5. It is reproducible
evidence for this revision, not a latency or throughput guarantee for every
workload.

- `make verify` passed: formatting, independent-package tests, integration and
  extreme scenarios, the race detector, vet, four 10-second decoder fuzz
  campaigns, `staticcheck`, `golangci-lint`, and a per-package coverage gate of
  at least 90%.
- `make docker-test` passed with Go 1.26; `govulncheck ./...` found no known
  vulnerabilities.
- A controlled three-host delivery probe confirmed idempotent duplicate
  delivery and rejected unauthorized, malformed, and oversized requests.
- The supplied benchmarks cover G-Counter and OR-Set `Merge`, `ApplyDelta`,
  and `MarshalBinary`. Run `make benchmark` on your target hardware before
  choosing capacity limits.

The OR-Set implementation avoids copying its own validated state during merge
and delta application. On the maintained 128-element benchmark, the current
Apple-silicon sample measured 46.6 microseconds and 57.8 KB per `Merge`, and
5.3 microseconds with zero allocations per duplicate `ApplyDelta` (12 logical
CPUs). Exact results depend on the CPU, Go version, element codec, set size,
and mutation mix.

Run `make test-extreme` to repeat the high-cardinality scenario in normal and
race-instrumented modes. Internal investigation data and deployment runbooks
are intentionally kept outside the public release tree.

## Publishing `v0.1.0`

Before publishing, run the verification commands above, review the public API,
commit the reviewed release contents, and ensure the repository is publicly
reachable at the module path. Then create an immutable semantic-version tag:

```sh
go mod tidy
go test ./...
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin v0.1.0
GOPROXY=proxy.golang.org go list -m github.com/darkinno/crdt@v0.1.0
```

Do not move or reuse a published tag. For Go modules, breaking changes after
the first stable release require a new major-version module path such as
`github.com/darkinno/crdt/v2`.

## Contributing

Please open an issue before proposing an API expansion. Contributions should
include focused tests, preserve deterministic wire encoding, keep untrusted
input bounded, and pass the verification commands above.

## License

SPDX-License-Identifier: MIT

Licensed under the [MIT License](LICENSE). Copyright (c) 2026 DarkInno.
