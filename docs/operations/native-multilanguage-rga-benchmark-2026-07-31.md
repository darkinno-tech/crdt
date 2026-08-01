# Native multilingual RGA controlled benchmark — 2026-07-31

## Scope

This is a controlled development regression baseline for the new Rust
`rga-run-v2` core. It is not a production capacity claim. The original
operation made one 1,408-Unicode-scalar local insert, applied its canonical
frame to another native replica, encoded a complete state, and restored that state into a third
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

## State-encoding allocation follow-up

The 2026-07-31 follow-up changed only the complete-state encoder and canonical
block builder. `Rga::encode_state` now borrows its complete node/tombstone sets
instead of first cloning them into a `Delta`; canonical block construction keeps
references to the same retained positions and nodes instead of copying each
position, replica ID, and node. Delta admission remains copy-on-validate, so a
rejected remote frame retains the same all-or-nothing merge behavior.

The original text label was inaccurate: `"rga-run-v2 "` is 11 Unicode scalars,
so its 128 repetitions measured 1,408 scalars, not 1,536. The follow-up appends
128 `x` scalars and asserts the exact 1,536-scalar workload in both Rust and
C++. The pre-optimization baseline was rerun from `eccdf78` with only that
workload/measurement correction; the candidate uses the same harness. On the
same Darwin arm64 host and Cargo 1.97.1, each workload ran eight inner
operations per process and three warm processes before and after the change.

| Workload | Before samples | After samples | Median change |
| --- | --- | --- | --- |
| Complete 1,536-scalar state encode | 1.042, 1.071, 1.096 ms/op | 0.795, 0.771, 0.787 ms/op | 1.071 → 0.787 ms/op (**-26.5%**) |
| Insert → relay → state → recovery | 7.663, 7.768, 7.876 ms/op | 6.617, 6.746, 6.702 ms/op | 7.768 → 6.702 ms/op (**-13.7%**) |

The C++20 facade exercised the same exact workload through the C ABI. Its
three-sample median changed from 7.911 ms/op (7.664, 7.990, 7.911) to
7.159 ms/op (7.064, 7.287, 7.159), or **-9.5%**. This is still a controlled
development signal, not a C++ allocator, package-distribution, or device SLA.

`make rust-test` retained byte-for-byte Go-vector round trips and added the
rejected-local-edit HLC atomicity check. `make python-test`, `make swift-test`,
and `make cpp-test` exercised the changed Rust core through all checked-in FFI
bindings. These results do not establish production memory, battery, allocator,
storage, or multi-thread throughput limits.
