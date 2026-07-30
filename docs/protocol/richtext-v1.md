# Rich-text v1 wire protocol

This is the normative wire specification for bounded inline rich text. It
allows a browser, mobile, or server implementation to exchange text and inline
formatting without changing the stable RGA run-v2 contract. Implementations
MUST satisfy this document and the [canonical vectors](testdata/richtext-v1-vectors.json).

The key words **MUST**, **MUST NOT**, **SHOULD**, and **MAY** are normative.

## 1. Contract and negotiation

Before sending a frame, an authenticated `replica.Manifest` MUST bind:

| Field | Required value |
| --- | --- |
| state TypeID | `23` |
| delta TypeID | `24` |
| codec ID | empty byte string |
| semantics version | `1` (`richtext.SemanticsVersion`) |
| schema ID | one exact, application-owned inline-attribute/rendering schema |
| epoch and limits | exact-match authenticated group values |

TypeIDs, CRC-32C, and a matching schema ID do not authenticate a peer,
authorize access, sanitize rendered values, or encrypt content. The schema ID
is not decorative: it MUST identify the allowed attribute keys, values, link
policy, and rendering/sanitization rules. A rich-text manifest MUST NOT share a
replication group with raw RGA run-v2 frames.

`richtext.SemanticSchemaID` (`github.com/DarkInno/crdt/richtext/semantic/v1`)
is an optional schema adapter, not a change to TypeIDs 23/24. A group that
uses it MUST negotiate that exact schema ID and MUST reject `rt.*` semantic
attributes from a different schema. It standardizes `rt.bold`, `rt.italic`, a
single U+FFFC object-replacement character with `rt.embed.kind`/`rt.embed.data`,
and paragraph-wide atomic `rt.block` markers. Generic v1
implementations remain free to reject or render those keys as ordinary opaque
attributes.

## 2. Frame and common types

Each message uses the repository's canonical CRDT frame envelope with TypeID
`23` or `24` and an empty codec ID. `uvarint`, `bytes`, `tag`, checksum, and
canonical tag ordering are defined by the [RGA run-v2 protocol](rga-run-v2.md).
`rga-delta` and `rga-state` below are complete nested canonical run-v2 frames
(TypeIDs `20` and `19` respectively), not raw payloads.

```text
attribute-change = key kind [value]
key              = bytes ; non-empty valid UTF-8
kind             = uvarint(0 assign / 1 remove)
value            = bytes ; valid UTF-8, required only for assign
operation        = tag target-count target* change-count attribute-change*
target           = tag
mark             = target key tag kind [value]
```

An attribute removal (`kind = 1`) MUST omit `value`; an assignment (`kind =
0`) MUST include it. Attribute values are opaque valid UTF-8 at this layer.

## 3. Delta payload

```text
delta-payload = rga-delta operation-count operation*
rga-delta     = bytes ; empty or a TypeID 20 canonical RGA run-v2 frame
```

The empty nested frame means that the delta changes formatting only. Each
operation MUST have at least one target and one attribute change. Operations
sort by tag ascending; targets sort by tag ascending without duplicates; changes
sort by bytewise key ascending without duplicates. A formatter resolves a local
offset range to these immutable RGA positions when it creates the operation.
It MUST NOT infer formatting for later concurrent inserts.

## 4. State payload

```text
state-payload = rga-state mark-count mark*
rga-state     = bytes ; one complete TypeID 19 canonical RGA run-v2 frame
```

Marks sort by target position ascending and then attribute key ascending. A
state MUST contain one mark at most for each `(position, key)`, including a
removed mark. Nested RGA state MUST contain all non-root parents and no pending
dependencies.

A decoder MUST validate the complete nested frame, every tag and UTF-8 string,
all ordering rules, count and byte budgets, and the outer canonical form before
changing visible text, formatting metadata, HLC state, or persistence. It MUST
re-encode a decoded frame byte-for-byte to reject alternate encodings.

## 5. Semantics

Rich text is the composition of an RGA run-v2 node set and a per-position,
per-key LWW register:

```text
position -> attribute key -> { HLC tag, UTF-8 value, removed }
```

For one `(position, key)`, the greatest full HLC tag wins. The same tag with a
different value is a conflict. A remove is retained and wins over a delayed
older assignment. A format operation may arrive before its target text; retain
it within the mark budget and expose it only once that position becomes visible.
`Spans()` coalesces adjacent visible positions with equal live attribute maps.

This exact-position model deliberately avoids unproven boundary inheritance.
It follows the separation of sequence and formatting required by rich-text
CRDT research such as [Peritext](https://doi.org/10.1145/3555644); applications
that need paragraphs, lists, tables, embeds, HTML, CSS, or executable values
MUST use a distinct, versioned object model.

The optional semantic schema does not change that rule: an embed is one exact
text position and a block marker is written to the exact current positions of
the selected paragraphs. Editors MUST explicitly assign attributes to later
inserts; they MUST NOT infer formatting or block membership across a concurrent
boundary. The embedded JSON object is application data, not markup or a URL
policy, and must be authorized and sanitized by the renderer.

## 6. Limits, security, and persistence

Receivers MUST bound outer frame bytes, payload bytes, tags, replica IDs,
strings, nested RGA retained/pending state, retained marks, attributes per
operation, and the product of targets and attributes before allocating or
mutating. A rejected delta MUST leave the RGA, marks, and HLC state unchanged.

An application MUST authenticate and authorize the group before decoding;
enforce its schema's URL/content policy before rendering; rate-limit writers;
and atomically persist `{state frame, HLC state, delivery frontier/outbox}`.
CRC-32C detects accidental corruption only.

## 7. Tombstone lifecycle

Text deletion tombstones retain structural RGA anchors. Attribute removals
retain LWW deletion markers. `Document.TombstoneTags` exposes both as a
canonical exact-acknowledgement input. After every member in one authenticated
membership epoch has acknowledged exact tags, a durable post-compaction
snapshot is recorded, and old-epoch frames are retired, an application MAY use
`Document.CompactTombstones` or `Document.CompactEligibleTombstones` (including
through `tombstonegc.Coordinator`).

The ordinary compactor is all-or-nothing for requested RGA structure. The
eligible compactor removes safe deleted descendants before ancestors. Both
retain out-of-order formatting for positions that were never locally retained;
they remove all formatting metadata attached to a text position only after that
position was structurally compacted. These methods do not themselves prove
membership authority, checkpoint durability, or old-frame retirement.

## 8. Conformance and versioning

For every [vector](testdata/richtext-v1-vectors.json), an implementation MUST
verify its frame, decode it, re-encode it identically, and obtain the specified
visible text and spans. It SHOULD also mutate a checksum, TypeID, ordering,
varint representation, and resource limit, confirming rejection without state
change. The repository additionally runs shuffled duplicate/out-of-order
multi-editor simulations, race detection, fuzzing, tombstone-GC integration,
and large-document benchmarks.

TypeIDs `23/24` and semantics version `1` are immutable. Any incompatible
change to framing, ordering, conflict resolution, format inheritance, schema
interpretation, or compaction requires a new TypeID pair, semantic version,
vectors, manifest agreement, and migration path.
