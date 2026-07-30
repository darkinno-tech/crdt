# Native TypeScript shared-type benchmark — 2026-07-30

## Scope and environment

This is a controlled development measurement of the dependency-free
`native-ts-v1` implementation. It is not a browser/mobile SLA or a comparison
against Go RGA/Wasm: the workloads include document construction, copying,
canonical JSON validation/encoding, and convergence assertions.

| Field | Value |
| --- | --- |
| Commit basis | native-ts-v1 worktree before beta publication |
| OS / architecture | Darwin 25.5.0 / arm64 |
| Node | v26.5.0 |
| npm | 11.17.0 |
| Command | `make typescript-native-benchmark` (`npm run bench:native`) |
| Sampling | 2 warmups, 5 samples per workload; heap delta is process heap before/after, not retained memory |

## Workloads and results

| Workload | What it verifies | Bytes | Samples (ms/op) | Median (ms/op) |
| --- | --- | ---: | --- | ---: |
| cold append + encoded merge, 4,096 values | one batch RGA insert, canonical encode/decode, remote merge | 475,977 | 99.537, 121.435, 117.047, 115.545, 96.614 | 115.545 |
| cold middle insert + encoded merge, 4,096 base values | base synchronization, index projection, middle insert, remote merge | 219 delta bytes | 73.372, 81.280, 66.981, 68.099, 65.799 | 68.099 |
| shuffled duplicate three-editor session | 192 offline edits/deletes, reverse delivery, periodic duplicate delivery, convergence | 33,588 total bytes | 17.949, 17.864, 17.982, 18.262, 17.530 | 17.949 |
| cold state encode + restore, 4,096 object values | state chunking, canonical updates, same-actor counter recovery | 519,109 | 177.348, 179.275, 178.869, 177.319, 177.284 | 177.348 |

The `cold` labels intentionally include fixture creation and validation, so they
represent an end-to-end local operation rather than an isolated inner-loop
microbenchmark. The state workload has the highest cost because it validates
and copies the source, serializes the complete state, restores it, and projects
the recovered array.

## Interpretation

- The protocol stays below the default 1 MiB message limit at 4,096 entries;
  `encodeStateAsUpdates()` incrementally splits larger state rather than
  repeatedly serializing a growing prefix.
- 4,096-item operations are well above a browser frame budget. A UI should
  batch input, transmit deltas, and run large local merges/state recovery in a
  Worker. This is an architectural boundary, not a claim that the main thread
  can render these workloads at 60 fps.
- Heap deltas varied with V8 garbage collection and are intentionally not used
  as a capacity estimate. The protocol's explicit retained-node/value limits
  remain the resource-safety control.

## Reproduce

```sh
make typescript-test
make typescript-native-benchmark
```

For cross-language RGA verification, run `make wasm-test` separately. That
tests the unchanged Go/Wasm run-v2 contract; it does not make native-ts-v1
updates Go-frame compatible.
