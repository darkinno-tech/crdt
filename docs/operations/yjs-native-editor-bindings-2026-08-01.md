# Yjs native incremental editor binding baseline — 2026-08-01

## Scope

This controlled local comparison answers one narrow question: after a native
Yjs update has reached the browser, does the binding issue an exact text range
write or request a whole-text editor projection?

It does **not** measure network, TLS, y-websocket, server authorization,
YJSStore durability, real browser layout/paint, or multi-user service capacity.

## Command and workload

```sh
make typescript-yjs-bindings-benchmark
```

- Host: Apple M4 Pro, macOS
- Runtime: Node `v26.5.0`
- Yjs: `13.6.31`, V1 updates
- Initial `Y.Text`: 49,152 UTF-16 code units
- Workload: 512 sequential single-code-unit remote replacements
- Samples: five per scenario
- `incremental_ytext_delta`: `YjsTextBinding` consumes `Y.TextEvent.delta`
  and applies the range to a small deterministic text-port model.
- `full_text_projection_baseline`: the same model calls `targetText.toString()`
  and replaces its complete text after every remote update.

## Observed output

| Scenario | Median elapsed | Median per remote merge | Full writes | Incremental writes |
| --- | ---: | ---: | ---: | ---: |
| `incremental_ytext_delta` | 11.82 ms | 0.023 ms | 0 | 512 |
| `full_text_projection_baseline` | 6.24 ms | 0.012 ms | 512 | 0 |

The full-projection baseline is faster in this synthetic Node text-port model.
That result is expected to be non-representative: assigning a JavaScript string
does not model a real editor's document rebuild, selection mapping, extension
work, or paint. The result therefore establishes **write shape**, not a
throughput win: the native path performed no full writes, while the baseline
performed 512. Heap deltas varied with V8 GC and are diagnostics only.

Use browser-device traces with the target CodeMirror extensions, actual Yjs
provider, representative documents, concurrent cursors, and slow-peer behavior
before selecting product SLOs or operational limits.
