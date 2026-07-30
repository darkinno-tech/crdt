# Browser native client benchmark — 2026-07-30

## Scope

This report measures the new `native-ts-v1` browser facade's append/recovery
path. It is a controlled development measurement, not a WAN, remote-durable,
mobile-device, or production capacity guarantee. The protocol's explicit
update/object/log limits remain the safety boundary.

## Memory persistence baseline

| Field | Value |
| --- | --- |
| Host | Darwin 25.5.0 / arm64 |
| Runtime | Node v26.5.0 |
| Workload | 512 independent map writes; each waits for local append commit; then a fresh actor restores all state |
| Store | `MemoryNativeBrowserPersistence` (no IndexedDB I/O) |
| Samples | 5 |

| Metric | Samples | Median |
| --- | --- | ---: |
| append + flush total | 40.489, 33.105, 30.731, 30.444, 33.136 ms | 33.105 ms |
| append + flush per mutation | 0.0791, 0.0647, 0.0600, 0.0595, 0.0647 ms | 0.0647 ms |
| restore 512 updates | 5.689, 4.578, 3.874, 4.172, 4.232 ms | 4.232 ms |
| retained canonical bytes | 79,544 bytes in every sample | 79,544 bytes |

Run it with:

```sh
npm --prefix clients/typescript run bench:browser
```

The memory baseline isolates canonical update validation, append-log handling,
and reconstruction. It does not measure a browser disk transaction.

## Actual browser check

The repository browser harness was served from the local checkout and run in a
real automated Chromium browser. It built and loaded the matching Go/Wasm
artifact, used actual IndexedDB for a close/reopen recovery check, and delivered
one native array update through two same-origin `BroadcastChannel` transports.

| Workload | Result |
| --- | ---: |
| Go/Wasm RGA local merge + snapshot recovery | PASS |
| IndexedDB native document write, close, new-actor restore | PASS |
| BroadcastChannel two-tab style delivery | PASS |
| Console errors / browser error overlay | none |
| 128 IndexedDB append + `flush()` writes | 30.6 ms total, 0.2391 ms/mutation |
| fresh-actor restore of 128 entries | 3.4 ms |

These browser figures include the local test page, native document mutation,
canonical encoding/validation, and browser IndexedDB scheduling on this host.
They do not measure a remote receipt, TLS, a server operation log, an actual
second browser profile/device, quota pressure, or recovery after process/power
loss. Repeat them on target devices and selected browser versions before
choosing product limits or claiming an interaction latency budget.
