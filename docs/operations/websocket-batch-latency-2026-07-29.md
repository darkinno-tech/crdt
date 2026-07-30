# WebSocket batch latency evidence — 2026-07-29

This note records a local, loopback-only comparison of the reference WebSocket
provider's v1 single-change transport and the new opt-in v2 batch transport.
It is not a WAN, browser, TLS, storage, or production-capacity guarantee.

## Change

`crdt-sync-v2` carries a bounded list of canonical v1 change envelopes. Every
item keeps its own `replica.Dot` and delta. The server validates and admits
each item individually, sends one v2 frame to v2 peers, and falls back to
individual v1 frames for v1 peers. `crdt-sync-v1` remains the client default.
An upgraded client that enables batches offers v2 first and v1 second, so it
can connect to a legacy v1-only server; `PublishBatch` then returns
`ErrBatchUnsupported` instead of silently changing its delivery contract.

`Client.PublishBatch` is explicit: application code chooses a short batch and
keeps every original change in its durable outbox until its own delivery rule is
satisfied. The example relay is intentionally not a durable outbox or ACK
authority.

## Method

Apple M4 Pro, macOS/arm64, Go 1.26.5. Three one-second samples per case:

```sh
go test ./examples/websocket-provider/provider -run '^$' \
  -bench '^BenchmarkProviderEndToEndRelay(Batch)?Fanout$' -benchmem -count=3
```

One v2 operation contains eight independently identified G-Counter changes.
Each result waits until every loopback observer decodes and installs every Dot.
The table compares median v1 time per logical change with median v2 time
divided by eight.

| Observers | v1 / logical change | v2 / batch | v2 / logical change | Time reduction | Allocation / logical change |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1 | 47.8 us | 65.5 us | 8.2 us | 5.8x | 5,739 B -> 3,649 B |
| 4 | 74.0 us | 111.0 us | 13.9 us | 5.3x | 11,139 B -> 6,451 B |
| 16 | 128.5 us | 182.4 us | 22.8 us | 5.6x | 32,648 B -> 17,647 B |

The reduction comes from amortizing WebSocket framing, handler admission, and
fan-out scheduling. It cannot lower a real network round trip below the
physical RTT; use an in-region relay plus a bounded client coalescing window to
keep the end-to-end tail within an application latency target.

## Correctness checks

- v2 wire round-trip and count bounds;
- one v2 publisher reaches both a v2 observer and a v1 fallback observer;
- every batch Dot is observed and installed exactly once per observer;
- v1 `PublishBatch` rejection and configured batch-limit rejection;
- race test for all packages.

`cmd/crdt-cluster-sim` was also changed so `/rga` returns a zero-body `204`
with `X-CRDT-Apply-Micros`; full state serialization is reserved for the final
`/state` convergence check. Its server accepts only loopback listeners, so a
cross-host run must use an authenticated tunnel or an application-owned relay
rather than exposing this test endpoint publicly.
