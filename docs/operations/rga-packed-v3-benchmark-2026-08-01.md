# Packed RGA v3 controlled byte and performance validation — 2026-08-01

## Scope

This report measures the explicitly negotiated packed RGA v3 candidate against
unchanged run-v2 and a real pinned `yjs@13.6.31` comparison harness. It is a
development regression record, not a cross-runtime speed ranking, WAN/TLS,
provider, mobile battery, storage, or production-capacity claim.

Packed v3 retains one scalar HLC `Position` and causal parent per character.
It emits a bitmap plus positive wall-time gaps only for a dense local HLC
chain, so a receiver reconstructs precisely the same node set before its
ordinary RGA merge. Non-dense or non-beneficial chains remain regular run
blocks.

## Environment and method

| Item | Value |
| --- | --- |
| Candidate base | `2efc4e8` (`origin/beta`) plus the three packed-v3 commits |
| Host | Apple M4 Pro, Darwin arm64 |
| Go / Node | Go `1.26.5` / Node `v26.5.0` |
| Yjs | `13.6.31`, exact committed `bench/competitors/package-lock.json` |
| Samples | 3 reported, 1 warmup, 10 operations per sample |
| Text | ASCII `x`, 4,096 and 16,384 Unicode scalars |

The initial cell creates two new replicas, inserts at offset zero, encodes,
decodes/applies, checks text equality, then records the update and complete
state frame sizes. The offline cell seeds three replicas, makes two concurrent
middle replacements, redelivers duplicates in reordered order, verifies
convergence, and records base plus two unique updates and final state.

Commands:

```sh
go run ./cmd/crdt-compare -protocol=run-v2 -scenario=initial -sizes=4096,16384 -samples=3 -warmups=1 -iterations=10
go run ./cmd/crdt-compare -protocol=packed-v3 -scenario=initial -sizes=4096,16384 -samples=3 -warmups=1 -iterations=10
npm --prefix bench/competitors ci --ignore-scripts --prefer-offline
npm --prefix bench/competitors run yjs -- --scenario initial --sizes 4096,16384 --samples 3 --warmups 1 --iterations 10
go test -run='^$' -bench='BenchmarkRGADeltaWireProtocols/(run-v2|packed-v3)$|BenchmarkRGAMarshalLinearDocument/(run_v2|packed_v3)$' -benchmem -benchtime=200ms -count=3 -cpu=1 ./text
```

## Byte results

| Scenario / runes | run-v2 update / state | packed-v3 update / state | Yjs update / state | packed-v3 versus run-v2 | packed-v3 versus Yjs |
| --- | ---: | ---: | ---: | ---: | ---: |
| Initial 4,096 | 36,774 / 36,774 B | 4,656 / 4,656 B | 4,113 / 4,113 B | -87.3% | +13.2% |
| Initial 16,384 | 147,239 / 147,239 B | 18,483 / 18,483 B | 16,403 / 16,403 B | -87.4% | +12.7% |
| Offline 4,096 | not rerun for this record | 4,831 / 4,779 B | 4,184 / 4,189 B | n/a | +15.5% / +14.1% |
| Offline 16,384 | not rerun for this record | 18,659 / 18,607 B | 16,473 / 16,477 B | n/a | +13.3% / +12.9% |

The byte comparison is valid only for this shared plain-text trace. It does
not make the protocols interoperable or establish identical rich-text, storage,
GC, allocation, or network behavior. The HLC timestamp width can vary with
clock time; the percentage and Yjs comparison are the useful signals.

## Local codec measurements

The following Go-only medians are from three 200 ms, one-CPU samples. They are
included to make the byte reduction's CPU/allocation cost visible.

| Workload | run-v2 median | packed-v3 median | Allocation/op |
| --- | ---: | ---: | ---: |
| Complete 100,000-scalar state marshal | 18.35 ms | 19.80 ms | 7.69 MiB / 6 allocs → 8.34 MiB / 9 allocs |
| 4,096-scalar delta encode + decode | 2.82 ms | 2.80 ms | about 3.08 MiB / 157 allocs → 3.01 MiB / 167 allocs |

Packed v3 is a wire-byte optimization, not a claim that every local CPU metric
falls. The dense-chain plan uses one bitmap, UTF-8 text buffer, and exact-sized
wall-gap slice; it avoids repeated growth allocations, keeping the measured
codec latency close to run-v2 while massively reducing bytes. Before an SLO,
repeat with production document shape, multi-writer branching, tombstone
ratio, encrypted persistence, actual provider framing, and target device CPU.

## Correctness and security evidence

- `go test ./...`, the 90% per-package coverage gate, and `go test -race ./text` passed.
- `FuzzRGAPackedUnmarshal` completed 150,000 one-worker executions (three new
  interesting inputs) without panic or an invalid decoded delta.
- Unit/integration tests cover byte-for-byte canonical re-encoding, preserved
  Positions, ordinary-chain fallback, malformed unused bitmap-bit rejection
  with unchanged receiver state, bounded snapshot recovery, manifest/inbox
  admission, duplicates, reordered concurrent edits, and snapshot deltas.
- The actual `yjs@13.6.31` harness passed its initial and offline-concurrent
  convergence cells. That is a controlled comparator, not a translation or
  interoperability test; the Yjs relay/store remains opaque and separately
  authorized.

Use packed v3 only after exact manifest authentication and compatible limits.
It does not authenticate peers, encrypt text, prove receipt, or authorize
tombstone GC.
