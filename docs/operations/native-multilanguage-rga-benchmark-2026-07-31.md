# Native multilingual RGA controlled benchmark — 2026-07-31

## Scope

This is a controlled development regression baseline for the new Rust
`rga-run-v2` core. It is not a production capacity claim. The operation makes
one 1,536-Unicode-scalar local insert, applies its canonical frame to another
native replica, encodes a complete state, and restores that state into a third
replica. It exercises local mutation, bounded decode/merge, canonical state
encoding, and recovery in one native-runtime path.

## Environment

| Item | Value |
| --- | --- |
| Candidate base | `7b89e52` plus uncommitted multilingual client work |
| Host | macOS 26.5.2, arm64 |
| Rust | `rustc 1.97.1`, Cargo `1.97.1` |
| Command | `cargo bench --manifest-path clients/rust/Cargo.toml --bench rga` |
| Inner samples | 8 operations per process; `Instant` wall time |

## Results

The first process after recompilation measured `50.316 ms/op`, which includes
cold process/cache effects and is not used as the steady-state number. Three
warm process results were `6.927 ms/op`, `7.258 ms/op`, and `6.981 ms/op`;
the median is **6.981 ms/op**.

The measurement includes correctness-preserving copy-on-validate state
admission. It intentionally does not isolate transport, FFI crossing, battery,
large tombstone, contention, or an actual mobile device. Before setting a
product limit, rerun with the production CPU/memory, encrypted persistence,
real editor batch distribution, expected document/tombstone size, and actual
concurrent replicas.

## Interpretation

This baseline is sufficient to catch accidental quadratic pending-chain
integration. The original first draft rescanned all pending nodes after each
newly integrated parent; the final parent-index queue visits the chain once.
The implementation still clones retained maps before non-duplicate admission
to preserve atomic rejection, so a future copy-on-write optimization requires
evidence from a retained-state scaling benchmark and must retain the same
invalid-frame atomicity tests.
