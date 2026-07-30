# RGA formal model

This Lean 4 project is the first formal-verification boundary for DarkInno
RGA. Its toolchain is pinned in `lean-toolchain`; it has no third-party theorem
libraries, so a proof check is reproducible with only `elan`/`lake`.

```sh
cd formal/rga
lake env lean RGAFormal/Delta.lean
```

## What is proven now

`RGAFormal/Delta.lean` models a delta as finite sets of retained position IDs
and structural tombstones. Lean checks:

- delta application is idempotent;
- two delta deliveries commute and batch grouping is associative;
- tombstones are monotone before explicit compaction;
- a delete delivered before its insertion cannot become visible after that
  insertion arrives;
- duplicate/reordered two-delta delivery converges.

These propositions correspond to the safety invariant exercised in
`text/rga.go`: structural tombstones are not ordinary values and cannot be
discarded merely because a node has not arrived.

## Deliberate boundary

This does **not** prove the Go code, protocol decoder, HLC implementation,
run-v2 sibling ordering, resource limits, snapshot recovery, authentication,
or tombstone compaction correctness. It also does not claim Yjs wire
compatibility. The model uses finite sets and therefore abstracts away the
ordered parent graph that determines RGA visible order.

The production confidence chain remains: bounded parser tests/fuzzing,
three-editor duplicate/reorder/recovery simulations, `-race`, and benchmarked
resource caps. The Lean model is an additional proof layer, not a replacement
for those gates.

## Next proof obligations

1. Add a parent graph and prove the exact descending sibling-order projection
   is deterministic for the current `text.childIndex` rule.
2. Model pending-parent integration and show a complete parent-before-child
   closure has the same result as arbitrary duplicate/reordered delivery.
3. Model an epoch/acknowledgement compaction boundary; prove old deltas cannot
   be accepted after structural anchors are retired.
4. Connect executable test vectors and a small extracted reference model to
   Go run-v2 frames. Do not describe the implementation as formally verified
   until this refinement relation and its toolchain gate are reviewed.
