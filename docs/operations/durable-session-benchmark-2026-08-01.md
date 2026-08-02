# Durable state-vector session benchmark — 2026-08-01

This report records controlled same-host evidence for the durable v2 session
work. It is not a WAN, TLS, identity-provider, storage-fsync, browser/mobile,
or production-capacity result.

## Workload

- Host: Apple M4 Pro, darwin/arm64
- Go: go1.26.5 darwin/arm64
- Transport: real `httptest` HTTP loopback and `github.com/coder/websocket`
- Storage: one local bbolt `durable.Store`, synchronous append per operation
- Sender: one WebSocket publisher, sequential G-Counter changes
- Receivers: 1, 4, or 16 `crdt-durable-v2` peers; each decodes the event and
  installs it through `replica.Inbox` before the benchmark advances
- Catch-up: each receiver completes an empty state-vector catch-up before the
  timed live fan-out phase

Command:

```sh
go test -run='^$' -bench='BenchmarkDurableSameHostFanout' \
  -benchtime=1s -count=3 -benchmem -cpu=1 ./durable
```

## Results

| Receivers | Median ns/op | Approx. ops/s | Median B/op | Median allocs/op |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 13,304,593 | 75.2 | 71,833 | 256 |
| 4 | 13,984,915 | 71.5 | 76,764 | 331 |
| 16 | 14,907,874 | 67.1 | 98,025 | 635 |

The three samples for each row were:

| Receivers | ns/op samples |
| ---: | --- |
| 1 | 13,304,593; 12,361,044; 13,889,815 |
| 4 | 13,505,747; 13,984,915; 14,139,029 |
| 16 | 14,907,874; 15,217,768; 14,808,706 |

## Interpretation

The 16-receiver case adds about 12% median end-to-end operation latency over
one receiver on this host, while every receiver performs a bounded wire decode
and Inbox installation. Its additional allocations are expected: each
receiver owns a decoded event and CRDT delta boundary rather than sharing
mutable payloads.

Do not use these figures to set a production concurrency limit. Before doing
so, repeat the test against the target filesystem or Redis/PostgreSQL log,
TLS termination, identity checks, expected document sizes, client checkpoint
latency, packet loss/partition behavior, and tenant quota settings. Track
p50/p95/p99 rather than this small local median alone.
