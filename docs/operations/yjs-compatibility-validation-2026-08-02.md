# Yjs compatibility validation — 2026-08-02

This record covers the Yjs relay awareness and Level 1 `YJSStore` lifecycle
fixes in `b979850`, `2519f6e`, and `8c65886`. It is a local development record,
not evidence of a production deployment, remote CI run, WAN capacity, or end
user receipt.

## Compatibility contract

- Awareness is ephemeral, never a durable acknowledgement. A client-selected
  awareness ID belongs to one WebSocket connection, not to an authenticated
  user who may have several browser tabs.
- A `null` awareness state at the active clock removes the state, matching the
  y-protocols rule. The relay retains a bounded, clock-only tombstone (default
  256); it retains no awareness JSON and rejects a delayed state at that clock.
- A Level 1 room keeps Yjs update semantics in the pinned Node sidecar. Go
  authenticates, authorizes, bounds, and relays; it does not claim to decode or
  translate Yjs state-vector semantics.
- Each sidecar operation materializes a `Y.Doc` from a durable snapshot and
  destroys it in `finally`. This is a resource-lifetime guarantee, not a claim
  that untrusted Yjs updates are intrinsically cheap; ingress, decoded-size,
  heap/container, and rate limits remain required.

## Validation matrix

| Scenario | Evidence | Result |
| --- | --- | --- |
| Equal-clock presence removal, stale replay, per-tab disconnect, and bounded tombstones | `go test -count=1 ./extensions -run '^TestYJSAwareness'` exercises `TestYJSAwarenessPreservesStandardNullClockAndTombstoneOrdering`, `TestYJSAwarenessDisconnectIsScopedToOneConnection`, and `TestYJSAwarenessTombstonesStayWithinConfiguredCapacity`. | Passed locally. |
| Request-scoped sidecar cleanup | `yjsstore/runtime/runtime.test.mjs` replaces `Y.Doc.prototype.destroy`, invokes apply/state-vector/diff/snapshot, and requires one destruction per materialization. | Passed locally. |
| Real Yjs semantic store and recovery | `make yjs-store-test` ran the committed `yjs@13.6.31` V1/V2 state-vector, merge, nested-type, duplicate, malformed/corrupt-record, restart, and deterministic mutation cases. | Passed locally. |
| Native-provider integration | The same target connected the official Node `y-websocket` provider through the Go relay and persistent Node sidecar, including concurrent/offline merge and fresh-client recovery. | Passed locally; this is a real local provider path, not browser/WAN evidence. |
| Go relay concurrency and wire safety | `go test -race -count=1 ./extensions -run '^TestYJS'` and `go test -run=^$ -fuzz=FuzzUnmarshalYJSMessages -fuzztime=10s ./extensions`. | Passed locally; the fuzz run completed 713,958 executions. |
| Project gates | `go test -count=1 ./...`, `go vet ./...`, and `make fmt-check generate-check`. | Passed locally. |

The repository policy runs the complete remote suite for a `beta` to
`preprod` pull request, not for every beta push. No remote CI result is
claimed by this document.

## Controlled local performance

Host: Apple M4 Pro, macOS/darwin arm64, Node `v26.5.0`, committed
`yjs@13.6.31`. The sidecar benchmark used a 4 KiB initial document, 40 measured
incremental edits after five warmups, loopback HTTP, 1 MiB update, and 16 MiB
snapshot limits. It appends one real Yjs text update, persists it, delivers
state-vector diffs to in-process receivers, and reads a snapshot.

| Simulated receivers | Apply p50 (ms) | Diff p50 (ms) | Snapshot p50 (ms) |
| ---: | ---: | ---: | ---: |
| 1 | 9.390 | 1.742 | 1.664 |
| 4 | 9.246 | 1.621 | 1.590 |
| 16 | 9.088 | 1.508 | 1.533 |
| 64 | 9.642 | 1.400 | 1.447 |

`go test -run=^$ -bench=BenchmarkYJSWireDecodeAndAdmission -benchmem
-benchtime=1s ./extensions` measured the Go relay's bounded opaque-wrapper
decode and admission path at `114.7 ns/op`, `136 B/op`, and `3 allocs/op`.

These are loopback baselines, not server fan-out throughput or a production
SLO: they exclude browser rendering, TLS, WAN loss/latency, authentication
storage, slow WebSocket receivers, quota backends, and real document shapes.
Before a release capacity claim, repeat the matrix with the target gateway,
heap limit, persistent storage, document distribution, and 1/4/16/64 real
browser clients; record p50/p95/p99 apply and reconnect latency, CPU, heap,
queue drops, sidecar errors, and recovery bytes.

## Release boundary

The fixes are ancestors of `origin/beta` as verified on 2026-08-02. They do
not modify `main`, do not add a Yjs-to-Go CRDT converter, and do not turn live
relay delivery into durable receipt or authorization evidence.
