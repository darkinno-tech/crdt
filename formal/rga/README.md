# RGA formal model

This Lean 4 project is the first formal-verification boundary for darkinno-tech
RGA. Its toolchain is pinned in `lean-toolchain`; it has no third-party theorem
libraries, so a proof check is reproducible with only `elan`/`lake`.

```sh
make formal-rga
```

## What is proven now

`RGAFormal/Delta.lean` models a delta as extensional sets of retained position
IDs and structural tombstones. Lean checks:

- delta application is idempotent;
- two delta deliveries commute and batch grouping is associative;
- tombstones are monotone before explicit compaction;
- a delete delivered before its insertion cannot become visible after that
  insertion arrives;
- duplicate/reordered two-delta delivery converges.

The next three models make the roadmap's highest-value adjacent invariants
machine-checkable while preserving a deliberately small abstraction boundary:

- `RGAFormal/Order.lean` proves that the descending sibling comparator is a
  strict total order for distinct validated position ranks;
- `RGAFormal/RunV2.lean` refines the negotiated run-v2 envelope: canonical
  state/delta type tags, empty codec ID, version and checksum admission, plus
  encoder/decoder round-trip at the parsed-envelope boundary;
- `RGAFormal/Recovery.lean` models a live/durable state machine and proves
  that crash recovery selects exactly one atomic `{state, HLC, frontier}`
  record rather than a mixed partial write.

Run every model with the pinned toolchain:

```sh
make formal-rga
```

These propositions correspond to the safety invariant exercised in
`text/rga.go`: structural tombstones are not ordinary values and cannot be
discarded merely because a node has not arrived.

## Deliberate boundary

This does **not** prove the Go code, byte parser, CRC-32C implementation,
full HLC implementation, filesystem/provider atomicity, resource limits,
authentication, or tombstone compaction correctness. It also does not claim
Yjs wire compatibility. `Order.lean` uses validated natural-number ranks in
place of Go's concrete `Tag.Compare`; `RunV2.lean` starts after byte/varint
parsing; and `Recovery.lean` assumes an atomic durable-record primitive.

The production confidence chain remains: bounded parser tests/fuzzing,
three-editor duplicate/reorder/recovery simulations, `-race`, and benchmarked
resource caps. The Lean model is an additional proof layer, not a replacement
for those gates.

## Next proof obligations

1. Model pending-parent integration and show a complete parent-before-child
   closure has the same result as arbitrary duplicate/reordered delivery.
2. Connect the concrete Go `Tag.Compare`, byte/varint parser, CRC-32C, and
   published vectors to the abstract sibling-order and envelope models.
3. Model an epoch/acknowledgement compaction boundary; prove old deltas cannot
   be accepted after structural anchors are retired.
4. Connect provider durability and concrete HLC overflow/physical-clock paths
   to the recovery state machine. Do not describe the implementation as
   formally verified until these refinement relations and their toolchain gate
   are reviewed.
