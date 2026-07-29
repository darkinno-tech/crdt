# Durable relay benchmark — 2026-07-29

This is controlled local evidence for the `durable` reference, not a service
level objective or a production-capacity estimate.

## Environment

| Item | Value |
| --- | --- |
| Date | 2026-07-29 |
| Host | Apple M4 Pro, darwin/arm64 |
| Go | go1.26.5 |
| Store | bbolt v1.3.10, default synchronous transaction settings |
| WebSocket | github.com/coder/websocket v1.8.13 |
| Command | `go test -run='^$' -bench='Benchmark(DurableAppend|DurableReplay|ReconnectHandshakeLoopback)$' -benchtime=1s -benchmem ./durable` |

## Results

| Benchmark | Workload | Result | Allocations |
| --- | --- | ---: | ---: |
| `BenchmarkDurableAppend` | One canonical G-Counter delta, bbolt append transaction and sync | 8,064,225 ns/op | 57,277 B/op; 133 allocs/op |
| `BenchmarkDurableReplay` | Replay 256 persisted canonical events from local store | 38,759 ns/op | 99,856 B/op; 1,052 allocs/op |
| `BenchmarkReconnectHandshakeLoopback` | Real local WebSocket hello/resume handshake at high-water | 162,732 ns/op | 43,270 B/op; 334 allocs/op |

The replay benchmark constructs owned `replica.Change` values for all 256
events; its allocation count is intentionally inclusive of safe byte ownership
and manifest validation, not a zero-copy storage claim. The append benchmark
includes the reference's one-event-per-transaction durability choice.

## Interpretation and limits

- The bbolt reference has one writer. These numbers are not evidence that
  additional processes can safely share one file or that write throughput
  scales horizontally.
- The run excludes TLS, an ingress/WAF, token verification, a real network,
  concrete CRDT application, client checkpoint transactions, disk contention,
  backup traffic, and multi-tenant quotas.
- Retention, replay, queue, and input limits remain hard ceilings. Increase a
  ceiling only after reproducing the complete workload plus fault tests on the
  intended storage and deployment platform.
- A production rollout still needs restore drills, storage latency/error
  metrics, queue/replay rejection metrics, and an authenticated checkpoint
  bootstrap path for clients outside the replay window.
