# Controlled performance-regression CI

The `performance` job in `.github/workflows/test.yml` compares the candidate
revision with its pull-request base (or the previous `beta` tip for a push) on
the same GitHub runner. It is a regression guard, not a production capacity
claim: repeat focused benchmarks on the target CPU, storage, Go version,
network and workload before setting a product limit.

## Covered workloads

| Layer | Benchmark | What one operation proves |
| --- | --- | --- |
| CRDT data plane | `BenchmarkGCounterApplyDelta` | One idempotent in-memory delta application. |
| Large document | `BenchmarkRGAApplyDeltaLinearChain` | Apply a bounded 100,000-node RGA delta to fresh state. |
| Transport | `BenchmarkProviderEndToEndRelayFanout` | One authenticated loopback WebSocket publish is decoded and installed by 1, 4, and 16 receivers. |

Every workload runs five times with `GOMAXPROCS=1`, `-cpu=1`, `-benchmem`, and
`-benchtime=100ms`. The checker requires all five samples and compares
medians. It fails if `ns/op` more than doubles, or `B/op` or `allocs/op` rises
by more than 5%. Hosted-runner timing is intentionally a coarse guard because
the real transport scenario is scheduler-sensitive; the allocation allowance
accounts for harmless runtime variation while still rejecting material
retained-work growth.

The baseline and candidate must expose the exact same benchmark names. This
prevents a renamed, skipped, or newly added sub-benchmark from silently
weakening the gate.

## Reproduce locally

Check out the revision to compare against alongside the candidate, then run:

```sh
BENCHMARK_BASE=../crdt-baseline make benchmark-regression
```

The raw baseline and candidate output stays under `.tmp/benchmark-results/`.
For a measurement report rather than a gate, use the focused `go test -bench`
commands in the relevant integration or operations guide and record the
machine and workload.
