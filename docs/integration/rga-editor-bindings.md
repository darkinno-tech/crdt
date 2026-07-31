# RGA plain-text editor bindings

`@darkinno/crdt-client/bindings` supplies a small, dependency-free bridge from
the negotiated Go/Wasm `RGAWasmDocument` to a text editor surface:

- `bindRGAPlainText` accepts a minimal read/write/observe port.
- `bindQuillPlainText` directly adapts Quill 2's `getText`, `setText`, and
  `text-change` APIs.
- `bindMonacoPlainText` adapts an `ITextModel`-shaped value surface.
- `bindCodeMirrorPlainText` directly adapts a CodeMirror 6 `EditorView`-shaped
  port. Its `applyViewUpdate()` is called from the host's configured update
  listener.
- `bindTiptapPlainText` directly accepts only Tiptap's canonical plain-text
  `doc` → `paragraph` → unmarked `text` JSON subset.
- `bindLexicalPlainText` accepts an application-owned Lexical text-leaf port,
  so the application keeps its approved root schema explicit.
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

CodeMirror 6 must have its listener extension at view construction. Keep the
binding variable outside the listener and forward all view updates; the
binding only consumes updates where `docChanged` is true:

```ts
let binding: CodeMirrorPlainTextBinding | undefined;
const updateListener = EditorView.updateListener.of((update) => {
  binding?.applyViewUpdate(update);
});
const view = new EditorView({
  state: EditorState.create({ doc: "", extensions: [updateListener] }),
  parent: element,
});
binding = bindCodeMirrorPlainText(document, view, {
  onLocalFrame(frame) {
    outbox.append(frame);
  },
});
```

Tiptap is direct only with its minimal `Document`, `Paragraph`, and `Text`
extensions. The binding reads canonical JSON rather than HTML and rejects a
mark, attribute, embed, hard break, or unknown node before it can be emitted:

```ts
const binding = bindTiptapPlainText(document, editor, {
  onLocalFrame(frame) {
    outbox.append(frame);
  },
});
```

For Lexical, construct a `LexicalTextPort` in the application. `readText()`
must read `$getRoot().getTextContent()` inside `editorState.read()`,
`replaceText()` must rewrite only the application's approved plain-text root
inside `editor.update()`, and `registerTextContentListener()` delegates to
Lexical's listener. This avoids treating a rich Lexical tree as an untyped
string transport.

The binding finds the common Unicode-scalar prefix/suffix of an editor change
and emits one atomic RGA replacement frame. The changed text must fit the
runtime's negotiated byte/rune limit; an over-limit replacement is rejected
and the editor is restored to its last replicated text. This avoids a
delete-plus-later-insert sequence that could otherwise leave a local,
unreplicable delete-only state. Remote frames merge through the Wasm RGA
before the adapter replaces editor text; a write guard prevents that
replacement from echoing into `onLocalFrame`.

## Position/Tag selections

`RGAWasmDocument.anchorAt(runeOffset)` and `resolveAnchor(anchor)` expose the
same RGA `Position`/Tag identity used by the Go `text.Anchor` API. An anchor
is `{ position?: { replicaID, wallTime, logical }, association: "before" |
"after" }`; omitting `position` represents the synthetic root (`before` =
start, `after` = end). This is a small local cursor model, not a Yjs
`RelativePosition` emulation and not a new RGA wire field.

`RGAPlainTextBinding.captureSelection()` returns two anchors for a capable
editor port, while `restoreSelection()` converts them back to the editor's
UTF-16 offsets. The Quill and CodeMirror adapters supply those optional ports
directly; CodeMirror also exposes the two methods on
`CodeMirrorPlainTextBinding`. On a remote merge the binding captures before
the frame, writes the merged text, then restores retained anchors. A malformed
selection or a compacted marker calls `onSelectionError` and leaves selection
handling to the host; it never guesses a nearby position.

Anchors are ephemeral application metadata. Do not put them in a state or
delta frame, IndexedDB outbox, snapshot, or unauthenticated peer message. A
presence protocol must authenticate and authorize the actor/document, validate
the bounded anchor shape, and clear a cursor when `anchor_gone` is returned.

## Scope boundary

This is plain-text integration only. Quill's trailing newline is replicated as
text. Tiptap accepts its narrow plain-text JSON subset, while Quill
formatting/embeds, non-plain Tiptap/ProseMirror nodes, Slate elements, Lexical
rich nodes, HTML/CSS, selection presence, shared undo history, persistence,
replay, TLS, identity, authorization, and network transport remain
application-owned.

Do not silently map a rich editor tree to plain text. For inline attributes,
use the separate stable `richtext.Document` protocol with its manifest,
policy, renderer validation, and atomic state/clock persistence. Block and
embed schemas require their own negotiated CRDT contract.

## Verification

The unit tests cover Unicode offsets, atomic replacement, rejection restore,
malformed Tiptap JSON, remote no-echo, lifecycle, and source handling. They
also instantiate CodeMirror 6, Tiptap 3, and Lexical headless editors, and
type-check the structural CodeMirror/Tiptap ports. The Wasm integration test
runs generic and CodeMirror-shaped flows over real Go RGA frames. Run:

```sh
make typescript-test
make wasm-test
make typescript-bindings-benchmark
make wasm-bindings-benchmark
```

The benchmark targets report controlled local-machine samples, not a
browser/device SLA. The simulated target isolates the full-text adapter work;
the Wasm target includes a 12,288-rune CodeMirror-port document, real Go
run-v2 replacement, and receiver application for each edit.
