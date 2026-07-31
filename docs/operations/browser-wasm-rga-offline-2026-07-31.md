# Browser Go/Wasm RGA offline baseline — 2026-07-31

## Scope

This records controlled development evidence for the Go/Wasm RGA browser
append-log facade. It is not a mobile-device capacity claim, durability SLA,
remote receipt measurement, or proof of browser quota/power-loss behaviour.

## Memory persistence simulation

| Field | Value |
| --- | --- |
| Runtime | Node v26.5.0 |
| Workload | 256 one-rune local inserts; wait for each append; fresh same-actor recovery |
| Store | `MemoryRGAWasmBrowserPersistence` |
| Samples | 5 |

| Metric | Samples | Median |
| --- | --- | ---: |
| append + flush total | 55.197, 39.276, 33.030, 33.105, 30.944 ms | 33.105 ms |
| append + flush per mutation | 0.2156, 0.1534, 0.1290, 0.1293, 0.1209 ms | 0.1293 ms |
| restore 256 frames | 7.054, 5.489, 6.495, 5.232, 5.042 ms | 5.489 ms |
| retained canonical bytes | 15,083 bytes in every sample | 15,083 bytes |

Run:

```sh
make wasm-browser-benchmark
```

This isolates Go/Wasm merge, canonical frame retention, outbox accounting, and
append-log recovery. It excludes real IndexedDB scheduling, transport, TLS,
server acknowledgement, and device contention.

## Real browser IndexedDB check

The repository harness was served from the checked-out candidate and opened in
automated Chromium. It built and loaded the matching Go/Wasm artifact, wrote
128 RGA frames to real IndexedDB (one `flush()` per frame), closed, then
reopened the same actor record.

| Workload | Result |
| --- | ---: |
| Go/Wasm RGA IndexedDB append + flush, 128 frames | 49.7 ms total / 0.3883 ms per frame |
| close + reopen recovery of 128 frames | 5.1 ms |
| document text after recovery | 128 retained runes |
| native IndexedDB and BroadcastChannel regression path | PASS |
| browser console errors / framework overlay | none |

The browser observation validates actual IndexedDB transactions and the
compiled Wasm path on this host. It does not validate a second device/profile,
background-tab eviction, storage-quota pressure, remote durable acknowledgement,
or crash/power-loss semantics. Repeat it on supported browser/device versions
before choosing product interaction budgets.
