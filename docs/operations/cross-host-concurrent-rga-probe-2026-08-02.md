# Cross-host concurrent RGA probe — 2026-08-02

## Scope and safety boundary

This is a controlled two-host acceptance test of the beta commit
`ecde311c5fbfe673c4925900a0cde110bd78c04e`. Both Linux amd64 hosts had four
vCPUs and about 3.8 GiB of memory. Host addresses, SSH credentials, and the
one-time bearer token are intentionally omitted.

The test used the statically built `crdt-sync-probe` binary, SHA-256
`d49c8649ac802dce5582fa790a632ab5fc84aefedc8dc4922118dc052d832a70`.
The hash matched after upload to both hosts. Each receiver bound only to
`127.0.0.1:49511`; local SSH port forwards reached those loopback listeners.
This avoided exposing the probe's intentionally short-lived HTTP bearer-token
endpoint on hosts that had no pre-existing ingress policy for the test port.

The probe exercises bounded frame decoding and in-memory CRDT application. It
does not negotiate a `replica.Manifest`, use `replica.Inbox`, persist an
operation log or snapshot, authenticate a production identity, provide TLS at
the probe endpoint, or establish a production latency/capacity SLO.

## Acceptance scenarios

| Scenario | Workload | Required outcome | Result |
| --- | --- | --- | --- |
| Duplicate delivery baseline | One run-v2 RGA delta of 4,096 runes, one counter increment of 2, one OR-Set element, each delivered five times to both receivers | One counter component of 2, one set element, equal RGA digest, zero pending | pass |
| Concurrent receivers | Eight distinct replicas concurrently sent one counter increment, one distinct OR-Set element, and an 8,192-rune run-v2 RGA delta; each delivery was repeated three times to both receivers | Equal final state on both receivers; every counter component exactly one; all elements present; no RGA pending nodes | pass |
| Unauthenticated read | `GET /state` without token on each receiver | HTTP 401 | pass |
| Malformed bounded input | Authenticated invalid body to `/counter` | HTTP 400 and unchanged state | pass |
| Oversized bounded input | Authenticated body of 1 MiB plus one byte to `/counter` | HTTP 400 and unchanged state | pass |
| Protocol separation | run-v2 receiver given a scalar-v1 RGA delta | HTTP 400 and unchanged state | pass |

The final concurrent state on both receivers was byte-identical:

- counter: baseline component `2`, plus eight distinct concurrent components
  each equal to `1`;
- OR-Set: the baseline item plus all eight distinct concurrent items;
- RGA: `run-v2`, `69,632` runes, SHA-256
  `09c2ad1bac49f1d70dc5b087975095eb91472249a0273c6bb1aa1d460407506f`,
  and `pending=0`.

## Cleanup and interpretation

The recorded probe PIDs were checked before stop. Both listeners were absent
afterward; the two remote test directories, one-time tokens, binaries, logs,
local forwarding sessions, and local temporary material were removed.

This is evidence that the run-v2 receiver remains convergent and bounded under
this controlled concurrent two-host transport path. It is not evidence for
durable receipt, replay/catch-up, authorization, TLS termination, WAN fault
tolerance, or provider/storage capacity. Those require a separately
manifest-bound and persistence-backed test harness.
