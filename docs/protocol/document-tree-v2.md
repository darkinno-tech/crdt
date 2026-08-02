# Document-tree v2 wire protocol

This document is normative for TypeIDs `31` (state) and `32` (delta),
semantic version `2`. A peer MUST negotiate this exact pair in one
authenticated `replica.Manifest` with an empty codec ID. It is not wire
compatible with Yjs, TypeScript `native-ts-nested-v1`, or document-tree v1
(`27/28`).

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
value = bytes-value / object-value
bytes-value = 1 bytes
object-value = 2 kind object-id
```

Kinds are `1` Map and `2` Array. Owners are `1` root, `2` map, and `3` array.
Roots sort by bytewise name; objects sort by object ID; map records sort by
`{target,key}`; array records and tombstones sort by `{target,position}`.
Every count, varint, flag, string and tag MUST be canonical; re-encoding a
decoded payload MUST produce the identical payload bytes.

## Semantics and closure

Root declarations and map keys are LWW registers using their creation/write
tag. Arrays are RGA node sets: a position has one immutable parent and visible
siblings traverse in descending tag order; tombstones hide a position while
retaining it as an anchor. All records merge by union except the greatest map
or root tag wins. Equal tags with different immutable contents are invalid.

An object-valued map write MUST have `write-tag == child-object-id`; an
object-valued array node MUST have `position == child-object-id`. The child
declaration's owner MUST exactly name that map key or array position. This is
the single-owner invariant. A receiver MUST reject a type mismatch, alias,
cycle, conflicting ID, or array self-parent before mutation.

Every reachable child is a Map or Array record in the same replication group.
There is no subdocument value, lazy-load state, GUID-like external document
identity, cross-group reference, or separate child content transport. A
complete state frame and a checkpoint therefore represent the complete bounded
tree. If a product needs different authorization, retention, or load behavior,
it MUST create a distinct authenticated replication group at that product
boundary rather than insert a pointer into this tree.

A delta MAY name missing root/object/array-parent records. It is retained only
inside local pending limits and becomes visible when dependencies arrive. A
state frame MUST contain no unresolved dependencies. Tombstones MAY name an
unknown position so a deletion delivered first remains effective when its node
later arrives.

## Limits, recovery, and migration

The host MUST bound transport body bytes before allocation and set compatible
`documenttree.Options` for every group. It MUST authenticate and authorize the
manifest, full-tree schema, and scalar values before accepting a frame. Persist
`{state frame, HLC state, delivery frontier, outbox}` atomically; restore a
same replica ID only with `NewFromSnapshot`.

`27/28` are permanently reserved for document-tree v1 and are not admitted by
the v2 protocol policy. `MigrateV1State` and `MigrateV1Delta` are explicit,
offline conversion helpers for v1 frames that contain only bytes and integrated
objects. They reject v1's former external lazy-reference value because its
content was absent from the old frame; an application must first recover that
content and model it as an owned Map/Array tree.

There is no in-place move or tombstone GC in v2. Any change to these semantics,
record order, value interpretation, ownership rule, or frame layout requires a
new TypeID pair, semantic version, conformance vectors, manifest, and
migration path.
