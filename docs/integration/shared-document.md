# Shared documents without CRDT plumbing

[简体中文](shared-document.zh-CN.md)

`shared.Document` is the smallest high-level Go entry point for a structured
collaborative document. It exposes named `Map` and `Array` objects in the same
spirit as Yjs shared types, while reusing this repository's stable,
`document/tree-v2` protocol.

It is deliberately not Yjs API or binary-update compatible. Use the separate
[Yjs relay](yjs-relay.md) when a browser or server already speaks Yjs.

## Start with the business object

For an in-process prototype, the API starts with a replica ID and named
objects. Local writes automatically create canonical update frames.

```go
doc, err := shared.New("editor-a")
if err != nil {
	return err
}

board, err := doc.Map("board") // Like Y.Doc.getMap("board").
if err != nil {
	return err
}
if err := board.SetString("title", "Release plan"); err != nil {
	return err
}

tasks, err := board.CreateArray("tasks")
if err != nil {
	return err
}
task, err := tasks.InsertMap(0)
if err != nil {
	return err
}
if err := task.SetJSON("task", map[string]any{"id": "release-notes", "done": false}); err != nil {
	return err
}
```

`Set`, `Get`, `SetString`, `String`, `SetJSON`, and `JSON` operate on copied
byte values. `CreateMap`, `CreateArray`, `InsertMap`, and `InsertArray` create
single-owner nested shared objects. A map or array child cannot be moved or
mounted twice, which keeps concurrent ownership deterministic.

Every reachable Map and Array uses one replication contract and is present in a
complete state/checkpoint frame. This facade deliberately has no descendant
`load`, `unload`, or external-document identifier: one authenticated
manifest/authorization boundary applies to the whole tree. Split content into
separately negotiated document groups before it needs independent access,
retention, or loading behavior.

You do not need to implement a CRDT algorithm, but you do need to select the
right product meaning:

- Concurrent writes to the same Map key use the document tree's LWW rule; one
  visible value wins deterministically.
- Array inserts retain a deterministic RGA order; deletions retain anchors for
  later offline updates.
- A CRDT cannot enforce an exclusive reservation, balance, access-control
  decision, or workflow transition. Keep those in an authoritative service.

The short comparison is:

| Collaboration task | Yjs | This Go facade |
| --- | --- | --- |
| Create a document | `new Y.Doc()` | `shared.New("editor-a")` |
| Get/create a named map | `doc.getMap("board")` | `doc.Map("board")` |
| Get/create a named array | `doc.getArray("tasks")` | `doc.Array("tasks")` |
| Listen for local frames | `doc.on("update", ...)` | `doc.OnUpdate(...)` |
| Apply a frame | `Y.applyUpdate(doc, update)` | `doc.ApplyUpdate(update)` |

Unlike JavaScript, Go has explicit errors. Check every returned error. The Go
facade emits one protocol frame for each successful mutating call; do not
assume that a sequence of calls is one transaction. Batch these independent
frames at the transport layer only when the authenticated provider's batching
contract preserves each frame's order-independent delivery semantics.

## Connect a real receiver safely

The one-argument constructor is convenient for a local prototype. A networked
group makes the frame budget explicit and sends local frames to its durable
outbox:

```go
limits := frame.DecoderLimits{
	MaxFrameBytes:  64 << 10,
	MaxPayload:     60 << 10,
	MaxCodecID:     128,
	MaxElements:    512,
	MaxTags:        512,
	MaxStringBytes: 1024,
}
options := shared.DefaultOptions()
options.FrameLimits = limits
doc, err := shared.NewWithOptions("editor-a", options)
if err != nil {
	return err
}

stop, err := doc.OnUpdate(func(update []byte) {
	// Append an owned canonical frame to the authenticated outbox. Do not do
	// blocking network I/O in this callback.
	outbox.Append(update)
})
if err != nil {
	return err
}
defer stop()
```

At the receiver, enforce the HTTP/WebSocket body limit before the bytes reach
Go, authenticate the peer, authorize the exact group/schema/value policy, then
call `ApplyUpdate`:

```go
if !authorized(peer, groupID, manifest) {
	return errUnauthorized
}
if len(body) > limits.MaxFrameBytes { // transport check before decode
	return errTooLarge
}
if err := doc.ApplyUpdate(body); err != nil {
	return fmt.Errorf("reject shared update: %w", err)
}
```

`ApplyUpdate` validates the complete document-tree frame within both the frame
and retained-state limits before changing document state. Duplicate and
parent-before-child frames are safe: valid dependencies are retained only
within the configured pending-work bounds. A checksum, a profile, or a
successful decode is never authentication or authorization.

The same `FrameLimits` bound outbound updates. A local operation that exceeds
that budget returns an output-frame limit error before it changes state or HLC
state, and it does not call `OnUpdate`. Choose the limits to accommodate every
single permitted local operation; keep individual values and delete ranges
within the negotiated update budget. A successful local frame is validated
once at that boundary and then handed to the outbox without a second
serialization pass.

## Persist before reusing an ID

For a restart-safe replica, atomically persist both checkpoint fields and the
host-owned frontier/outbox before the same `ReplicaID` writes again:

```go
checkpoint, err := doc.Checkpoint()
if err != nil {
	return err
}
// Store checkpoint.State, checkpoint.ClockState, the delivery frontier, and
// the outbox in one host transaction.

recovered, err := shared.Restore(checkpoint, options)
if err != nil {
	return err
}
```

Use the exact production `Options` on restore. `Checkpoint` has no
authentication or durable storage built in.

## Let people and tools inspect the contract

The facade always selects the stable `document/tree-v2` profile, so a setup
tool can expose the merge rule without copying TypeIDs:

```go
profile := shared.Profile()
fmt.Println(profile.ConflictRule)
```

For manifest construction, use the profile-aware helpers from the
[intent-first setup](intent-first-setup.md) after the host has selected and
authenticated its exact group/schema/epoch agreement. `shared.Document` does
not negotiate, authenticate, persist, encrypt, or authorize a collaboration
session on your behalf.

Run the complete duplicate/reordered example:

```sh
(cd examples && go run ./shared-document)
# title=Release plan
# task=release-notes
# done=false
# updates=5
```

For the underlying ownership rule, pending-work limits, recovery model, and
wire contract, read [document-tree v2 architecture](../design/document-tree-v2.md)
and [document-tree v2 protocol](../protocol/document-tree-v2.md).
