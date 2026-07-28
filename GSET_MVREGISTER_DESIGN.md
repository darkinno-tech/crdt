# G-Set and MV-Register design

This document fixes the semantics and operational boundary for the two stable
framed CRDTs added after the original counter/set core. Their state and delta
frames are assigned `13/14` (G-Set) and `15/16` (MV-Register). They are
included in the default `crdt.ProtocolPolicy` because each has a complete,
bounded, deterministic codec and no HLC or tombstone-GC lifecycle.

## G-Set

`set.GSet[T]` is a grow-only set: its state is the set of canonical elements;
`Add`, `Merge`, and `ApplyDelta` all use set union. It deliberately has no
`Remove`. An application that needs removal must use `ORSet`, including its
tombstone acknowledgement requirements.

- Merge law: `S join T = S union T`, therefore it is associative,
  commutative, and idempotent.
- Deltas are partial G-Set states, normally a single element. A duplicate
  delta first checks membership under a read lock and does not take the write
  lock.
- Values are caller-defined `comparable` values. A stable `ElementCodec` is
  required for frames; elements sort by their canonical encoded bytes, not Go
  map order.
- The decoder rejects codec mismatch, noncanonical order, duplicate encoded
  values, non-round-trippable values, trailing data, and configured limits.

## MV-Register

`register.MVRegister` represents state as `(C, V)`: `C` is a version vector
from replica ID to the greatest observed counter, and `V` maps the causally
maximal dots to opaque byte values. A dot is `(replica ID, counter)`.

`Set(value)` increments the local component, replaces `V` with the new dot,
and returns that dot plus the *full* resulting `C`. Consequently it overwrites
every value the writer observed, but cannot overwrite a value that is only
concurrent with it. `Values` returns all concurrent values in canonical dot
order; `Value` succeeds only when exactly one value is visible.

For states `(C1, V1)` and `(C2, V2)`, a merge retains:

```
(V1 intersect V2) union (V1 minus C2) union (V2 minus C1)
C = component-wise-max(C1, C2)
```

where `V minus C` removes a dot whose counter is covered by that vector. A
shared dot must have identical bytes; otherwise the merge fails with
`ErrTagConflict` and leaves the receiver unchanged. This makes replica-ID
reuse without restoring state unsafe: it can create one dot with two values.

## Complexity and persistence

| Operation | G-Set | MV-Register |
| --- | --- | --- |
| Local write | expected O(1) | O(1) plus copying the returned delta context |
| Duplicate delta | expected O(delta elements), read lock only | O(delta context + values), read lock only |
| Merge | O(source elements) | O(context entries + visible values) |
| Frame encoding | O(n log n) for canonical element order | O(r log r + v log v) for context/value ordering |

Frames and snapshots copy all exposed bytes. Persist an MV-Register snapshot
before reusing its replica ID, because the state frame contains the local
causal counter. Decoder limits bound contexts, visible values, individual IDs,
values, payloads, and frames; untrusted inputs are never applied partially.
