# Yjs core compatibility layer — controlled benchmark, 2026-08-02

## Scope

This is a local development baseline for the TypeScript Yjs compatibility
layer. It measures only in-process work after a native Yjs document is already
available. It does **not** establish browser paint latency, y-websocket or TLS
throughput, authorization cost, YJSStore durability, WAN reconnection, or
production receiver capacity.

## Command and workload

```sh
make typescript-yjs-core-benchmark
```

- Host: Apple M4 Pro, macOS/darwin arm64
- Runtime: Node `v26.5.0`
- Yjs: `13.6.31`, V1 update format for the y-protocols sync scenario
- Five samples, 16,384 UTF-16 initial text units, 256 measured rounds
- `state_vector_sync`: one source appends one character per round; the target
  sends a standard SyncStep1 and receives the bounded SyncStep2 difference.
- `deep_observer`: one nested `Y.Map` key update per transaction through the
  bounded path/target observer.
- `undo_redo`: 256 explicitly separated local one-character replacements,
  followed by 256 undo and 256 redo operations. This is exactly the default
  local-history cap; the focused suite separately proves the reset on the next
  replacement.

## Observed output

| Scenario | Median elapsed | Median unit cost | Other result |
| --- | ---: | ---: | --- |
| `state_vector_sync` | 3.83 ms / 256 rounds | 0.015 ms / round | 24.0 average SyncStep2 bytes |
| `deep_observer` | 0.53 ms / 256 events | 0.002 ms / event | 256 bounded callbacks delivered |
| `undo_redo` | 5.07 ms / 768 operations | 0.007 ms / operation | final text restored after 256 undos + 256 redos |

The warm samples are faster than the first sample, so these medians are only a
repeatable local regression signal. In particular, the 24-byte diff is a
single-writer, one-character update result; it must not be projected onto
rich-text documents, concurrent writers, retained delete sets, or a real
WebSocket envelope.

The existing `make typescript-yjs-bindings-benchmark` was also rerun. Its
incremental path issued 512 range writes and no full-text writes for 512 remote
changes; its plain Node full-string baseline remained faster, which is a write
shape result rather than a browser-editor throughput claim.

Before setting a deployment limit, run the same scenarios with the actual
editor schema, YJSStore/relay limits, authentication, target browser/device,
slow receivers, 1/4/16/64 users, reconnect/recovery, and the selected durable
receipt boundary.
