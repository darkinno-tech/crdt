# Yjs relay controlled validation — 2026-07-31

This is a development baseline for the bounded `extensions.YJSHandler`, not a
WAN, browser, TLS, durable-store, or production-capacity result.

## Contract exercised

The relay implements the stable `y-websocket` wrapper around
[`y-protocols`](https://docs.yjs.dev/ecosystem/other/y-protocols): sync Step 1,
sync Step 2/update, awareness, and awareness query. It relays opaque Yjs v1
updates and never translates them into this repository's RGA/run-v2 protocol.

The controlled Node loopback check used:

| Component | Version / setup |
| --- | --- |
| Node | v26.5.0 |
| Yjs | `13.6.31` |
| y-websocket | `2.1.0` |
| Relay | `httptest` loopback, one configured `notes` room |
| Scenario | Two provider-backed `Y.Doc`s initial-sync, insert `hello Yjs` into `Y.Text("shared")`, then publish a local awareness field. |

Both providers reported initial sync. The second document rendered `hello Yjs`,
and its awareness map observed the first provider's local user field. This is
real client/provider interoperability evidence, but it does not verify an
Internet deployment, browser origin policy, TLS, auth implementation,
persistence, reconnect durability, or arbitrary document shapes.

## Simulated transport and adversarial evidence

The committed Go suite adds a deterministic eight-writer concurrent fan-out
simulation, duplicate history admission, malformed/non-canonical varuint
rejection, awareness ownership conflict rejection, awareness cap enforcement,
and disconnect cleanup. It passed:

```text
go test ./extensions                         PASS
go test -race ./extensions -run 'TestYJS'    PASS
go test -run='^$' -fuzz=FuzzUnmarshalYJSMessages -fuzztime=10s -parallel=1 ./extensions
                                                PASS; 124,088 executions
```

Fuzzing exercises only the bounded outer y-protocols wrapper. It cannot prove
the semantics of an opaque Yjs update; that validation requires a Yjs-aware
document runtime at the persistence/application boundary.

## Microbenchmark

```sh
go test -run '^$' -bench='BenchmarkYJSWireDecodeAndAdmission$' -benchmem -benchtime=1s -count=3 ./extensions
```

Host: Apple M4 Pro, macOS/darwin arm64, Go toolchain selected by this checkout.
The benchmark parses one real Yjs v1 `Y.Text` update and admits it into an
in-memory duplicate-aware room; it has no sockets, TLS, JSON-aware Yjs decoder,
or peer write.

| Run | ns/op | B/op | allocs/op |
| ---: | ---: | ---: | ---: |
| 1 | 108.3 | 136 | 3 |
| 2 | 110.3 | 136 | 3 |
| 3 | 108.5 | 136 | 3 |
| Median | 108.5 | 136 | 3 |

Use it as a regression signal only. Before setting production limits, benchmark
end-to-end p50/p95/p99 under expected update sizes, concurrent rooms, slow
receivers, TLS termination, authorization, persistence flushes, reconnects,
and adversarial messages.
