# YJSStore request admission — design and validation, 2026-08-02

## Decision

The loopback-only Level 1 `yjsstore/runtime` now has an explicit request
admission and receive-time boundary before it collects an HTTP body or
materializes a Yjs document. The default allows four active semantic requests;
operators may choose `1..64` with `YJS_STORE_MAX_CONCURRENT_REQUESTS`.
Excess work returns `503 {"code":"unavailable"}` without a durable mutation.

The service also requires a nonzero `YJS_STORE_REQUEST_TIMEOUT_MS` in the range
one to 120 seconds (default ten). Incomplete headers are capped at the smaller
of the configured request deadline and five seconds. The 0700 data-directory
check is repeated immediately before the listener becomes live.

## Multi-dimensional assessment

| Dimension | Prior state | Decision and evidence |
| --- | --- | --- |
| Implementation | Every accepted Node HTTP request could enter `readBody`; the runtime depended on Node's five-minute default request timeout. Directory mode was checked only by `loadConfig`. | Gate before body collection, release in `finally`, use Node receive/header deadlines, and recheck the final data directory at `listen()`. Direct server construction keeps compatibility by receiving safe defaults. |
| Architecture | Per-document `KeyedLock` preserves one-writer durability, but process-wide active work was not explicitly budgeted. | Keep the existing document lock and wire API; add an independent process-wide active-request budget. This constrains queued locks, decoded request buffers, request-scoped `Y.Doc`s, and disk work without claiming cross-process HA. |
| Correctness | A capacity response had no defined test boundary; a partial upload could occupy a request while no Yjs transaction was possible. | Over-cap work returns `unavailable` before application body collection. Completion and Node's 408 timeout both release the slot. The 16-writer convergence test explicitly provisions 16 slots and still verifies duplicate cursor semantics. |
| Security | A direct embedding wrapper could bypass the configuration-time 0700 check, and slow request bodies had the Node default five-minute window. | Listen-time directory recheck closes the permission-drift/direct-construction gap. Nonzero header/body deadlines and bounded admission add defense in depth against local slow-body and heap/lock pressure. Loopback, token, size, checksum, Node heap, and gateway rate limits remain required. |
| Performance | Normal loopback operations repeatedly materialize and fsync a request-scoped document; unconstrained concurrency could turn an overload into memory growth and long tail latency. | The limit intentionally rejects overload instead of buffering it. Normal 1/4/16/64 receiver baselines remain recorded below; they measure no browser, TLS, WAN, authorization, or production capacity. |
| Compatibility | Go client already maps sidecar `unavailable` to `ErrYJSStoreUnavailable`. | No Yjs V1/V2, update bytes, document identity, snapshot schema, state-vector, y-websocket, or Go API changes. The two environment variables are optional defaults, and callers retry/recover above the binding. |

## Validation matrix

| Scenario | Command or test | Result |
| --- | --- | --- |
| Config and permission boundary | Direct loopback rejection; invalid concurrent/time configuration; change a loaded data directory to `0755` before `listen()` | Passed locally. The service refused to listen. |
| Real slow-body saturation | Real Node loopback half-body with one admission slot; a parallel snapshot received 503; completing the first request released the slot | Passed locally. No rejected request body was collected by the application. |
| Receive deadline | Real Node loopback half-body with a one-second deadline | Passed locally: Node returned 408 and a subsequent snapshot used the released slot. |
| Semantic regression | `npm --prefix yjsstore/runtime test` | Passed: 10 real Yjs tests, including V1/V2, state-vector diff, merge, restart, corrupt/malformed rejection, duplicate, and a configured 16-writer convergence simulation. |
| Go/Node durable relay | `make yjs-store-test` | Passed locally: the 10 Node tests and selected Go `TestYJS.*Node.*Integration` path, including real sidecar and official y-websocket relay behavior. |
| Locked dependency audit | `npm ci --ignore-scripts --prefer-offline` | Passed locally; npm reported zero known audit vulnerabilities for the locked runtime dependency graph. This is not a substitute for future advisory monitoring. |
| Controlled performance | `make yjs-store-benchmark`, Apple M4 Pro, Node v26.5.0, `yjs@13.6.31`, 4,096 initial UTF-16 units, 40 measured edits after 5 warmups, 1 MiB update / 16 MiB snapshot limits | Passed locally; results below. |

## Controlled loopback benchmark

| Receivers | Apply p50 / p95 / p99 ms | Diff p50 / p95 / p99 ms | Snapshot p50 / p95 / p99 ms | Mean diff bytes |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 10.299 / 11.283 / 11.699 | 1.763 / 2.180 / 2.332 | 1.762 / 1.948 / 2.527 | 20.875 |
| 4 | 11.135 / 12.700 / 13.175 | 1.698 / 1.868 / 2.178 | 1.726 / 1.875 / 2.576 | 22.875 |
| 16 | 10.993 / 13.821 / 15.335 | 1.750 / 1.897 / 2.365 | 1.792 / 1.895 / 2.721 | 22.875 |
| 64 | 11.327 / 18.041 / 24.601 | 1.660 / 1.779 / 1.964 | 1.700 / 1.848 / 3.662 | 22.875 |

The receiver loop is serial local HTTP work; it does not measure concurrent
writer throughput, browser rendering, public ingress, TLS, real gateway
authorization, external storage, or a service SLO. Admission protects overload
by rejecting surplus requests, not by making an unbounded workload faster.

## Follow-up plan

1. Set `MAX_CONCURRENT_REQUESTS`, the sidecar/Go timeout, and Node heap from a
   target-host 1/4/16/64 writer test with the production snapshot limit and
   disk latency. Include retry backoff and a durable outbox/recovery contract.
2. Export only aggregate active/rejected/timeout counts at the trusted process
   boundary. Never attach document bytes, Yjs client IDs, bearer tokens, or
   document identity to those metrics.
3. Keep public TLS, authentication, authorization, rate limiting, and
   cross-process writer fencing outside the bundled loopback runtime.

## Release boundary

Implementation and tests are `48ed32c` and `4f4de95`. This is a local beta
candidate record, not a remote-CI, production-capacity, or live-device claim.
