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
| Build and merge 64 nested cards | 63,392 update bytes | 26.239, 26.387, 24.847, 25.774, 24.561 | 25.774 |
| Snapshot and restore 64 nested cards | 44,747 state bytes | 19.224, 19.243, 19.096, 19.137, 18.938 | 19.137 |
| Three editors, shuffled duplicate delivery | 96 divergent edits; 89,426 total bytes | 120.040, 120.108, 119.492, 119.651, 119.665 | 119.665 |

The first two workloads run three iterations per sample; the simulation runs
four. Heap deltas vary with V8 garbage collection and are not a capacity
measurement. The simulator exercises independent nested maps/arrays, reverse
delivery, periodic duplicate frames, and final JSON convergence.

The local transaction path now does incremental exact-envelope byte accounting;
it keeps the existing pre-mutation 1 MiB and operation-count checks while
avoiding a full canonical re-encode of all preceding operations for every
member of a batch. Large recovery or a much larger nested tree still belongs in
a Worker and behind product-specific limits.
