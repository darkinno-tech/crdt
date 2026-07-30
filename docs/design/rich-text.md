# Bounded Rich Text (Inline Formatting) Design

## Decision

Add rich text as a new **experimental, manifest-bound** CRDT instead of
changing either RGA v1 (`11/12`) or RGA run-v2 (`19/20`). The proposed wire
pair is `23/24`, with semantic version `1` and an explicit
`crdt.ProtocolPolicy{AllowExperimental: true}` at every replication boundary.

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
 authenticated replica.Manifest + experimental policy
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
| Compatibility | Frames `23/24` are rejected by the zero policy. A rich-text manifest cannot be mixed with raw RGA `19/20` frames in one replication group. |

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

## Promotion gates

This remains experimental until it has cross-language vectors, a renderer
schema/interoperability contract, measured large-document profiles, a
format-metadata GC bridge tied to exact acknowledgements, and an
intent-preservation evaluation for boundary spans and concurrent formatting.
