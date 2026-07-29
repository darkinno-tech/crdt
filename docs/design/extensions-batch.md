# Opt-in WebSocket batch design

[简体中文](extensions-batch.zh-CN.md)

## Status and scope

WebSocket batching is an optional live-relay transport feature. It is disabled
by default, requires the existing WebSocket feature, and introduces the
`crdt-sync-v2` subprotocol without changing any CRDT frame, manifest, HTTP, or
SSE contract.

The extension is deliberately not a durable replication service. It does not
create acknowledgements, replay history, an operation log, an outbox, or an
atomic application transaction.

## Contract

One batch contains a bounded ordered list of complete canonical `crdt-sync-v1`
change envelopes. Each item retains its own replica dot. A v2-capable client
offers v2 first and v1 second; a v1-only relay remains usable, but batch
publication returns an explicit unsupported error.

The relay validates the complete batch envelope, every manifest-bound change,
and every authorization decision before the first Inbox mutation. It then
admits items independently under the existing per-group ordering lock.

This is intentionally non-atomic. A generic application Apply callback has no
rollback interface, so a later item can fail after an earlier item is
accepted. In that case the relay forwards the accepted prefix before ending the
publishing connection. The caller keeps every source item in a durable outbox
and retries items independently. Retrying an already accepted dot is safe and
does not fan out a duplicate.

## Safety and capacity

- The feature switch is construction-time only. A batch feature without the
  WebSocket feature is rejected.
- A batch has both a total message-byte limit and an item-count limit.
- The default item limit is 16 and may not exceed the per-peer queued-message
  limit. A legacy WebSocket or SSE peer therefore queues a full batch of
  individual messages or is disconnected before a partial queue insertion.
- A batch-capable WebSocket peer queues one bounded batch message. Existing
  per-peer byte limits still apply.
- HTTP publishing and SSE events continue to carry individual v1 envelopes,
  preserving their current public contract and browser behavior.
- Authentication, write authorization, read authorization, strict origins,
  disabled compression, and the application-owned durable recovery boundary
  remain unchanged.

## Verification matrix

- Wire round trips, count limits, malformed/truncated input, and fuzzing.
- v2 publisher delivery to v2 and v1 WebSocket peers.
- Fallback to a v1-only relay and explicit batch rejection.
- Preflight authorization failure with no mutation or broadcast.
- Dynamic failure after an accepted prefix, proving the prefix is forwarded and
  a duplicate retry is not re-forwarded.
- Atomic multi-message peer queue admission, race checks, and loopback
  benchmarks normalized by logical changes rather than raw batches.
