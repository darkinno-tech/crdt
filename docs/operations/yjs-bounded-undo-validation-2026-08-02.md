# Yjs bounded local undo — design and validation, 2026-08-02

## Decision

The browser `YjsTextBinding` now bounds the `Y.UndoManager` it owns to 256
local stack items by default. An application may select another positive safe
integer with `maxStackItems`. This applies only to local plain-text UI history;
it does not change Yjs update bytes, V1/V2 selection, state vectors, document
identity, durable recovery, or the authenticated transport contract.

## Multi-dimensional assessment

| Dimension | Existing state | Decision and evidence |
| --- | --- | --- |
| Implementation | `captureTimeout` grouped entries but left `Y.UndoManager`'s `undoStack`/`redoStack` unbounded. | Before a binding-owned replacement at capacity, clear the complete Yjs history through `UndoManager.clear()`, then record the new replacement. |
| Correctness | A simple array splice would make an arbitrary old action unavailable without releasing Yjs's retained deleted structs. | Keep Yjs's public full-stack release semantics. The newest replacement remains undoable, redo remains a compensating update, and all prior history is intentionally local-only. |
| Security and resources | A long-lived editor could accumulate local stack entries even while visible text stays within `maxTextUTF16`. | Default cap is 256; zero, negative, fractional, and unsafe values fail with `invalid_options`. This is defense in depth, not a substitute for server/store update, heap, and rate limits. |
| Compatibility | Standard providers and servers own update/awareness exchange. | No wire, package, room, persistence, or provider changes. V1/V2 remain explicitly pinned by the existing binding configuration. |
| Performance | Clearing at capacity is an infrequent local UI operation; a normal edit must stay incremental. | Controlled benchmark keeps the existing 256-entry undo/redo workload. A separate cap-two regression exercises the reset and subsequent undo/redo semantics. |

## Validation matrix

| Scenario | Command or test | Result |
| --- | --- | --- |
| Bounded local history | `clients/typescript/test/yjs.test.mjs` cap-two case: three local edits, reset, undo/redo, a new local edit, invalid zero cap | Passed locally. |
| Native browser-editor path | `make typescript-test` with a real CodeMirror 6 view under JSDOM, V1/V2, formatted-text refusal, awareness, state-vector and reordered/duplicate delivery tests | Passed: 82 tests. JSDOM is not a real browser/device result. |
| Real semantic sidecar path | `make yjs-store-test` runs pinned real Yjs V1/V2 store recovery plus Go-to-Node and official `y-websocket` relay integration | Passed locally. It does not measure WAN, TLS, browser paint, or remote CI. |
| Core controlled benchmark | `make typescript-yjs-core-benchmark` on Apple M4 Pro / Node v26.5.0, 16,384 UTF-16 initial units and 256 rounds | Warm samples: state-vector SyncStep 0.013–0.019 ms/round; undo/redo 0.006–0.007 ms/operation. |
| Incremental editor benchmark | `make typescript-yjs-bindings-benchmark` with 49,152 initial UTF-16 units and 512 remote edits | 512 incremental range writes and zero full writes; warm samples 0.021–0.026 ms/remote merge. |

## Follow-up plan

1. Before claiming a deployment capacity, run the same cap-reset and
   state-vector flows in the target browser/editor schema with 1/4/16/64
   authenticated receivers, slow peers, storage latency, TLS, and reconnects.
2. If product requirements need user-visible history across a reload, design a
   separate, encrypted application history store with retention and identity
   rules. Do not serialize Yjs internal undo stack items as a durable log.
3. Keep Level 2 rich-text integration on maintained schema-specific Yjs
   bindings; the bounded plain-text layer must continue to reject formats and
   embeds rather than flattening them.

## Release boundary

Local evidence was recorded for commits `a872191` and `13adb26`. The beta
branch remains the release candidate; no remote CI, production acceptance, or
service SLO is implied by these measurements.
