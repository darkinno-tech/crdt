# Yjs manual callback boundary — design and validation, 2026-08-02

## Decision

`YjsTextBinding` now treats a synchronously failing manual `onLocalUpdate` or
`onLocalAwarenessUpdate` callback as an explicit local delivery-boundary
failure. It reports a stable `YjsBindingError`, latches only the affected
outbound path, and rejects later binding-owned writes before they create
another update that cannot be handed to the application-owned outbox.

The triggering Yjs transaction has already committed before Yjs emits its
update/awareness observer. It is therefore intentionally not rolled back.
The caller must repair its outbox/transport, recover the room through its
normal state-vector path, and attach a replacement binding rather than issue a
second editor mutation.

## Multi-dimensional assessment

| Dimension | Previous behavior | Decision and evidence |
| --- | --- | --- |
| Implementation | Local document and awareness observers directly invoked application callbacks. A thrown exception crossed Yjs's synchronous event stack unchanged; local byte-cap overflow merely reported an error. | Catch callback failures in the observer, record `local_update_failed` / `local_awareness_failed` (or `resource_limit`), then expose that stable code after the initiating method returns. Error-report callbacks are also contained. |
| Correctness | A local `Y.Text` change, undo, redo, or cursor state could commit even though the manual hand-off failed; the caller saw an arbitrary error and could continue producing more unhanded binding writes. | The origin mutation is documented as committed-but-delivery-unknown, and the relevant path is fail-closed. Text failures block later `applyLocalReplacement`, `undo`, and `redo`; awareness failures block later cursor writes. |
| Security and resources | Outbound local byte limits did not prevent later binding-owned writes after an oversized generated update. A throwing callback could re-enter callers with unbounded application error behavior. | Generated out-of-cap updates never reach the callback and latch the path. No document bytes are sent to `onError`; exceptions from `onError` are swallowed to avoid re-entering the synchronous observer loop. This is defense in depth; relay/store authentication, rate, heap, and durable-receipt limits remain mandatory. |
| Transport and durability | A callback could be mistaken for a successful send or receipt. | The contract explicitly calls it a synchronous hand-off. Durable delivery requires a caller-owned retry/outbox record before any fallible send; asynchronous transport failures remain above the binding. Awareness remains ephemeral. |
| Compatibility | Manual callback behavior was undocumented at this failure boundary. | No Yjs V1/V2 bytes, room identity, state-vector protocol, YJSStore schema, y-websocket envelope, or Go CRDT protocol changed. Standard providers remain separate transport owners. |
| Performance | The fix adds two local failure-state checks and only creates errors on a failure path. | Re-ran controlled native-Yjs core and incremental-editor benchmarks. Their measurements are local regression signals, not browser paint, network, persistence, or capacity claims. |

## Validation matrix

| Scenario | Command or test | Result |
| --- | --- | --- |
| Local update callback throws | Focused `yjs.test.mjs`: first local replacement commits, returns `local_update_failed`, ignores a throwing `onError`, and blocks a second replacement | Passed locally. |
| Generated local update over cap | Focused `yjs.test.mjs`: generated update never reaches callback, returns and latches `resource_limit` after its committed transaction | Passed locally. |
| Awareness callback and cap failures | Focused `yjs.test.mjs`: cursor state commits, returns stable `local_awareness_failed` / `resource_limit`, and blocks later `clearLocalCursor` | Passed locally. |
| Undo/redo failure boundary | Focused `yjs.test.mjs`: failed compensating undo returns `local_update_failed`; redo is rejected before another write | Passed locally. |
| Browser/editor + simulated replicas | `make typescript-test` uses a real CodeMirror 6 view under JSDOM plus V1/V2, state-vector, rich-text refusal, offline, and three-replica delayed/duplicate/reordered cases | Passed: 87 tests. JSDOM is not a target-browser/device result. |
| Real Yjs durable and relay path | `make yjs-store-test` runs real pinned Yjs V1/V2 durable recovery, corrupt/oversized rejection, parallel writers, Go-to-Node store integration, and official `y-websocket` relay integration | Passed locally: 7 Node tests and the selected Go integration test. It does not establish WAN/TLS/remote CI behavior. |
| Native Yjs controlled benchmark | `make typescript-yjs-core-benchmark`, Node v26.5.0, 16,384 UTF-16 units, 256 rounds, five samples | Warm state-vector sync: 0.013 ms/round; deep observer: 0.001–0.002 ms/event; binding undo/redo: 0.006 ms/operation. |
| Incremental editor benchmark | `make typescript-yjs-bindings-benchmark`, Node v26.5.0, 49,152 UTF-16 units, 512 remote edits, five samples | Incremental Y.Text delta: 0.018–0.022 ms/remote merge on warm samples, 512 incremental writes and zero full writes. Full-string baseline is faster in this Node-only test but performs 512 full writes, so it is not an editor UX or browser-paint result. |

## Follow-up plan

1. In the product transport, make the manual callback append a bounded,
   encrypted-at-rest where appropriate, application-owned outbox record before
   any network I/O; do not use a successful JavaScript callback return as a
   server receipt.
2. Add target-browser/device soak tests with the actual editor schema and
   1/4/16/64 authenticated receivers, slow outbox storage, thrown and rejected
   sends, reconnect/state-vector recovery, and explicit durable receipt loss.
3. Instrument only aggregate failure-latch/recovery counts and latency. Do not
   record Yjs update bytes, cursor payloads, document text, room secrets, or
   user identity in client diagnostics.

## Release boundary

The implementation and focused regression commits are `7234b05` and
`aee786b`. This record describes local validation on the beta candidate only;
it does not claim remote CI, a publication receipt, or production acceptance.
