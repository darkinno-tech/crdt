# Controlled benchmark evidence — 2026-07-29

These are reproducible development measurements, not a production latency,
throughput, or capacity promise. Host addresses, credentials, tokens, and
application data are deliberately omitted.

## Method

- RGA and `replica.Inbox`: detached commit `152838ea30d1b65cef87b248b21ddbf0b1714550`.
- WebSocket provider: SHA-256 `8ff2686f3bc431564027c342214220409da32370f433bab4348359a8a13efe59`; it adds only the test harness in `examples/websocket-provider/provider/provider_benchmark_test.go`.
- Two Debian 13 `linux/amd64` hosts: four Intel Xeon Platinum 8272CL vCPUs and 3.8 GiB memory. Test binaries were locally cross-compiled with Go 1.26.5, uploaded to mode-0700 temporary directories, then SHA-256 verified.
- Each remote cell is the arithmetic mean of three `-benchtime=2s` samples at the stated `GOMAXPROCS`.

The RGA fixture is a 100,000-rune linear document. `snapshot` computes a delta
from raw snapshot bytes; `cached_base` starts with a prevalidated
`SnapshotBase`.

| Workload | Host A, G=1 | Host A, G=4 | Host B, G=1 | Host B, G=4 | Allocation/op |
| --- | ---: | ---: | ---: | ---: | --- |
| RGA state v1 marshal | 125.2 ms | 100.0 ms | 123.6 ms | 101.7 ms | 16,061,696 B; 263 allocs |
| RGA state run-v2 marshal | 128.4 ms | 98.6 ms | 125.2 ms | 100.3 ms | 21,386,544 B; 266 allocs |
| RGA delta from raw snapshot | 37.9 ms | 29.2 ms | 37.6 ms | 29.6 ms | about 11.8 MiB; 356-371 allocs |
| RGA delta from cached base | 3.44 ms | 3.40 ms | 3.48 ms | 3.43 ms | 110,296 B; 25 allocs |
| Inbox installed duplicate | 246.3 ns | 249.5 ns | 249.5 ns | 249.6 ns | 8 B; 1 alloc |

The RGA and Inbox loops are serial, so `GOMAXPROCS=4` is a runtime-setting
sample, not four-core aggregate throughput. In this fixture the cached-base
path is about eleven times faster than rescanning raw snapshot bytes; it does
not relax validation or compatibility checks.

## WebSocket reference provider

The `Group` duplicate benchmark uses `RunParallel`, measuring contention on one
bounded admission lock. The fan-out benchmark has one publisher and waits until
every loopback observer decodes and installs the change. The table combines the
two hosts (six samples per cell).

| Workload | G=1 | G=4 | Allocation/op |
| --- | ---: | ---: | --- |
| Installed duplicate admission, parallel | 745.4 ns | 783.0 ns | 224 B; 7 allocs |
| End-to-end relay, 1 observer | 52.7 µs | 70.0 µs | 5,607 B; 77-78 allocs |
| End-to-end relay, 4 observers | 102.1 µs | 96.7 µs | 11,007 B; 150-151 allocs |
| End-to-end relay, 16 observers | 311.0 µs | 182.9 µs | 32,505 B; 439 allocs |

No public port was opened: the provider handler and clients used compression-
disabled `httptest` loopback. These numbers exclude WAN, TLS, external
authentication, durable outbox/storage, reconnect, snapshot, anti-entropy, and
browser costs.

## Local Node and Go-Wasm sample

Local environment: Apple M4 Pro, 12 logical CPUs, 24 GiB memory, Go 1.26.5,
and Node v26.5.0. Each JavaScript result has five samples.

| Workload | Result |
| --- | --- |
| TypeScript decode, 512 KiB frame | 492.06-508.43 MiB/s; mean 502.59 MiB/s |
| Node-to-Go-Wasm one-rune insert + apply, 38 B frame | 0.090-0.131 ms; mean 0.109 ms |
| Node-to-Go-Wasm 4,096-rune insert + apply, 187,900 B frame | 60.707-62.125 ms; mean 61.523 ms |

Local Go RGA samples are intentionally not reported: another fuzz campaign
began during collection and materially increased variance. Re-run on an idle
machine before comparing local Go data to either Linux host.

## Reproduction

```sh
go test -count=1 ./text ./replica ./examples/websocket-provider/provider
go test -race -count=1 ./examples/websocket-provider/provider

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o text.test ./text
GOMAXPROCS=4 ./text.test -test.run=NO_SUCH_TEST \
  -test.bench='BenchmarkRGAMarshal' -test.benchmem -test.benchtime=2s -test.count=3

make typescript-benchmark
make wasm-benchmark
```
