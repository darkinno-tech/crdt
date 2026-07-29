# Attachment reference integration

`attachment.Register` is the experimental CRDT boundary for images, audio,
video, and arbitrary binary data. It replicates bounded, immutable references;
it never transfers object bytes.

Use the [runnable attachment collaboration example](../../examples/attachment-collaboration)
for the complete text + attachment + manifest + snapshot + verification flow:

```sh
go run ./examples/attachment-collaboration
go test ./examples/attachment-collaboration
```

## What is replicated

| Field | Purpose | Security rule |
| --- | --- | --- |
| application key | Stable attachment location in one document | Bounded UTF-8, no controls or surrounding whitespace. |
| `ObjectID` | Opaque application object identifier | Never a signed URL, credential, personal datum, or raw media payload. |
| `MediaType` | Canonical MIME type, such as `image/png` or `audio/ogg` | Parameters and non-canonical forms are rejected. |
| `Size` | Expected object byte length | Bounded by `attachment.Options.MaxObjectBytes`. |
| `Digest` | SHA-256 of the exact object bytes | Verify before decode, preview, or render. |

Text belongs in `text.RGA`; use an attachment reference only for immutable
external object content. Mutable structured data should use the CRDT whose
conflict rule matches the product, such as `lww.Map`, OR-Set, or OR-Tree.

## Replication contract

Attachment references use experimental LWW-Map state/delta frame IDs 9/10.
One attachment group must have an authenticated `replica.Manifest` with:

| Manifest field | Required value |
| --- | --- |
| `StateID` / `DeltaID` | `crdt.TypeIDLWWMapState` / `crdt.TypeIDLWWMapDelta` |
| `SchemaID` | `github.com/DarkInno/crdt/attachment-reference/v1` |
| `CodecID` | empty string |
| `SemanticsVersion` | `attachment.SemanticsVersion` |
| policy | `crdt.ProtocolPolicy{AllowExperimental: true}` at every boundary |

Do not put editable RGA text and attachment references in one manifest: a
manifest represents one concrete CRDT protocol. Create separate text and
attachment groups for the same application document, as the runnable example
does.

At the transport boundary, authenticate the peer and exact manifest before
accepting any frame. Decode an attachment delta with
`attachment.UnmarshalDeltaWithLimits`, using both transport `frame.DecoderLimits`
and matching `attachment.Options`, then call `Register.ApplyDelta`. A valid
checksum does not authorize a sender or validate the object in storage.

## Create, deliver, recover, and verify

1. Upload or select immutable object bytes through the application's authorized
   storage path. Scan content and enforce storage quota before publishing a
   reference.
2. Compute SHA-256 and create `attachment.Reference`; use `Register.Put` to
   obtain a delta. Persist the local outbox/receipt record atomically with the
   CRDT snapshot and HLC state.
3. Send only the canonical delta frame under a persistent `replica.Dot`. The
   receiver uses `replica.Inbox` to preserve its contiguous delivery frontier.
4. Persist `Register.SnapshotCurrentState()` atomically and restore a same-ID
   replica with `attachment.NewFromSnapshotWithOptions`.
5. After an authorized download, call `Reference.Verify` before parsing or
   rendering the returned object. It streams with a fixed buffer, rejects short
   or oversized responses, and compares SHA-256 without retaining media bytes.

```go
file, err := os.Open(downloadedPath) // An authorized, bounded download target.
if err != nil {
	return err
}
defer file.Close()
if err := ref.Verify(file); err != nil {
	// Treat ErrContentMismatch as an untrusted/invalid object; do not decode it.
	return err
}
```

## Limits, deletes, and operations

Set `attachment.Options` per replication group. `MaxEntries` includes retained
delete metadata, so size the budget for the document lifetime rather than only
currently visible media. A delete is an LWW tombstone; do not silently erase it
while an offline replica can still send an older reference. The current library
does not provide attachment tombstone compaction: monitor retained entries and
establish a product-specific retention/recovery plan before adding one.

The CRDT library does not implement object storage, signed upload/download
URLs, identity, authorization, encryption, malware scanning, content policy,
or retry queues. Those are application responsibilities and must use the same
tenant/document authorization as the attachment key.

## Verification

```sh
go test ./attachment
go test -race ./attachment
go test -run=^$ -fuzz=FuzzUnmarshalDelta -fuzztime=10s ./attachment
go test -run=^$ -fuzz=FuzzReferenceVerify -fuzztime=10s ./attachment
go test ./examples/attachment-collaboration
```
