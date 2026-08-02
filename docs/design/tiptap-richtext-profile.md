# Tiptap / ProseMirror rich-text profile

## Decision

Add `tiptap-core-richtext-v1` as a bounded adapter profile over the stable `richtext` v1 CRDT, without changing TypeIDs `23/24`, frame encoding, LWW mark resolution, snapshots, or RGA positions. The profile supplies direct Tiptap 3 binding plus an application-owned ProseMirror port, keeps the TypeScript client free of production Tiptap/ProseMirror dependencies, and admits inline embeds only through explicit bidirectional codecs.

This is intentionally not a claim that arbitrary Tiptap JSON is a CRDT wire format. Tiptap documents are schema-governed ProseMirror trees, and its JSON can include application-defined nodes, marks, and attrs. The adapter preserves one reversible subset; unknown values fail closed.

Official editor documentation supports this boundary: Tiptap models documents as schema-defined nodes and marks and exposes JSON as its persistence format ([Tiptap concepts](https://tiptap.dev/docs/editor/core-concepts/introduction)); Tiptap atomic nodes are treated as one view unit ([Node API](https://tiptap.dev/docs/editor/extensions/custom-extensions/create-new/node)); and ProseMirror schemas explicitly control valid nesting and attributes ([ProseMirror guide](https://prosemirror.net/docs/guide/)).

## Multi-dimensional assessment

| Dimension | Finding | Decision |
| --- | --- | --- |
| Correctness | `richtext` v1 is a linear RGA plus exact-position LWW attributes, whereas an editor document is a tree. Flattening arbitrary trees loses identity and nesting. | Preserve only a specified projection and use retain/delete/insert operations so unchanged RGA positions survive. Put trees and independently mutable objects in `documenttree`. |
| Security | Editor JSON can contain arbitrary nodes, attrs, links, NodeViews, and executable rendering behavior. A checksum cannot authorize any of them. | Require the exact Manifest Schema ID, reject unknown nodes/marks/attrs, and allow embeds only through codecs that validate both directions and canonical JSON. Rendering/sanitisation remains host-owned. |
| Resources | Large JSON, many marks, or embed payloads can expand local operation, frame, and renderer work. | Bound blocks (4,096), inline nodes (16,384), text and aggregate embed payloads (64 KiB), Unicode scalars (16,384), and editor operations (512) before CRDT mutation. Go/Wasm rechecks its independent limits. |
| Performance | Rebuilding an editor from each received span is a controlled full projection; cross-tree Step replay would be unsafe without a shared schema and transaction contract. | Diff the run projection without per-character arrays, retain unchanged RGA positions, and use a remote-write guard. Treat this profile as a bounded general-document path; high-frequency code editing stays on the incremental CodeMirror plain-text binding. |
| Ecosystem | Quill and BlockNote already have their own schema profiles, but Tiptap and ProseMirror previously only exposed plain text. | Add first-class Tiptap rich binding and a named ProseMirror port. Lexical and Slate retain application-owned ports until each has an equally explicit reversible schema profile. |

## Negotiation and reversible subset

The authenticated `replica.Manifest` for this adapter must use:

```text
state TypeID:      23
delta TypeID:      24
semantics version: 1
schema ID:         darkinno:tiptap-core-richtext-v1
```

It must not share a group, state frame, outbox, or log with raw RGA, Quill, or BlockNote schemas. A reader that has not negotiated this exact schema must reject its `rt.*` values before rendering.

| Tiptap source | Rich-text projection | Reverse projection |
| --- | --- | --- |
| `paragraph` | `rt.block=paragraph` on every current position and terminator | `paragraph` |
| `heading` level 1–6 | `rt.block=heading:N` | `heading` with the same level |
| one-paragraph `blockquote` | `rt.block=quote` | `blockquote > paragraph` |
| `codeBlock` | `rt.block=code`; internal LF is a private hard-break marker | `codeBlock` text |
| `bold`, `italic`, `underline`, `strike`, `code` with no attrs | `rt.bold`, `rt.italic`, `rt.underline`, `rt.strike`, `rt.code` = `true` | corresponding mark |
| `hardBreak` | `rt.tiptap.hard-break=true` on a single LF | `hardBreak` |
| configured inline atom | one U+FFFC with `rt.embed.kind` and `rt.embed.data` | the configured atomic node |

Lists, tables, task trees, arbitrary block attrs, links, media, block embeds, custom text marks, nested editors, NodeViews, decorations, editor IDs, selection, undo history, and ProseMirror plugin state are rejected. They are not flattened to text. Store a bounded object/reference in a separately negotiated `documenttree` group and place only its application-authorized reference in an inline embed when the product needs a visible placeholder.

## Embed security contract

`TiptapEmbedCodec` declares one exact local `nodeType` and one semantic `kind`. For a local atom, `encode()` must validate the node and return a JSON object; the binding sorts object keys recursively, rejects non-finite values and depth over 32, and bounds the canonical UTF-8 payload. For a received atom, `decode()` produces a node, after which the binding calls `encode()` again and requires byte-identical canonical JSON. This prevents a permissive decoder from turning a received embed into an unnegotiated local node.

The codec is schema validation, not authorization or HTML sanitisation. Hosts must authenticate the peer, bind group/epoch/schema/limits before decoding, authorize referenced resources, apply URL/content policies, and render untrusted values without treating them as markup or executable code.

The generic rich-text core cannot infer this editor-specific Profile from an otherwise authorized frame. Hosts must validate this Profile before `applyRemote`. If an invalid profile frame reaches the binding after merging, it restores the last safe editor projection and freezes; it cannot roll back the CRDT safely in the presence of concurrent work, so the host must recover the projection from an authenticated checkpoint.

## ProseMirror integration boundary

`bindProseMirrorRichText` accepts an explicit port rather than importing ProseMirror. Its `readJSON()` returns the approved schema's JSON; `replaceJSON()` must dispatch a tagged remote transaction; and `observeUpdate()` must not report that tagged transaction. This preserves the host's selection, plugin, and undo semantics and prevents a remote projection from re-entering the durable outbox. Do not mutate a dispatched transaction; the host should create its own tagged replacement transaction after applying local transaction policy.

## Validation matrix

- Deterministic projection tests cover valid blocks/marks/hard breaks/embeds, unknown node rejection, codec payload canonicality, aggregate resource limits, remote no-echo, invalid-remote binding freeze, and the ProseMirror transaction port.
- A real Tiptap 3 editor test uses actual Mark and atomic Node extensions.
- `make wasm-test` applies frames through the actual Go/Wasm `richtext` runtime, verifies TypeID `24`, remote no-echo, and snapshot recovery.
- `go test -race ./richtext ./internal/wasm` and rich-text decoder fuzzing protect the concurrent core and untrusted-frame paths; the adapter itself never bypasses Go/Wasm preflight.
- `make typescript-tiptap-richtext-benchmark` exercises actual Tiptap Core with marked prose and atomic embeds; it is a local controlled baseline, not browser paint, WAN, TLS, persistence, or capacity evidence.
