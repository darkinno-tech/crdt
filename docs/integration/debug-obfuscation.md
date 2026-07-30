# RGA diagnostic obfuscation

`text.RGA` can export an isolated, structurally valid debug copy without
sharing its inserted characters. This is analogous to a document support
artifact, not a security boundary or a replacement for end-to-end encryption.

```go
// State export for a support case. It does not mutate document.
debugState, err := document.MarshalObfuscatedRunBinary()

// Incremental update export for the same isolated debug timeline.
debugDelta, err := delta.MarshalObfuscatedRunBinary()
```

Legacy scalar RGA methods are also available:
`MarshalObfuscatedBinary`, `MarshalObfuscatedBinaryWithLimits`, and
`Delta.Obfuscate`. All `WithLimits` variants enforce the same frame budget as
their normal encoder counterparts.

## Preserved and removed data

Each inserted Unicode scalar becomes a fixed inert placeholder with the same
uvarint width. The export preserves:

- node positions and parent relationships;
- tombstones, operation counts, and visible/tombstoned structure;
- canonical wire validation and the ability to merge independently obfuscated
  deltas from the same original timeline; and
- frame payload length for the normal scalar and run encoders.

It removes the actual text values and never mutates the source `RGA` or delta.
This makes duplicate and shuffled **obfuscated** frames usable in an empty
debug replica for reproducing parser, ordering, pending-parent, tombstone, and
snapshot behavior.

## Safety boundary

Obfuscation deliberately retains replica IDs, HLC-derived positions, document
topology, operation counts, and approximate rune encoding widths. These may
reveal author identifiers, timing, document size, edit shape, and character
classes. It is therefore unsuitable for adversaries, regulated exports, or
logs that must hide metadata. Scrub application-defined schema names, provider
headers, attachment references, and surrounding logs separately.

Never apply an obfuscated delta to a replica that can already contain the
original delta: RGA positions are immutable, and a different character at the
same position is correctly rejected as `text.ErrTagConflict`. Keep original
and obfuscated artifacts in distinct support namespaces and never send either
to a live peer without normal authentication, authorization, and transport
encryption.

The automated simulation verifies three debug replicas converge after duplicate
and shuffled run-v2 deliveries, while an original delta is rejected after its
obfuscated counterpart. Measure export overhead on the actual support machine:

```sh
go test -run='^$' -bench='BenchmarkRGARunObfuscatedState' -benchmem ./text
```
