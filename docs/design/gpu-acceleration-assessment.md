# GPU acceleration assessment

## Decision

Do not add a GPU dependency or route online CRDT operations through a GPU at
this time. Keep the protocol, validation, merge, persistence, and anti-entropy
paths CPU-owned. The justified improvement is CPU-side batching of an already
validated linear RGA run; it preserves the existing canonical order and frame
bytes without adding a device/runtime boundary.

This is a no-go for the current online path, not a permanent ban on GPU work.
An opt-in offline batch verifier or a large, device-resident Merkle workload
may be reconsidered only after it meets the evidence gates below.

## Evidence from the current implementation

`RGA.ApplyDelta` mutates one lock-protected, pointer-rich sequence index and
maintains deterministic sibling order. The common large-paste path is a
parent-before-child chain, but every applied node must still update retained
positions, tombstones, HLC witnessing, and pending-replay state. This is
latency-sensitive, branch-heavy work, not an independent arithmetic kernel.

The pre-change 100,000-node linear-delta profile on an Apple M4 Pro with Go
1.26.5 reported approximately 103 ms/op, 57.6 MB/op, and 101k allocations/op.
The largest sampled CPU costs were `runtime.madvise` (18%), sequence-marker
refresh/merge (23% cumulative), map updates, and GC-related work. SHA-256 and
Merkle calculation were not the observed RGA merge bottleneck.

The new batch path builds the known depth-first marker order once and splices
its Cartesian treap in one operation. It only runs after the existing strict
same-replica, complete, parent-resolved validation. A three-run local sample
after the change ranged from roughly 77 to 89 ms/op under the same benchmark
shape, but a later thermally variable sample reached 149 ms/op. These are
development observations, not a production-capacity claim; the CI benchmark
gate must compare repeated fixed-host CPU controls before accepting a numeric
regression threshold.

## Multi-dimensional assessment

| Dimension | Finding | Decision impact |
| --- | --- | --- |
| Correctness | Online merge order, tombstones, HLC state, and canonical frames are part of the CRDT contract. A parallel device result would still need deterministic CPU validation before mutation. | Do not move authoritative merge decisions off CPU. |
| Performance | The measured hot work is pointer/index maintenance and allocation. Per-edit GPU dispatch and data marshaling would add latency; batching CPU index construction directly targets the hotspot. | Use the CPU batch path first. |
| Security | Device drivers, shader/runtime bindings, buffer ownership, and fallback behavior create a new trusted-computing and denial-of-service boundary. Peer bytes must never select kernels, sizes, or unbounded buffers. | No GPU dependency in the untrusted transport path. |
| Portability and operations | The library supports ordinary Go servers, browser/Wasm clients, and heterogeneous provider deployments. CUDA would exclude many targets; Metal/WebGPU would require distinct implementations and conformance gates. | Avoid a mandatory accelerator abstraction or new runtime dependency. |
| Cost and maintenance | A GPU implementation needs device detection, queue backpressure, memory budgets, observability, reproducible CPU fallbacks, and cross-vendor golden tests. | The expected current benefit does not pay for the maintenance cost. |

GPU guidance from NVIDIA and Apple aligns with this result: GPUs pay off for
large data-parallel work kept resident on the device; host/device transfer and
per-command overhead must be minimized. The online CRDT path repeatedly needs
authoritative CPU state and carries small or irregular updates, the opposite
shape.

## Future admission gates

Create an experimental, non-authoritative accelerator only when all are true:

1. A production trace demonstrates a batchable workload with at least tens of
   thousands of independent records and an end-to-end CPU bottleneck dominated
   by digesting or verification, not transport, locking, or index mutation.
2. The input is bounded before device allocation, has a fixed binary layout,
   and can remain device-resident across several kernels. Device output is
   checked against CPU golden vectors before it can influence a decision.
3. CPU fallback produces byte-identical output; accelerator unavailability,
   timeout, queue saturation, and malformed input fail closed without
   weakening protocol/resource limits.
4. The benchmark measures wall-clock CPU fallback, marshaling, dispatch,
   synchronization, and device work together. It must compare repeated
   realistic replication traces and synthetic large batches on each supported
   platform.
5. The experiment remains build-tagged and opt-in until race, fuzz, vector,
   deterministic replay, and security/resource-limit tests pass on both paths.

## Validation retained for the CPU batch path

- Unit tests compare batched sequence ordering against the existing generic
  builder with pre-existing siblings, a tombstone in the batch, and a later
  concurrent sibling.
- Existing text-package tests and `go test -race ./text` protect concurrent
  reads, compaction, recovery, duplicate delivery, and protocol framing.
- The three-replica unreliable-network scenario and collaboration-simulation
  smoke test remain required because a fast local batch is not evidence of
  distributed convergence on its own.
