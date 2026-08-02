# Native Yjs Quill Delta binding baseline — 2026-08-03

## Scope

This is a controlled in-process regression benchmark for the new native
`Y.Text` rich-text binding. It compares applying an incremental Yjs Delta to a
Quill-shaped Delta port with rebuilding that port from `Y.Text.toDelta()` after
every remote change. It is not an actual Quill browser paint, provider, TLS,
authorization, YJSStore, WAN, persistence, or multi-user capacity result.

## Command and workload

```sh
make typescript-yjs-richtext-benchmark
```

- Host: Apple M4 Pro, macOS/darwin arm64
- Runtime: Node `v26.5.0`
- Yjs: `13.6.31`, V1 updates
- Initial `Y.Text`: 16,385 UTF-16 units (16 KiB text plus terminal newline)
- Five samples per scenario; 512 remote single-character `bold` format edits
- `incremental_ytext_delta`: `YjsRichTextBinding` validates each native
  `Y.TextEvent.delta` and calls the port's `updateContents` once.
- `full_delta_projection_baseline`: reads complete `Y.Text.toDelta()` and calls
  the port's replacement operation once after every update.

## Observed output

| Scenario | Median elapsed | Median per remote merge | Full writes | Incremental writes |
| --- | ---: | ---: | ---: | ---: |
| `incremental_ytext_delta` | 52.72 ms / 512 | 0.103 ms | 0 | 512 |
| `full_delta_projection_baseline` | 312.11 ms / 512 | 0.610 ms | 512 | 0 |

The sample verifies convergence of author `Y.Text`, replica `Y.Text`, and the
Delta port after every scenario. Heap deltas varied with V8 GC and are not
reported as a capacity result. The time ratio is only a signal for this simple
in-memory Delta model; profile a real Quill 2 build on target browsers with the
selected provider, live cursor rendering, content schema, images, slow peers,
offline recovery, and 1/4/16/64 receiver cases before setting SLOs.
