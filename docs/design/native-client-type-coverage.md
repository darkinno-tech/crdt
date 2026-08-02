# Native client type-coverage decision

## Decision

Rust, Python, Swift, and C++ are supported bindings over one Rust semantic
core, not four independent CRDT ports. A generic `void *` frame handle cannot
turn every registered TypeID into a safe native CRDT implementation.

This change adds a complete native `lww-map-v1` implementation alongside
`rga-run-v2`: Go TypeIDs `9/10`, semantic version `1`, empty codec, and the
same canonical frames as Go. It does not claim every registry pair is native.

## Maturity matrix

| Native status | Protocols | Reason |
| --- | --- | --- |
| Go-wire conforming now | LWW-Map 9/10; run-v2 RGA 19/20 | Normative contract, canonical Go vectors, bounded Rust merge/recovery, and Python/Swift/C++ execution paths. |
| Needs its own implementation | G/PN counter 1/3/5/6; OR-Set 2/4; LWW-Set 7/8; G-Set 13/14; MV-Register 15/16 | Actor/value codecs, overflow, add/remove observations, multi-value projection, and tombstones are not map aliases. |
| Must stay protocol/profile specific | scalar RGA 11/12; OR-Tree 17/18; List RGA 21/22; rich text 23/24; MoveRGA 25/26; document tree 27/28; packed RGA 29/30 | Each has exact element, move, editor-schema, or recursive-declaration semantics. |
| Not a native CRDT type | attachment references, awareness, Yjs relay | These require application validation, ephemeral session state, or opaque relay behavior. |

## Why LWW-Map is the next vertical slice

| Dimension | Decision |
| --- | --- |
| Correctness | Implement the exact key/tag ordering, equal-tag conflict, tombstone, TypeID, and canonical-vector contract. |
| Security/resources | Bound frame, payload, key/value, entry, and tombstone state before mutation; bind an application value schema in the authenticated manifest. |
| Performance | Use single-key deltas and a sorted complete state; preserve copy-on-validate atomic rejection until retained-state benchmarks justify a change. |
| API | Expose `set`, `delete`, `get`, visible ordered `keys`, `state`, and `clock_state`; values remain opaque bytes. |
| Maintenance | Keep one audited Rust core and thin ownership wrappers; do not misrepresent them as independent language implementations. |

## Contract boundary

`LwwMap` accepts only empty-codec TypeID 9 complete state and TypeID 10 delta.
Before mutation it rejects checksum/type/codec errors, non-shortest varints,
invalid UTF-8 or blank keys/replica IDs, unsorted or duplicate keys, duplicate
tags across keys, same-tag conflicting data, trailing bytes, and exceeded
negotiated limits. The greatest tag wins per key; deletes remain tombstones.

The durable unit is `{state frame, HLC state, application frontier/outbox}`.
Neither CRC, a frontier, HLC maximum, nor this binding authenticates a peer or
authorizes tombstone deletion. Hosts still enforce body limits, authentication,
document/schema/epoch authorization, replay policy, TLS/storage protection,
and opaque-value validation. `keys` is an FFI read list, never a wire or
persistence representation.

## Gate for another native type

Every new pair needs a normative protocol and Go vectors; bounded Rust
decode/encode/merge with atomic rejection; duplicate/reorder/partition and
recovery tests; a deliberate ownership-safe FFI model; real Python/Swift/C++
execution; controlled performance evidence; and manifest/authorization/
durability/tombstone documentation. Frame decoding alone is not client support.
