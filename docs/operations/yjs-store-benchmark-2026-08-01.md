# YJSStore controlled validation — 2026-08-01

This is a development baseline for the Level 1 `YJSStore` sidecar. It is not a
WAN, browser, TLS, authentication/authorization, process-isolation, or
production-capacity result.

## Correctness and safety evidence

`make yjs-store-test` installed the committed `yjs@13.6.31` lockfile and
passed all of the following:

- Direct real-Yjs V1 state-vector diff, update merge, nested `Y.Map`/`Y.Array`
  changes, snapshot recovery, and sidecar restart recovery.
- Direct real-Yjs V2 state-vector sync and rejection of a V2 update submitted
  to a V1-pinned document.
- A 16-writer offline simulation applied in parallel, followed by an exact
  convergence check and duplicate replay check (`cursor` stayed at 17).
- Invalid update, oversized update, invalid token, and manually corrupted
  snapshot record checks. Each rejected request retained the preceding durable
  snapshot.
- A deterministic 256-case mutation run against genuine Yjs updates. Every
  rejected case retained the snapshot observed immediately before it.
- A Go-to-Node sidecar integration check using the real fixed V1 `Y.Text`
  update, state vector, diff, snapshot, duplicate, and invalid-update paths.

Focused Go wire fuzzing also passed:

```text
go test -run='^$' -fuzz=FuzzDecodeYJSStoreBytes -fuzztime=10s -parallel=1 ./extensions
PASS; 134,550 executions, 48 new interesting inputs
```

The fuzz result covers Go-side response/identifier bounds only. The Node
mutation test covers sidecar request rejection and rollback behavior. Neither
proves arbitrary Yjs structural allocations are cheap, so deployments retain
raw-size limits, a Node heap/container ceiling, and ingress rate limits.

## Loopback durable benchmark

Command for each receiver count:

```sh
npm --prefix yjsstore/runtime run bench -- \
  --initial-bytes 4096 --iterations 20 --warmups 5 --receivers <1|4|16|64>
```

Host: Apple M4 Pro, macOS/darwin arm64, Node `v26.5.0`, `yjs@13.6.31`.
Each iteration appends a small real Yjs text update, durably applies it through
the loopback HTTP sidecar, sends a state-vector diff to each in-process Yjs
receiver, and reads a recovery snapshot. Limits were 1 MiB per update and 16
MiB per snapshot. Values are milliseconds.

| Receivers | Apply p50 / p95 / p99 | Diff p50 / p95 / p99 | Snapshot p50 / p95 / p99 | Mean diff bytes |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 10.074 / 10.611 / 11.348 | 1.896 / 2.133 / 2.136 | 1.855 / 2.482 / 2.488 | 22.75 |
| 4 | 10.974 / 13.628 / 19.771 | 1.955 / 4.030 / 5.586 | 1.860 / 2.976 / 3.014 | 22.75 |
| 16 | 10.966 / 13.839 / 14.464 | 2.584 / 4.157 / 9.797 | 2.525 / 4.400 / 4.620 | 22.75 |
| 64 | 13.819 / 44.478 / 50.167 | 2.089 / 4.931 / 11.468 | 2.362 / 8.126 / 9.715 | 22.75 |

The 64-receiver loop has serial local HTTP reads, so its p99 includes client
work and scheduler contention; it is not server fan-out throughput. Apply
includes materialization plus fsync/rename persistence. Repeat this workload
with the chosen disk, Node heap limit, realistic document size, update burst,
TLS termination, authorization policy, slow WebSocket receivers, and browser
provider before setting an SLO or production capacity number.
