# Yjs live subscription revocation — design and validation, 2026-08-03

## Decision

Keep Yjs documents end-to-end in Yjs and retain the Level 0 relay / Level 1
`YJSStore` split. The relay does not parse or translate Yjs updates into Go
RGA, rich-text, manifest, cursor, or awareness state. This change closes one
production-readiness gap at that transport boundary: an optional,
context-bounded `YJSConfig.RevalidateSubscription` periodically rechecks a
live reader's application authorization after WebSocket upgrade.

When the callback returns an error or reaches `RevalidateTimeout`, the handler
closes that subscriber and drops queued fan-out work. The maximum normal
revocation window is the selected `RevalidateInterval + RevalidateTimeout`.
An already in-flight network write cannot be recalled; callers that need a
smaller exposure window must select a smaller measured interval and provision
their policy backend accordingly. The callback receives only the authenticated
`Peer` and configured room, never Yjs client IDs, awareness JSON, update bytes,
or state vectors.

## Multi-dimensional assessment

| Dimension | Evidence and decision |
| --- | --- |
| Architecture | `YJSHandler` remains the authenticated WebSocket boundary and `YJSStore` remains the only Yjs-aware durable engine. No Go/Yjs live-operation bridge is introduced. |
| Correctness | A denied or timed-out recheck closes the exact WebSocket; the normal `ServeHTTP` cleanup removes only that connection's awareness state. Rechecks are serial per subscriber, so one slow policy lookup does not create overlapping authorization work. |
| Security | Upgrade authorization remains mandatory. Revalidation is opt-in for backward compatibility, but deployments with revocable access must configure it. The callback's context is canceled on close or timeout; duration values without a callback are rejected. |
| Resource bounds | One timer and at most one in-flight policy lookup exist per subscriber. With a callback, zero values default to a one-minute interval and five-second timeout, with that timeout capped at any shorter explicit interval; applications must choose a shorter value only after measuring policy-store load. |
| Compatibility | The y-websocket / y-protocols envelope, Yjs V1/V2 selection, state-vector behavior, awareness wire bytes, store identity, and durable cursor are unchanged. Existing deployments retain connection-lifetime authorization unless they opt in. |
| Operations | Export only aggregate authorization outcome and latency metrics at the trusted gateway. Do not attach document contents, client IDs, bearer tokens, or raw room identity to telemetry. Treat a callback timeout as deny, investigate it separately, and recheck current access rather than cached cookie text. |

Yjs updates are commutative and idempotent, while state vectors support
missing-update exchange; neither property is authorization or receipt evidence.
See the [Yjs document update API](https://docs.yjs.dev/api/document-updates),
[Awareness guidance](https://docs.yjs.dev/api/about-awareness), and
[y-websocket provider documentation](https://docs.yjs.dev/ecosystem/connection-provider/y-websocket).

## Validation matrix

| Scenario | Command or test | Result and scope |
| --- | --- | --- |
| Revoked live reader | `TestYJSHandlerRevalidatesAndClosesRevokedSubscription` | Passed locally: a live WebSocket was closed after a revoked recheck and removed from the room. |
| Stalled policy backend | `TestYJSHandlerClosesWhenSubscriptionRevalidationTimesOut` | Passed locally: the callback context expired and the subscriber was closed fail-closed. |
| Standard provider revocation | `TestYJSHandlerNativeYWebsocketRevocationNodeIntegration` through `make yjs-store-test` | Passed locally: a real pinned `y-websocket` provider observed an authenticated connection followed by the revalidation-driven close. It is not a browser/device result. |
| Relay race safety | `go test -race -count=1 ./extensions -run '^TestYJS'` | Passed locally. Covers handler, bounded awareness, store-backed handshake, and new revalidation paths. |
| Decoder / client bounds | `FuzzUnmarshalYJSMessages` and `FuzzDecodeYJSStoreBytes`, each `-fuzztime=50000x -parallel=1` | Passed locally with 50,000 executions per target. This is a bounded local fuzz run, not a security proof. |
| Real semantic store and provider | `make yjs-store-test` | Runs pinned real Yjs V1/V2 semantic tests plus Go-to-Node and standard `y-websocket` provider integration. It is a Node loopback client result, not browser/device or remote-CI evidence. |
| Controlled durable load | `make yjs-store-benchmark` | Apple M4 Pro, Node v26.5.0, `yjs@13.6.31`, 4 KiB initial text, 40 measured edits after 5 warmups: at 64 receivers Apply p50/p95 10.063/12.396 ms, Diff 1.475/1.830 ms, Snapshot 1.532/1.799 ms. Local loopback only; it excludes TLS, WAN, gateway authorization, browser rendering, and production disk contention. |
| Relay decode microbenchmark | `go test -run '^$' -bench='BenchmarkYJSWireDecodeAndAdmission$' -benchmem -benchtime=1s ./extensions` | Apple M4 Pro: 118.9 ns/op, 136 B/op, 3 allocs/op. It exercises only opaque wrapper decode/admission. |

## Production acceptance before enablement

1. Select and load-test the revalidation interval/timeout with the production
   authorization store. Alert on callback timeout, denial, sidecar unavailable,
   queue-drop, and reconnect rates without logging Yjs content.
2. Exercise real browsers and the selected editor binding over TLS with
   Secure/HttpOnly cookies; revoke read access while the document is active and
   confirm the target connection closes within the configured bound.
3. Run slow-receiver, policy-store latency, sidecar restart, denied write,
   duplicate/reordered update, durable recovery, and backup/restore tests with
   target document shapes and quotas.
4. Keep Yjs and Go CRDT identities, persistence, awareness, and migration
   histories separate. A cross-CRDT migration remains an offline, one-way,
   epoch-fenced export/import with an explicit loss report.
