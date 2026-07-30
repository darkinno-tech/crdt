# Generic list, XML fragments, and local undo/redo

This document defines the first safe collaboration boundary for ordered
application values and XML-like documents. These APIs are experimental: a
replication group must advertise `crdt.ProtocolPolicy{AllowExperimental: true}`
and bind the exact state/delta IDs, codec ID, and semantics version in its
authenticated `replica.Manifest`.

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
extra top-level content. Rendering sorts attributes canonically and escapes
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

Undo history is process-local and intentionally not serialized. Call `Clear`
before compaction. If a retained predecessor has already been compacted,
undo/redo fails with `text.ErrUndoAnchorGone` instead of guessing a new offset
and changing user intent.

## Verification

Run the focused checks:

```sh
go test ./list ./xml ./text -count=1
go test -race ./list ./xml ./text -count=1
go test -run=^$ -fuzz=FuzzRGAUnmarshal -fuzztime=10s -parallel=1 ./list
go test -run=^$ -fuzz=FuzzParseDocument -fuzztime=10s -parallel=1 ./xml
go test -run='^$' -bench='BenchmarkRGAAppendIndexedList|BenchmarkRGAValuesTenThousand' -benchmem ./list
```
