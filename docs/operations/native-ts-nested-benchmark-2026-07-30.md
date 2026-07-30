# Native TypeScript nested benchmark — 2026-07-30

## Environment and method

Controlled local measurements of `native-ts-nested-v1`; not a browser/mobile
SLA or a comparison with Yjs or Go RGA/Wasm.

| Field | Value |
| --- | --- |
| OS / architecture | Darwin 25.5.0 / arm64 |
| Node | v26.5.0 |
| Go | go1.26.5 darwin/arm64 |
| Command | `npm --prefix clients/typescript run bench:nested` |
| Sampling | 2 warmups, 5 samples; output includes construction, validation, copying, and convergence assertions |

## Results

| Workload | Payload | Samples (ms/op) | Median (ms/op) |
| --- | ---: | --- | ---: |
| Build and merge 64 nested cards | 63,392 update bytes | 26.618, 26.152, 24.606, 25.792, 25.616 | 25.792 |
| Snapshot and restore 64 nested cards | 44,747 state bytes | 19.707, 19.883, 20.347, 19.873, 19.583 | 19.873 |
| Three editors, shuffled duplicate delivery | 96 divergent edits; 89,426 total bytes | 122.200, 122.061, 122.282, 121.262, 122.554 | 122.200 |

The first two workloads run three iterations per sample; the simulation runs
four. Heap deltas vary with V8 garbage collection and are not a capacity
measurement. The simulator exercises independent nested maps/arrays, reverse
delivery, periodic duplicate frames, and final JSON convergence.

The local transaction path now does incremental exact-envelope byte accounting;
it keeps the existing pre-mutation 1 MiB and operation-count checks while
avoiding a full canonical re-encode of all preceding operations for every
member of a batch. Large recovery or a much larger nested tree still belongs in
a Worker and behind product-specific limits.
