# Bounded Rich Text (Inline Formatting) Design

## Decision

Rich text is a stable, manifest-bound CRDT instead of a change to either RGA
v1 (`11/12`) or RGA run-v2 (`19/20`). Its immutable wire pair is `23/24` with
semantic version `1`; the zero-value `crdt.ProtocolPolicy` accepts it. The
[rich-text v1 wire protocol](../protocol/richtext-v1.md) is normative.

The first version deliberately supports **inline attributes** only: bold,
italic, colour tokens, links, comments, and application-defined string
attributes. Paragraphs, lists, tables, embeds, and HTML are separate object
models and are not encoded by this protocol.

## Evidence and constraints

The existing `text.RGA` already supplies stable character positions, bounded
out-of-order integration, deterministic run-v2 frames, and HLC-backed
snapshot recovery. Its stable positions make it a safe text substrate, but
formatting is a different replicated data type: inserting fields into an RGA
node would make legacy frames ambiguous and let old clients silently discard
formatting.

This design is informed by [Peritext](https://doi.org/10.1145/3555644), which
shows that naive rich-text extensions create intent anomalies and represents
formatting independently from the character sequence. It also keeps a useful
application boundary from [Y.Text's delta model](https://docs.yjs.dev/api/delta-format):
the editor sees contiguous text spans with attribute maps, rather than CRDT
metadata.

## Data model and convergence rule

`richtext.Document` composes one private run-v2 RGA and a bounded map:

```text
Position -> attribute key -> { HLC tag, string value, deleted }
```

- `Position` is an immutable RGA character identifier, never a byte offset.
- `Format(offset, count, changes)` resolves the current visible offsets to an
  exact set of positions. A later concurrent insertion is therefore not
  accidentally styled merely because it renders between two old characters.
- Each `(position, key)` is a Last-Writer-Wins register ordered by the complete
  HLC tag. Equal tags with different values are a conflict and reject the
  frame; equal values are idempotent.
- Removing an attribute records a retained LWW tombstone. It wins over a
  delayed older assignment and is not represented by an ambiguous empty value.
- A formatting delta may arrive before its text delta. The register is retained
  (within the configured bound) and becomes visible once its position becomes
  visible in the RGA. This eliminates a delivery-order dependency.
- `Spans()` walks visible RGA positions and coalesces adjacent characters with
  equal live attributes. It never exposes tags, tombstones, or orphaned
  formatting metadata.

These rules make a merge a per-position, per-key maximum plus the existing RGA
join. It is commutative, associative, and idempotent. This is intentionally
more conservative than a boundary-span protocol: exact target positions cost
linear bytes in the selection but avoid making an unproven inheritance rule
part of the initial wire contract. `InsertWithAttributes` is the explicit way
to style newly inserted text.

### Hot-path representation

The wire and logical model remain one LWW register per `(position, key)`, but
the in-memory representation stores a position's first attribute inline and
allocates an overflow map only when a second key is present. Ordinary range
formatting (for example, `bold` over a selection) therefore does not allocate
one Go map per character. The state encoder still sorts every logical register
by position then key, so this is an implementation optimization rather than a
wire or snapshot change.

`Spans()` obtains positions and runes from one RGA projection. It must not
compose separate position and string reads: doing so both traverses a large
document twice and weakens the useful invariant that the two slices describe
the same visible version.

## Architecture

```text
editor offsets
   | resolve only for local edits
   v
richtext.Document
   |-- text.RGA (run-v2 nested state/delta)
   `-- LWW attribute registers (stable RGA positions)
             |
             v
       Type 23/24 canonical outer frame
             |
             v
 authenticated replica.Manifest + stable policy
```

The outer payload contains one canonical nested run-v2 RGA frame and either
compressed formatting operations (delta) or canonical register entries
(state). A formatting operation stores one HLC tag, an ordered set of target
positions, and an ordered set of attribute changes. This makes a single
attribute change over a long selection `O(selection + attributes)` on the
wire, rather than duplicating the key/value per character. State remains one
register record per retained `(position, key)` so removals and partial delivery
can recover exactly.

## Safety boundaries

| Dimension | Required behaviour |
| --- | --- |
| Correctness | Validate every complete outer and nested frame before mutation; reject non-canonical order, duplicate targets/keys, invalid UTF-8, invalid tags, equal-tag conflicts, and wrong TypeIDs. |
| Resource safety | Bound text nodes with `text.Options`; separately bound retained mark registers and attributes per operation. One delta may perform no more target/key updates than the document's retained-mark budget, so a repeated overwrite cannot multiply decoder work without limit. Decoder limits cap frames, elements, tags, and strings before allocation. |
| Atomic rejection | Preflight metadata before applying it. A nested RGA delta performs its own limit check before the rich-text document witnesses formatting tags; a rejected text resource limit therefore leaves visible text, marks, and HLC state unchanged. |
| Concurrency | A document-level mutex makes a compound text-plus-format delta atomic to public callers; RGA retains its own synchronization. |
| Security | Attributes are UTF-8 strings, not HTML, CSS, JSON, URLs, or executable values. Rendering policy, link allowlists, sanitization, authorization, rate limits, and identity remain application-owned. CRC-32C detects corruption, not attackers. |
| Persistence | Save the rich-text state frame and its shared RGA HLC state atomically. Keep removed attributes and text tombstones until the same authenticated epoch/exact-ack checkpoint rule permits retirement. |
| Compatibility | Frames `23/24` are accepted by the zero policy. A manifest's exact `SchemaID` binds the renderer/attribute schema; a rich-text manifest cannot be mixed with raw RGA `19/20` frames in one replication group. |

## API contract

The public surface is intentionally small:

- `New` / `NewWithOptions` and snapshot recovery.
- `Insert`, `InsertWithAttributes`, `Delete`, and `Format` return opaque,
  canonical deltas only after output-size preflight.
- `ApplyDelta`, `Merge`, `MarshalBinary`, `UnmarshalBinary`, and
  `SnapshotCurrentState` provide replication and recovery.
- `String`, `Spans`, and `AttributesAt` expose presentation data without
  leaking positions or clock state.

Attribute changes use `{Key, Value, Remove}`. Keys are non-empty UTF-8 strings;
`Remove` must not carry a value. Values are opaque strings. Attribute meaning
and any schema such as `bold=true` or `link=https://…` are application-owned.

### Relative positions and semantic formatting adapter

`Document.AnchorAt` and `Document.ResolveAnchor` expose the already-defined
`text.Anchor`; rich text does not add a second relative-position identity
format. `text.Anchor.MarshalBinary` / `UnmarshalAnchor` provide a bounded,
versioned host-metadata record for a persistent cursor. `AnchorRangeAt` /
`ResolveAnchorRange` and the equivalent `AnchorRange` codec capture an
anchor/head selection or comment range from one locked RGA projection while
preserving direction. These are not TypeID 23/24 frames and must be stored
beside an authenticated document checkpoint, bound to its document/group/epoch
by the host.

`FormatAnchored` resolves its start and exclusive end under the same document
lock that creates the exact-position format delta. A compacted anchor returns
`text.ErrAnchorGone` rather than silently moving a user's selection or
comment. Tombstone retention is therefore part of the product's annotation
retention policy; a compaction coordinator cannot silently rewrite anchors.

The optional `SemanticSchemaID` adapter provides typed helpers for bold,
italic, embeds, and block presentation while preserving the v1 wire model.
`InsertEmbed` inserts exactly U+FFFC with a lower-case identifier and a bounded
JSON object; consumers must still enforce their own asset, URL, and renderer
policy. `FormatBlocks` expands a selection to complete newline-delimited
paragraphs and writes a validated `paragraph`, `heading` (levels 1-6), `quote`,
`code`, or `list-item` marker to each current rune. These markers do not inherit
onto future concurrent inserts: an editor that needs inheritance must explicitly
apply it in its local adapter after manifest/schema negotiation.

The adapter also exposes `Blocks()` for a single consistent newline-delimited
projection, `ClearBlocks` for LWW marker removal, and anchored variants for
relative selections. `Blocks()` only reports a `BlockFormat` when every current
position in a paragraph agrees on one valid marker; conflicting concurrent
edits remain visibly unformatted rather than choosing an arbitrary winner.
`InsertWithBlockFormat` is the intentional, explicit way for an editor to add
new text with a selected block format.

## Performance plan and acceptance tests

The implementation must retain the RGA's run-v2 text compression. Formatting
has these expected costs:

| Operation | Time | Additional retained memory |
| --- | --- | --- |
| Apply one format op | `O(targets * changes)` | one register per new `(position,key)` |
| Render spans | `O(visible characters + live attributes)` | one text projection plus result; maps are copied once per emitted span, not once per rune |
| Encode one format delta | `O(targets + changes)` | output only |
| Encode state | `O(retained registers log retained registers)` | sorted output plan |

The test plan covers unit and wire rejection paths, three-replica shuffled
duplicate delivery, formatting-before-text delivery, deletion, concurrent
same-key writes, snapshot/restart, race detection, decoder fuzzing, and
benchmarks for 10K-character formatted selections and realistic multi-editor
documents. Results are a local production-like simulation, not proof of a
live deployment.

## Stable boundary

The stable surface is intentionally limited to inline attributes selected by
exact RGA positions. Canonical cross-language vectors, schema-bound manifests,
large-document benchmarks, exact-acknowledgement metadata compaction, and
concurrent formatting simulations are part of the contract. Boundary
inheritance, paragraphs, lists, tables, embeds, HTML/CSS values, and tree
structure remain out of scope; any of them requires a separately negotiated
protocol instead of changing v1 semantics.
