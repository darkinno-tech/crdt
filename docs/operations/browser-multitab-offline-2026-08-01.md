# Browser multi-tab/offline controlled validation — 2026-08-01

## Scope

This record validates the TypeScript Collections structured-editor example, the
separate live/durable browser transport boundary, and the offline static-shell
policy. It is a controlled development record, not a remote durability,
production traffic, browser quota, or mobile-device capacity claim.

## Environment

| Field | Value |
| --- | --- |
| Revision | `cb42edf` beta base plus this candidate |
| Host | Darwin arm64 |
| Node | v26.5.0 |
| Go/Wasm | go1.26.5 |
| Persistence in the measured loops | deterministic in-memory facade |
| Samples | 5 |

## Correctness and safety evidence

| Scenario | Result |
| --- | --- |
| Collections editor: reverse + duplicate native updates | PASS; title, label set, outline, and 4 revision events converged |
| Native browser: append before live publication | PASS; a failed append produced zero live messages |
| Native browser: live path receipt boundary | PASS; sender retained 1 durable outbox item and receiver retained 0 |
| Go/Wasm RGA: live path receipt boundary | PASS with real run-v2 frames; sender retained 1 durable outbox item |
| Native offline simulation | PASS; 3 editors converged under reverse and duplicate deliveries |
| Go/Wasm offline simulation | PASS; 3 editors converged after duplicate/reordered delivery and recovery |
| Service Worker policy | PASS; only reviewed same-origin GET static URLs accepted; API, cross-origin, non-GET, opaque, and failed responses rejected |

`BroadcastChannelNativeTransport` is now a `NativeBrowserLiveTransport`, so it
cannot type-check as a durable `NativeBrowserTransport`. Its messages are
published only after the local persistence append; they remain bounded input at
the receiver and never cause outbox acknowledgement.

## Controlled performance samples

| Workload | Median | Notes |
| --- | ---: | --- |
| Native browser append + memory persistence, 512 writes | 0.6418 ms/write | Retained 79,544 canonical bytes; fresh recovery median 3.40 ms. |
| Native append + memory persistence + one live receiver, 512 writes | 1.3209 ms/write | Sender retained all 512 outbox entries because no durable receipt was supplied. |
| Collections 3-replica offline workboard, 288 updates | 1.552 ms/update | 544,776 encoded bytes; outlier samples reflect managed-runtime GC/scheduling. |
| Go/Wasm RGA append + memory persistence, 256 writes | 0.1071 ms/write | Retained 15,083 frame bytes; fresh recovery median 4.80 ms. |
| Go/Wasm RGA append + live receiver, 256 writes | 0.2144 ms/write | Sender retained all 256 outbox entries with no durable receipt. |

The live benchmarks use an asynchronous in-process hub to isolate the facade
and delivery boundary. They do not measure actual browser `BroadcastChannel`,
IndexedDB disk I/O, Service Worker startup/cache I/O, network receipts, TLS,
background termination, process/power loss, or mobile storage quota.

## Commands

```sh
make wasm
npm --prefix clients/typescript test
CRDT_WASM_DIR="$PWD/.tmp/crdt-rga-wasm" npm --prefix clients/typescript run test:compat
npm --prefix clients/typescript run bench:browser
npm --prefix clients/typescript run bench:collections
CRDT_WASM_DIR="$PWD/.tmp/crdt-rga-wasm" npm --prefix clients/typescript run bench:wasm-browser
```

No Chromium/WebKit executable was installed in the validation environment, so
the Service Worker itself was verified by its shared request/response policy
tests and source-scope review, not by a newly observed real-browser cache
session. Before release to supported browsers, run the example over HTTPS in
Chromium, Firefox, and Safari with a second tab, offline reload after worker
control, IndexedDB quota pressure, and a reconnecting authenticated relay.
