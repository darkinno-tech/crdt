# Generic list, XML fragments, and local undo/redo

This document defines the stable collaboration boundary for ordered application
values and XML-like documents. A replication group must bind the exact
state/delta IDs, codec ID, and semantics version in its authenticated
`replica.Manifest`.

## Generic RGA list

`list.RGA[T]` stores one canonical caller-coded `T` at each stable RGA
position. It is not a text wrapper: a whole value is one insertion/deletion
unit, regardless of the number of bytes in its encoding.

```go
type stringCodec struct{}

func (stringCodec) ID() string { return "example.com/task/v1" }
func (stringCodec) Marshal(value string) ([]byte, error) { return []byte(value), nil }
func (stringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

tasks, err := list.New("desktop-a", stringCodec{})
if err != nil { /* handle */ }
delta, err := tasks.Insert(0, []string{"draft", "review"})
if err != nil { /* handle */ }
// Deliver delta.MarshalBinary() through an authenticated, bounded transport.
```

The list uses TypeIDs `21/22` with the codec ID in the frame envelope. Each
local or received value must round-trip through `Unmarshal` then `Marshal` to
the identical byte sequence before it is retained. This rejects a wrong codec
or an ambiguous/non-canonical representation before state mutation. Snapshots
are refused while a parent is unresolved; persist the emitted HLC state with
the complete snapshot before reusing the replica ID.

The indexed sequence uses stable enter/exit markers and a deterministic
sibling order. Offset lookup is `O(log n)`; full value projection is `O(n)`.
It retains deletion tombstones as structural anchors. Only call
`CompactTombstones` after a durable, exact-acknowledgement membership epoch,
post-compaction checkpoint, and retirement of older deltas. It refuses any
node with a retained child or unresolved dependent.

## Move-capable sequence

`list.MoveRGA[T]` is the separately versioned answer for products whose domain
identity must survive drag-and-drop, prioritisation, or kanban reordering. It
uses TypeIDs `25/26` with MoveRGA semantics version `2`; it is not
wire-compatible with `list.RGA[T]` or MoveRGA semantics version `1`, and has
its own manifest schema/semantics contract.

```go
tasks, err := list.NewMoveRGA("desktop-a", stringCodec{})
if err != nil { /* handle */ }
_, _ = tasks.Append([]string{"draft", "review", "publish"})
move, err := tasks.Move(2, 1, 0) // `to` is indexed after removing the range.
if err != nil { /* handle */ }
// `publish` keeps its original Position identity; replicate move as usual.
wire, err := move.MarshalBinary()
_ = wire
```

The state has three monotonic components: immutable value nodes, observed-remove
tombstones, and one HLC-tagged placement register per node. A range move uses
one operation tag plus per-item rank, so its local order remains defined. The
join is immutable-node union, tombstone union, and maximum placement register
per element. This makes duplicate and out-of-order delivery idempotent.

Two concurrent moves may request `A after B` and `B after A`. That is a real
conflict rather than an implementation error: storing a mutable parent pointer
would produce a cycle. `MoveRGA` keeps both operations in the join state and
constructs a deterministic projection by considering placement tags in canonical
order and dropping only a cycle-closing attachment to the synthetic root. Every
replica therefore makes the same bounded repair without rewriting either user's
operation. A snapshot accepts such resolved-by-projection state, but rejects
missing node or anchor dependencies.

Projection is currently `O(n log n)` because it recomputes attachment order and
the deterministic cycle repair. That is a deliberate correctness-first tradeoff;
benchmark the real move mix before adopting it
for very large, frequently reordered lists. Do not compact tombstones or old
placement anchors until an authenticated exact-acknowledgement epoch, durable
post-compaction snapshot, and old-delta retirement are complete.

## XML fragments

`xml.Fragment` is an ordered `list.RGA[xml.Node]` with a canonical XML-node
codec (`github.com/DarkInno/crdt/xml-fragment-node/v1`). It replicates
insertion and removal of complete element/text nodes. Nodes can contain a
complete immutable subtree, which makes offline insertion, duplication,
reordering, and deletion converge under the same list protocol.

```go
fragment, err := xml.New("editor-a")
if err != nil { /* handle */ }
article, err := xml.ParseDocument([]byte(`<article><p>draft</p></article>`))
if err != nil { /* handle */ }
delta, err := fragment.Append([]xml.Node{article})
if err != nil { /* handle */ }
rendered, err := fragment.RenderFragment()
```

`ParseDocument` is deliberately strict and bounded (4 MiB input, 65,536
nodes, depth 128, 32,768 attributes). It accepts one document root and text/
element content; it rejects DTDs, comments, non-declaration processing
instructions, namespaces, duplicate attributes, invalid XML characters, and
extra top-level content. Only XML `S` characters (space, tab, CR, and LF) may
follow the root; broader Unicode whitespace is content and is rejected rather
than silently discarded. Rendering sorts attributes canonically and escapes
text/attribute values through Go's `encoding/xml` encoder.

This is **not** a claim of per-attribute or per-descendant mutable XML merge.
Replacing an element is a delete plus insertion of an immutable replacement.
Concurrent attribute registers, namespace preservation, comments, and rich
text formatting need their own separately versioned semantics and frame types;
they must not be added by changing the current node encoding in place.

## Local text undo/redo

`text.UndoManager` captures only calls made through its `Insert` and `Delete`
methods. It emits compensating RGA deltas, so it does not roll back shared
state, erase a remote edit, or remove a tombstone. Redo creates fresh RGA
positions; it never resurrects old tags.

```go
history, err := text.NewUndoManager(document)
if err != nil { /* handle */ }
_, _ = history.Insert(0, "draft")
undoDelta, err := history.Undo()
// Replicate undoDelta exactly like any other local RGA delta.
```

Undo history is process-local and intentionally not serialized. Its default
policy retains at most 256 entries and 1,048,576 Unicode scalar values;
callers that need another local policy can use `NewUndoManagerWithOptions`.
When a successful edit would exceed the total retained budget, the manager
releases the complete local stack and captures that newest edit. An individual
edit larger than `MaxRunes` fails with `text.ErrUndoHistoryLimit` before it
changes the RGA. These limits bound local metadata only; they do not replace
RGA, frame, outbox, storage, identity, or transport limits.

Call `Clear` before compaction. If either a retained predecessor or a position
that a compensating tombstone must target has already been compacted, undo/redo
fails with `text.ErrUndoAnchorGone` instead of guessing a new offset or
reintroducing an obsolete tombstone. Local history clearing and compaction
authorization remain application responsibilities: replicated compaction still
requires the authenticated exact-acknowledgement epoch, durable post-compaction
checkpoint, and retirement of obsolete deltas.

## Verification

Run the focused checks:

```sh
go test ./list ./xml ./text -count=1
go test -race ./list ./xml ./text -count=1
go test -run=^$ -fuzz=FuzzRGAUnmarshal -fuzztime=250000x -parallel=1 ./list
go test -run=^$ -fuzz=FuzzParseDocument -fuzztime=250000x -parallel=1 ./xml
go test -run='^$' -bench='BenchmarkRGAAppendIndexedList|BenchmarkRGAValuesTenThousand' -benchmem ./list
go test -run='^$' -bench='BenchmarkUndoManagerInsertUndoDiscardRedo' -benchmem ./text
```
