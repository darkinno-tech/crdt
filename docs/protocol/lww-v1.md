# LWW Set and Map v1 wire protocol

This is the normative contract for stable LWW-Set (TypeIDs `7/8`) and LWW-Map
(TypeIDs `9/10`). Implementations MUST satisfy this document and the
[canonical vectors](testdata/lww-v1-vectors.json). `SemanticsVersion` is `1`.

An authenticated manifest binds one exact state/delta pair, schema ID, codec
ID, epoch, and receiver limits. A set frame uses an application-owned canonical
codec ID; a map frame uses an empty codec ID and UTF-8 string keys with opaque
byte values. Type IDs and CRC-32C detect framing errors only: they do not
authenticate a peer, authorize mutation, encrypt data, or validate application
values.

Each payload is `entry-count entry*`, sorted by key/encoded set element. An
entry is `key-or-element tag present [value]`; `present` is exactly `0` or `1`.
Tags sort by wall time, logical clock, then replica ID. A map value is present
only when `present = 1`. Decoders MUST reject unsorted/duplicate entries,
duplicate tags assigned to different entries, malformed tags or lengths,
non-canonical values, trailing bytes, a wrong TypeID/codec, checksum failures,
or any breached limit before mutation.

For one key/element, the entry with the greatest tag wins; an incompatible
reuse of the same tag is rejected. Join is therefore commutative, associative,
and idempotent. Persist `{state frame, HLC state, delivery frontier/outbox}`
atomically. State bytes without HLC state can reuse a local tag after restart.

Delete entries are tombstones. They MAY be removed only after every active
member has acknowledged the exact tags in one authenticated membership epoch,
a post-compaction snapshot is durable, and old-epoch deltas are retired. Use
`tombstonegc.Coordinator` with `CompactTombstones`; a frontier, later HLC, or
Merkle maximum is not an acknowledgement of a specific delete.

Any incompatible change to ordering, conflict rules, encoding, limits, or
tombstone retirement requires a new TypeID pair and semantic version.
