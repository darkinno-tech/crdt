# Rich-text editor bindings

`@darkinno/crdt-client/bindings` can bind schema-specific Quill, BlockNote,
and Tiptap/ProseMirror surfaces to the manifest-bound `richtext` v1 protocol.
This is not an upgrade of a plain-text RGA binding: rich-text uses state/delta
TypeIDs `23/24`, semantic version `1`, its own renderer `SchemaID`, and an
atomic state/frontier/HLC persistence unit.

## Architecture and scope

```text
Quill Delta
  -> application RichTextAttributeCodec (approved schema only)
  -> RichTextEditorOperation retain / insert / delete
  -> Go/Wasm richtext.Document.ApplyEditorDelta
  -> one canonical rich-text v1 frame
  -> authenticated outbox / transport
```

The core first simulates the full editor transaction in an isolated RGA copy,
then preflights the complete text, mark, target, attribute, and frame budget.
Only after that preflight succeeds does it apply the nested text delta and
formatting operations to the real document. A rejected replacement therefore
cannot leave a local delete-only state.

Inline formatting preserves exact positions. The reserved `rt.block` marker
has one intentional editor adaptation: Quill stores a paragraph attribute on
its newline, while rich-text v1 requires every current position in a formatted
paragraph to carry one consistent marker. An editor transaction targeting a
newline with `rt.block` expands to that complete newline-delimited paragraph
inside the same canonical frame. This keeps `Document.Blocks()` truthful under
concurrent formatting instead of pretending that a newline-only mark formatted
the preceding text.

Embeds, links, block kinds, colors, custom marks, and nested editor nodes are
not guessed. The application explicitly maps each allowed value through a
codec selected by the authenticated rich-text Manifest `SchemaID`.

## Use the combined Go/Wasm runtime

Build the artifact with `make wasm`, load the matching `wasm_exec.js`, then
construct a separately negotiated rich-text runtime:

```ts
import {
  bindQuillRichText,
  initRichTextWasm,
} from "@darkinno/crdt-client";

const runtime = await initRichTextWasm({ wasmURL: "/assets/crdt-rga.wasm" });
const document = runtime.create("browser-replica-7");

const attributes = {
  toDocumentChanges(quill, operation) {
    const changes = [];
    if (quill.bold === true) changes.push({ key: "rt.bold", value: "true" });
    if (operation === "retain" && quill.bold === null) changes.push({ key: "rt.bold", remove: true });
    if (quill.header === 2) changes.push({ key: "rt.block", value: "heading:2" });
    return changes;
  },
  toEditorAttributes(crdt, text) {
    const result = {};
    if (crdt["rt.bold"] === "true") result.bold = true;
    // Quill owns block attributes on newlines only.
    if (crdt["rt.block"] === "heading:2" && text.endsWith("\n")) result.header = 2;
    return result;
  },
};

const binding = bindQuillRichText(document, quill, {
  initialContent: "editor", // imports Quill's required terminal newline once
  attributes,
  onLocalFrame(frame) {
    // Verify the rich-text Manifest, append to the durable outbox, then send.
    outbox.append(frame);
  },
});

socket.onmessage = ({ data }) => binding.applyRemote(new Uint8Array(data));
```

The ordinary `make wasm` artifact carries RGA run-v2 and needs no additional
option. If a separately authenticated deployment intentionally loads the
legacy combined RGA v1 artifact, pass its Manifest-selected RGA expectation as
`expectedRGAProtocol`; this validates the artifact's shared runtime without
changing the rich-text v1 frame contract.

Quill always has a terminal newline. It must be part of the rich-text CRDT
projection for every participant. Use `initialContent: "editor"` only for the
one-time import of a new Quill document. A joining client must first receive
the rich-text frame/snapshot and then bind with `initialContent: "document"`;
creating an unrelated local newline would be a concurrent document edit.

For BlockNote's default text-block document rather than a Quill Delta, use
[`bindBlockNoteRichText`](blocknote-richtext-bindings.md). It has its own exact
`darkinno:blocknote-text-v1` SchemaID, preserves a bounded default text-block
subset, and rejects tables, media, links, custom blocks, and unknown props
instead of flattening them into this rich-text group.

For Tiptap 3 and an application-owned ProseMirror port, use
[`bindTiptapRichText` / `bindProseMirrorRichText`](tiptap-richtext-bindings.md).
Its `darkinno:tiptap-core-richtext-v1` profile preserves approved blocks,
marks, hard breaks, and codec-validated inline atoms. It rejects lists,
tables, media, custom attrs/marks, block embeds, and NodeViews rather than
flattening them into the rich-text group.

## Safety and deployment boundary

- The browser runtime caps frames at 1 MiB, local inserted text at 64 KiB and
  16,384 Unicode scalars, local editor transactions at 512 operations, and
  attributes per operation at 32. Hosts should lower these to their document
  budget when appropriate.
- A checksum only detects accidental corruption. Authenticate the peer and
  bind state/delta IDs, semantic version, group, epoch, and the exact renderer
  schema before accepting a frame.
- Attribute values are opaque strings. Validate links, mentions, embeds, CSS,
  and block schemas in the codec, then sanitize at rendering boundaries. Do
  not treat attributes as HTML, authorization claims, or executable data.
- Persist `document.snapshot()` atomically. Its state, frontier, and clock are
  one unit; persisting the state without the HLC clock can reuse mutation IDs
  after a browser restart.
- `document.anchorAt` / `anchorRangeAt` capture stable RGA boundaries. Persist
  their `marshalAnchor` / `marshalAnchorRange` bytes only in a host-owned
  cursor, selection, or comment record bound to authenticated document ID,
  tenant/group, epoch, and actor. They never belong in `snapshot()` state, a
  CRDT delta, outbox, or unauthenticated presence payload. A reversed range is
  valid for selection direction; comment storage must require ordered resolved
  offsets. `anchor_gone` requires explicit reattachment, never nearest-offset
  guessing. The browser caps one encoded range at 128 KiB plus 64 bytes and a
  replica ID at 64 KiB; negotiate lower document-specific bounds in production.
- Presence transport, collaborative undo policy, annotation storage, transport
  retry, TLS, authorization, and durable outbox delivery are application-owned.

## Verification

```sh
go test ./richtext ./internal/wasm
go test -race ./richtext ./internal/wasm
make wasm-test
go test -run '^$' \
  -bench='BenchmarkRichText(ApplyEditorDeltaReviewTransaction|RuntimeApplyEditorDelta)$' \
  -benchmem -count=3 ./richtext ./internal/wasm
```

The Go tests cover atomic rejection, Quill newline-to-block expansion,
duplicate/reordered three-replica convergence, and snapshot recovery. The
Wasm integration test runs a real Go rich-text runtime through the TypeScript
Quill Delta binding, including approved inline marks, paragraph headers,
remote no-echo, and recovery. Benchmarks are controlled local regression
measurements; they do not establish browser, WAN, TLS, persistence, or
service-capacity latency.
