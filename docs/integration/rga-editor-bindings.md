# RGA plain-text editor bindings

`@darkinno/crdt-client/bindings` supplies a small, dependency-free bridge from
the negotiated Go/Wasm `RGAWasmDocument` to a text editor surface:

- `bindRGAPlainText` accepts a minimal read/write/observe port.
- `bindQuillPlainText` directly adapts Quill 2's `getText`, `setText`, and
  `text-change` APIs.
- `bindMonacoPlainText` adapts an `ITextModel`-shaped value surface.
- `bindProseMirrorPlainText` and `bindSlatePlainText` intentionally accept an
  application-supplied, schema-preserving text-leaf port rather than flattening
  blocks or marks into an unsafe string conversion.

```ts
const binding = bindQuillPlainText(document, quill, {
  initialContent: "editor", // explicit one-time import; default is document
  onLocalFrame(frame) {
    // Authenticate and bind the RGA run-v2 Manifest before durable outbox/send.
    outbox.append(frame);
  },
});

socket.onmessage = ({ data }) => binding.applyRemote(new Uint8Array(data));
// binding.destroy() only removes observers; it does not close document.
```

The binding finds the common Unicode-scalar prefix/suffix of an editor change
and emits one atomic RGA replacement frame. The changed text must fit the
runtime's negotiated byte/rune limit; an over-limit replacement is rejected
and the editor is restored to its last replicated text. This avoids a
delete-plus-later-insert sequence that could otherwise leave a local,
unreplicable delete-only state. Remote frames merge through the Wasm RGA
before the adapter replaces editor text; a write guard prevents that
replacement from echoing into `onLocalFrame`.

## Scope boundary

This is plain-text integration only. Quill's trailing newline is replicated as
text. Quill formatting/embeds, ProseMirror/Tiptap nodes, Slate elements,
HTML/CSS, selection presence, shared undo history, persistence, replay, TLS,
identity, authorization, and network transport remain application-owned.

Do not silently map a rich editor tree to plain text. For inline attributes,
use the separate stable `richtext.Document` protocol with its manifest,
policy, renderer validation, and atomic state/clock persistence. Block and
embed schemas require their own negotiated CRDT contract.

## Verification

The adapter unit tests use a deterministic editor port to cover Unicode split,
replacement, remote no-echo, lifecycle, and Quill source handling. The Wasm
integration test runs the same flow over real Go RGA frames. Run:

```sh
make typescript-test
make wasm-test
```
