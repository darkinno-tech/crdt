# Browser Go/Wasm RGA: local-first persistence boundary

## Decision

`openRGAWasmBrowserDocument` adds an opt-in browser facade for the existing
manifest-selected Go/Wasm RGA runtime. It owns one actor's recovery lifecycle:

```text
local RGA edit / authenticated received frame
              |
              v
      bounded Go/Wasm RGA merge
              |
              v
  captured complete snapshot + canonical frame append
              |
              +--> durable local outbox (local frames only)
              |                 |
              v                 v
       IndexedDB RGA stores   authenticated transport receipt
```

The facade is separate from `openNativeBrowserDocument`. `native-ts-v1` JSON
updates are not Go RGA frames, so the two implementations use different
interfaces and different `rga-documents` / `rga-updates` IndexedDB stores.
They share neither snapshot bytes nor delta records.

## Record and lifecycle

The persistence key is an unambiguous pair of `{ documentID, replicaID }`.
This prevents a concurrently active second tab from restoring an old tab's HLC
identity by accident. The application must supply a stable replica ID only for
one non-concurrent actor recovery scope; a fresh tab must use a fresh actor
and its own storage scope.

Every record contains:

1. an optional complete `RGASnapshot` (`state`, `clock`, and `frontier`);
2. a bounded append log of canonical RGA delta frames;
3. monotonically allocated local log sequence numbers; and
4. a `pending` bit for locally produced frames until `transport.send` resolves.

Appending a frame and advancing log accounting is one IndexedDB transaction.
The facade takes a complete snapshot immediately after each accepted mutation,
before later synchronous editor events can affect it. It can compact only that
captured state, only with no pending local receipt, and only after configured
update/byte thresholds. An out-of-order RGA state that cannot snapshot remains
an append log until its dependencies resolve; it is not serialized as a
partial checkpoint.

## Safety and security

- The Go/Wasm runtime still validates frame type, canonical encoding, tags,
  node/pending/resource limits, and applies a frame atomically before it is
  persisted. Hosts must cap network bodies before allocating a `Uint8Array`.
- IndexedDB is an availability boundary, not authentication. The caller must
  bind manifest, group, schema, epoch, actor authorization, and receipt policy
  in its transport. A WebSocket enqueue is not a durable server receipt.
- The recovery store is bounded by the existing browser defaults: compaction
  at 128 updates or 1 MiB, rejection at 10,000 updates or 32 MiB. A limit or
  storage failure is reported; no update is silently discarded.
- `flush()` proves the browser completed requested IndexedDB work. It does not
  prove power-loss durability, quota eviction survival, peer receipt, or a
  remote operation-log commit.
- `Position`/Tag anchors stay local or authenticated presence metadata. They
  are never added to an RGA state/delta frame or durable outbox.

## Alternatives considered

| Alternative | Decision |
| --- | --- |
| Persist only `RGAWasmDocument.snapshot()` on each edit | Rejected: correct but rewrites total state on every keystroke. |
| Reuse `native-ts-v1` IndexedDB records | Rejected: state and wire contracts differ, risking protocol confusion. |
| Persist RGA state without clock/frontier | Rejected: a recovered actor can reuse tags or lose delivery context. |
| Append log + complete snapshot + receipt-gated compaction | Selected: bounds normal write cost and supports offline recovery/retry. |

## Verification

`make wasm-test` covers real Go/Wasm Position/Tag anchors, UTF-16 selection
preservation, durable outbox retry, receipt-gated compaction, three offline
actors, reverse/duplicate delivery, and recovery. The browser harness uses
real Chromium IndexedDB for 128 append/flush operations followed by a close /
reopen check. Controlled development measurements are recorded in
[`browser-wasm-rga-offline-2026-07-31.md`](../operations/browser-wasm-rga-offline-2026-07-31.md).
