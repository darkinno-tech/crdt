# YJSStore Apply overhead validation — 2026-08-02

## Decision

Replace the `Apply` hot path's full pre-apply snapshot encoding with the
transaction outcome supplied by the pinned `yjs@13.6.31` engine. Keep all
other durable boundaries: a request still loads and semantically verifies a
fresh document, holds the per-document lock, enforces byte caps, and writes a
new snapshot only through the fsync/rename/directory-fsync sequence.

The predicate is exactly the condition Yjs uses before it emits an update:

1. a nonempty delete set changed the document; or
2. a client clock differs between the transaction's before and after states.

A state-vector comparison alone is rejected as an optimization: a pure delete
set changes visible state while leaving its state vector unchanged. The sidecar
runs the surrounding transaction as remote (`local=false`) so applying a
network update retains Yjs's remote-update behavior. It fails closed with 503
if the pinned runtime ever fails to expose that transaction.

## Review matrix

| Dimension | Decision and evidence |
| --- | --- |
| Implementation | Removes one full `encodeStateAsUpdate`/`encodeStateAsUpdateV2` before every apply. Reuses the state vector that `loadDocument` has already recomputed and matched to the persisted record. |
| Correctness | Direct V1 and V2 tests prove a delete-only update is `Applied=true`, advances the cursor, survives snapshot restoration, and its replay is `Applied=false` at the same cursor even though both vectors are identical. |
| Design | No cache, update log, protocol conversion, or API/configuration change. A duplicate still materializes and validates the persisted record, preserving corrupt-record rejection. |
| Security and resources | Existing input, snapshot, vector, request, admission, directory, bearer-token, and keyed-lock limits remain in force. The change retains one request-scoped `Y.Doc` and destroys it in `finally`; no references are retained. |
| Yjs compatibility | The code uses real pinned Yjs V1/V2 apply functions and the engine's own transaction criterion, rather than interpreting binary Yjs updates in Go or treating vectors as authorization/receipt evidence. |

## Controlled loopback results

Host: macOS/darwin arm64, Node `v26.5.0`, `yjs@13.6.31`. Baseline:
`origin/beta` at `5d68cde`; candidate ran immediately afterward with the same
committed lockfile. These are local durable-sidecar measurements, not browser,
WAN, TLS, authorization, cross-process, or production-capacity results.

Normal writes used the committed benchmark: a 256 KiB initial `Y.Text`, 80
measured iterations after 20 warmups, four real in-process receivers, loopback
HTTP, state-vector diff, snapshot read, and durable Apply. Values are ms.

| Variant | Apply p50 / p95 / p99 | Diff p50 / p95 / p99 | Snapshot p50 / p95 / p99 |
| --- | --- | --- | --- |
| Baseline | 11.775 / 13.115 / 14.621 | 2.434 / 2.917 / 3.418 | 2.832 / 3.433 / 4.138 |
| Candidate | 11.360 / 12.618 / 14.071 | 2.400 / 2.846 / 3.426 | 2.716 / 3.159 / 3.883 |

The Apply p50 fell 3.5%; p95 fell 3.8%. A small 4 KiB version of the same
workload was within scheduler/fsync noise (candidate Apply 10.428 / 12.089 /
12.925 versus baseline 10.664 / 11.905 / 14.139), so it must not be used to
claim a universal latency improvement.

The retry-focused real HTTP scenario used one already durable 512 KiB update,
25 warmups, and 250 duplicate applies. Every response had `Applied=false` and
cursor `1`; no request performed a durable write.

| Variant | Duplicate Apply p50 / p95 / p99 (ms) |
| --- | --- |
| Baseline | 4.949 / 6.166 / 7.322 |
| Candidate | 4.655 / 5.507 / 6.431 |

That is a 5.9% p50, 10.7% p95, and 12.2% p99 reduction for a replay workload,
where skipping both unnecessary full snapshots is material.

## Validation run

- `npm --prefix yjsstore/runtime test`: 11 real-Yjs Node tests, including V1,
  V2, nested types, recovery, malformed mutations, pure deletion, duplicate
  replay, admission timeout, and the 16-writer simulation.
- `CRDT_YJS_STORE_NODE_INTEGRATION=1 go test -count=1 . -run
  '^TestYJS.*Node.*Integration$'` from `extensions`: Go-to-Node fixed-update,
  vector, diff, snapshot, duplicate, and invalid-update contract.
- `make yjs-store-benchmark`: follows with the standard 1/4/16/64 receiver
  loopback sweep. Its figures remain controlled local baselines only.

The runtime still needs a production evaluation on the selected disk and
container heap, with the actual document size, write burst, gateway
authorization, browser provider, slow receivers, and process-isolation model
before changing an SLO or capacity limit.
