# Document-tree v1 wire protocol

> Historical protocol only. New groups MUST use
> [document-tree v2](document-tree-v2.md); TypeIDs `27/28` are never reused.
> v2 intentionally removes v1's external lazy-reference value and does not
> admit v1 frames through its default protocol policy.

This document is normative for TypeIDs `27` (state) and `28` (delta), semantic
version `1`. A peer MUST negotiate this exact pair in an authenticated
`replica.Manifest` with an empty codec ID. It is not wire-compatible with Yjs,
the TypeScript native nested contract, or existing Go collection frames.

## Frame and record layout

The outer frame, `uvarint`, `bytes`, `tag`, checksum, and canonical tag order
are defined by [RGA run-v2](rga-run-v2.md). The payload has five sorted
sections:

```text
payload = roots objects map-records array-records tombstones
roots = count root*
root = name kind object-id
objects = count object*
object = object-id kind owner-kind owner
owner = root-name / map-parent map-key / array-parent
map-records = count map-record*
map-record = target key write-tag present [value]
array-records = count array-record*
array-record = target position parent-present [parent] value
tombstones = count tombstone*
tombstone = target position
value = bytes-value / object-value / subdocument-value
bytes-value = 1 bytes
object-value = 2 kind object-id
subdocument-value = 3 subdocument-id
```

Kinds are `1` Map and `2` Array. Owners are `1` root, `2` map, and `3` array.
Roots sort by bytewise name; objects sort by object ID; map records sort by
`{target,key}`; array records and tombstones sort by `{target,position}`.
Every count, varint, flag, string and tag must be canonical; re-encoding a
decoded payload must produce exactly the received bytes.

## Semantics

Root declarations and map keys are LWW registers using their creation/write
tag. Arrays are RGA node sets: a position has one immutable parent and visible
siblings traverse in descending tag order; tombstones hide a position while
retaining it as an anchor. All records merge by union except the greatest map
or root tag wins. Equal tags with different immutable contents are invalid.

An object-valued map write MUST have `write-tag == child-object-id`; an
object-valued array node MUST have `position == child-object-id`. The child
declaration's owner must exactly name that map key or array position. This is
the single-owner invariant. A receiver MUST reject type mismatch, alias,
cycle, conflicting ID, or any array self-parent before mutation.

A delta MAY name missing root/object/array-parent records. It is retained only
inside local pending limits and becomes visible when dependencies arrive. A
state frame MUST contain no unresolved dependencies. Tombstones may name an
unknown position so a deletion delivered first remains effective when its node
later arrives.

## Limits, recovery, and versioning

The host MUST bound transport body bytes before allocation and set compatible
`documenttree.Options` for every group. It must authenticate and authorize the
manifest, parent schema, scalar values, and each subdocument group before
accepting a frame. Persist `{state frame, HLC state, delivery frontier, outbox}`
atomically; restore a same replica ID only with `NewFromSnapshot`.

There is no in-place move, automatic subdocument load, inherited permission,
tombstone GC, or subdocument-content transport in v1. Any change to these
semantics, record order, value interpretation, ownership rule, or frame layout
requires a new TypeID pair, semantic version, conformance vectors, manifest,
and migration path.
