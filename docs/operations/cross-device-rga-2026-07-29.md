# Controlled cross-device RGA evidence — 2026-07-29

This is a short-lived test of two Linux/amd64 hosts in the same region. Host
addresses, credentials, tokens, and business data are omitted. It is evidence
for this controlled path, not a production SLA.

> Historical protocol boundary: these figures were captured before
> `crdt-sync-probe` defaulted to stable RGA run-v2 frames (19/20). The legacy
> scalar v1 path (11/12) now requires explicit opt-in on both endpoints, so
> this record is a baseline only and must not be used as run-v2 performance
> evidence.

## Safety and method

- A statically built `crdt-sync-probe` binary was SHA-256 verified after upload
  to each host.
- A random one-time token authenticated the temporary HTTP probe. No business
  data was sent. Both listeners, token files, binaries, and logs were removed
  after the test.
- Each successful delta POST now returns a zero-body `204 No Content` with
  `X-CRDT-Apply-Micros`; complete state is built only for the final `/state`
  convergence request.
- Small-edit samples use a fresh process and final `/state` read for every
  sample, so they include more overhead than a persistent application session.

## Results

### Bidirectional small edits

Twenty independent one-rune RGA edits were sent with one delivery each.

| Direction | Mean | p50 | p95 | p99 / max |
| --- | ---: | ---: | ---: | ---: |
| Host B -> Host A | 4.90 ms | 4.80 ms | 5.40 ms | 5.97 ms |
| Host A -> Host B | 5.04 ms | 4.94 ms | 5.54 ms | 6.13 ms |

These samples meet a 50 ms same-region target with substantial headroom. They
do not imply that a client whose network RTT alone exceeds 50 ms can meet the
same target.

### Mutual delivery and convergence

1. User A delivered a 4,096-rune RGA delta and a counter increment to both
   hosts three times.
2. User B did the same with a different RGA delta and actor.

Both hosts reported the same final state: `8,192` visible runes, identical
SHA-256 `db1e71cd1a95dd101d2ae00c60c2afc8bdad0048489f90454fd93d0a521ef1e5`,
zero pending entries, and counter values `{user-a: 1, user-b: 1}`. This proves
idempotent duplicate delivery and cross-host convergence for the exercised
case; it is not a substitute for durable outbox, anti-entropy, or recovery
testing.

### Large paste boundary

A 200,000-rune delta delivered twice from Host B to Host A took 4.27 seconds
before the ACK change and 4.03 seconds after it. This single-run reduction is
small, which strongly suggests that constructing, decoding, applying, and
finally verifying a 200K-node RGA delta dominate this path. Capture a CPU and
allocation profile before attributing the remaining time to one of those steps.

Treat a large paste as a resumable bulk-sync operation. It cannot share the
same <=100 ms SLO as a one-character interactive edit. The v2 WebSocket batch
transport amortizes incremental operation framing; it does not make a single
200K-node delta interactive.

## Reproduction boundary

Use a disposable token, a temporary port restricted to the test hosts, and
remove all temporary material afterward. Do not run this HTTP probe as a public
or production endpoint: a production relay needs TLS, authenticated durable
outbox/ACK semantics, reconnect recovery, rate limits, and observability.
