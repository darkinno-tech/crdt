# Packed RGA v3 outer-v2 initial-sync validation — 2026-08-02

`packed-v3-v2` is an opt-in artifact for a peer whose manifest binds packed
RGA semantics v3 to outer frame format v2.  It keeps the TypeIDs and decoded
canonical packed payload unchanged; the outer frame may use its existing
bounded DEFLATE representation when that is smaller.  The measurements below
compare it with the same packed-v3 payload in outer frame v1.

These are controlled local results, not a WAN, TLS, browser-device, or
production-capacity claim.  In particular, all content is intentionally
repetitive, which is favorable to compression.  Frame v2 emits raw payload
when compression is not smaller, so incompressible content needs its own
deployment measurement.

## Method

All Go benchmarks used one CPU (`GOMAXPROCS=1`, `-cpu=1`), three samples, and
their median.  The compare CLI used one warm-up, three samples, and ten
iterations per sample.

| Scenario | Command / workload | What it exercises |
| --- | --- | --- |
| Initial, two replicas | `go run ./cmd/crdt-compare -protocol packed-v3[-v2] -scenario initial` | initial insertion, packed snapshot frame, decode, and convergence |
| Offline concurrent, three replicas | `go run ./cmd/crdt-compare -protocol packed-v3[-v2] -scenario offline-concurrent` | duplicate and reordered concurrent replaces before convergence |
| Go Wasm runtime | `go test -run '^$' -bench 'BenchmarkRuntimeInitialSnapshotAndRestore65536Runes/(packed_v3|packed_v3_outer_v2)$' -benchmem -benchtime=1s -count=3 -cpu=1 ./internal/wasm` | 65,536 Chinese runes, snapshot plus restore through the Wasm-facing runtime |
| Compiled Wasm compatibility | `make wasm-packed-v2-test` | actual Go Wasm module under its local HTTP harness, durability/outbox, live transport, offline recovery, and snapshot restore |

## Results

The first two rows show frame bytes after an initial insertion.  The latter
two additionally include a three-replica offline convergence workload.  Time
is an observed median on this machine; it is included to make the byte/CPU
trade-off visible rather than as a throughput target.

| Workload | packed-v3 v1 update / state | packed-v3-v2 update / state | Byte reduction | v1 median | v2 median |
| --- | ---: | ---: | ---: | ---: | ---: |
| Initial 4,096 repeated ASCII runes | 4,656 / 4,656 B | 87 / 87 B | 98.13% | 6.923 ms | 6.846 ms |
| Initial 16,384 repeated ASCII runes | 18,484 / 18,484 B | 109 / 109 B | 99.41% | 31.025 ms | 30.677 ms |
| Offline concurrent 4,096 runes | 4,831 / 4,779 B | 273 / 138 B | update 94.35%; state 97.11% | 18.330 ms | 28.982 ms |
| Offline concurrent 16,384 runes | 18,659 / 18,607 B | 295 / 169 B | update 98.42%; state 99.09% | 73.953 ms | 76.597 ms |

For the Wasm-facing 65,536-Chinese-rune snapshot+restore benchmark, the
outer-v2 median snapshot was **1,079 B**, versus **204,858 B** for outer-v1
(-99.47%).  Median end-to-end benchmark time changed from **129.281 ms** to
**131.552 ms** (+1.76%).  Allocation and runtime differences were not used as
an acceptance target: this is a compression trade-off measured on a single
machine.

## Correctness and safety gates

- The v2 encoder wraps the same canonical packed payload; byte-for-byte
  payload equality, snapshot restore, anti-entropy delta, and mutator
  preflight atomicity are covered by focused Go tests.
- A runtime requires the exact manifest-bound outer frame version before it
  applies a delta or snapshot.  v1 and v2 packed artifacts deliberately reject
  each other.
- Selecting the expected decoder uses only a bounded frame-version peek; full
  decoding still validates the frame checksum and `MaxPayload` before any RGA
  mutation.  The peek is never authentication or acceptance evidence.
- Outer v2 is compression only.  Deployments still need the negotiated
  manifest, transport-body cap, authorization, and TLS/integrity guarantees.
- `make wasm-packed-v2-test` passed 89 tests against the compiled Go Wasm
  module.  It is a real artifact loopback, but it is not evidence of a remote
  browser/device network path.

## Reproduction boundary

Use the new `packed-v3-v2` comparison protocol only for an explicit A/B
measurement.  Do not infer a saving for random, encrypted, or already
compressed input from these repetitive-text results.  TypeScript's standalone
frame helper remains outer-v1-only; its Wasm runtime may load this artifact
only after exact protocol negotiation.
