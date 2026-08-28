# BlockNote text-block rich-text binding

`bindBlockNoteRichText` connects BlockNote's default text blocks to the stable
`richtext` v1 CRDT (state/delta TypeIDs `23/24`). It is an adapter for
BlockNote's public `document`, `onChange`, and `replaceBlocks` API; the shipped
client has no BlockNote or Yjs production dependency.

The authenticated rich-text Manifest for this adapter must use the exact
renderer SchemaID `darkinno-tech:blocknote-text-v1`. This is not a Yjs binding and
must not share a group, state frame, or update log with BlockNote's Yjs
collaboration extension.

## Reversible scope

| BlockNote feature | `blocknote-text-v1` treatment |
| --- | --- |
| Paragraph, heading, bullet/numbered/check/toggle list, quote, code block | Preserved, including default props. |
| Nested default text blocks | Preserved as a bounded depth marker on each paragraph. |
| Bold, italic, underline, strike, code, text/background color | Preserved as explicit rich-text attributes. |
| Block background/text color and alignment; heading level/toggleability; checklist state; code language | Preserved in a canonical `rt.block` marker. |
| Links, tables, media/file blocks, embeds, custom blocks, custom props/styles, arbitrary inline nodes | Rejected before local CRDT mutation; never flattened to text. |

The adapter does not replicate BlockNote block IDs, selections, undo history,
menus, drag UI, upload state, or Yjs state. Block IDs remain local editor
identities; rich-text RGA positions are the replicated identities.

Use a separate, explicitly versioned `documenttree` group for tables, files,
media, custom block payloads, or content needing an independent replication
boundary. Do not put
their JSON or URLs into `rt.block` strings.

## Architecture and correctness

```text
BlockNote document/change
  -> bounded default-block validation and canonical projection
  -> atomic rich-text retain / delete / insert transaction
  -> one rich-text v1 frame
  -> authenticated durable outbox

authenticated rich-text frame
  -> Go/Wasm rich-text merge
  -> schema-checked spans
  -> BlockNote replaceBlocks (remote-write guard prevents echo)
```

The adapter compares runes without expanding every character into a JavaScript
object. It preserves shared text RGA positions where the block text is
unchanged, emitting retained formatting changes for a block/style update and a
delete/insert only for changed text. The Go core preflights the entire resulting
transaction before mutating live text or marks, so a rejected local block
replacement cannot create a delete-only CRDT state.

`rt.block` values are opaque to the wire protocol, so the host must authorize
this SchemaID and its fixed values before it calls `applyRemote`. A checksum
detects corruption; it does not authorize a peer, a schema, a link, or a block
payload. The binding validates the merged span projection before rendering, but
cannot undo a frame already admitted by an insufficient host receive policy.

## Use

```ts
import {
  bindBlockNoteRichText,
  BLOCKNOTE_RICH_TEXT_SCHEMA_ID,
  initRichTextWasm,
} from "@darkinno-tech/crdt-client";

// Authenticate a rich-text Manifest with SchemaID
// BLOCKNOTE_RICH_TEXT_SCHEMA_ID before creating this binding.
const runtime = await initRichTextWasm({ wasmURL: "/assets/crdt-rga.wasm" });
const document = runtime.create("browser-replica-7");

const binding = bindBlockNoteRichText(document, blockNoteEditor, {
  initialContent: "document", // normal join/recovery path
  onLocalFrame(frame) {
    // Persist the rich-text snapshot/frontier/HLC and durable outbox atomically.
    outbox.append(frame);
  },
});

socket.onmessage = ({ data }) => binding.applyRemote(new Uint8Array(data));
```

Use `initialContent: "editor"` only once when seeding a new group from a
validated local BlockNote document. A joining client must recover the CRDT
state/frame first and use `"document"`; otherwise it creates independent
concurrent block content.

The defaults cap one editor projection at 4,096 blocks, depth 64, 16,384 inline
text runs, 1 MiB UTF-8 text, and 262,144 Unicode scalars. Tighten the optional
binding limits to the authenticated group budget. The Go/Wasm runtime validates
its own local-operation, mark, frame, and received-frame limits again.

## Performance evidence

Run the controlled local benchmark:

```sh
make typescript-blocknote-benchmark
```

On the development machine used for this change (Darwin arm64, Node 26.5.0),
the five-sample 128-block / 32,896-rune workload measured these medians:

| Scenario | Local 256 edits | Remote 128 merges |
| --- | ---: | ---: |
| In-memory BlockNote-shaped port | 2.410 ms/edit | 1.258 ms/merge |
| Actual `@blocknote/core` editor API | 2.000 ms/edit | 5.053 ms/merge |

Both scenarios assert document convergence and exactly 256 local frames; remote
merges must not echo into the outbox. Heap deltas are diagnostic because V8 GC
varies. These are controlled development measurements, not browser paint,
mobile, network, TLS, persistence, or service-capacity guarantees. Use a Worker
or an application-level incremental editor port for documents whose full
projection cost would block the UI.

## Verification

```sh
make typescript-test
make wasm-test
make typescript-blocknote-benchmark
```

The adapter tests include nested default blocks, default props and inline
styles, no remote echo, table rejection, resource-limit rollback, and a real
`@blocknote/core` import/local-update/remote-render flow. `make wasm-test`
adds the actual Go/Wasm rich-text parser, frame decoder, duplicate/reordered
delivery, and snapshot-recovery gates.
