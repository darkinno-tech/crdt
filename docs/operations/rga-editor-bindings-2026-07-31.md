# RGA editor binding expansion assessment — 2026-07-31

## Decision

Add first-class plain-text entry points for CodeMirror 6, Tiptap 3, and
Lexical, bringing the supported named editor surfaces from four to seven.
Keep the package runtime dependency-free and do not claim Yjs compatibility.
Tiptap and Lexical rich documents remain outside this RGA string contract.

## Architecture and correctness

| Editor | Adapter boundary | Why |
| --- | --- | --- |
| CodeMirror 6 | `EditorView`-shaped port plus forwarded `ViewUpdate` | CodeMirror is a text editor; full-document dispatch avoids mixing its UTF-16 positions with RGA's Unicode-scalar positions. |
| Tiptap 3 | Canonical JSON containing only `doc` / `paragraph` / unmarked `text` | This is reversible to the RGA string. Marks, attributes, embeds, hard breaks, and arbitrary nodes are rejected rather than flattened. |
| Lexical | Application-provided plain-text leaf port | Lexical owns a state tree and update batching. The application must define the approved root schema before projecting text. |

Every local replacement now maps to one `RGAWasmDocument.replace()` frame.
The binding prechecks rune and UTF-8 byte limits, and restores the editor to
the last replicated projection if preflight fails. It deliberately rejects an
over-limit replacement; splitting delete plus later inserts could expose a
local delete-only state if an insertion fails. Applications needing a large
paste must split it into individually accepted editor transactions before
calling this binding.

The comparison/replace projection remains O(document runes) per editor change
because it computes the common prefix/suffix. This is correct across all
supported editor ports but is not a long-document incremental-operation
engine. A future optimization may consume editor-native incremental changes
only after proving Unicode offset conversion, composition handling, selection,
and atomic frame admission against the same test vectors.

## Security

- Incoming frame authentication, authorization, manifest/group/epoch binding,
  replay handling, transport body caps, and durable outbox receipts remain the
  host boundary; an editor callback is not a network trust boundary.
- Tiptap JSON is checked before text projection. Nulls, arrays, unknown nodes,
  schema attributes, malformed children, non-string text, and formatting
  marks return `unsupported_rich_text`; the editor is restored without a
  native exception or RGA mutation.
- The runtime's negotiated `maxLocalEditBytes` and `maxLocalEditRunes` are
  checked before mutation. This protects frame output and RGA node expansion;
  receiver limits are still enforced again by Go/Wasm.

## Verification

| Layer | Evidence |
| --- | --- |
| Deterministic simulation | 38 TypeScript tests: malformed Tiptap structures, atomic rejection restore, Unicode, local/remote no-echo, and lifecycle. |
| Real editor APIs | Actual CodeMirror 6.43.7, Tiptap 3.29.2, and Lexical 0.49.0 instances pass local update and remote no-echo flows; TypeScript checks direct CodeMirror/Tiptap port compatibility. |
| Real protocol | `make wasm-test`: 46 tests pass, including the new CodeMirror-shaped binding over a built Go 1.26.5 run-v2 Wasm artifact, duplicate/reordered delivery, snapshot recovery, and malformed-frame atomic rejection. |
| Simulated performance | 32,768-rune CodeMirror-port workload, 512 local + 256 remote one-rune replacements, five samples: median local **0.375 ms/edit**, median remote **0.205 ms/edit**. |
| Go/Wasm performance | 12,288-rune CodeMirror-port workload, 256 local replacements each immediately applied to a real receiver, five samples: median **1.227 ms/local-merge**; total emitted frames 27,089 bytes in the median sample. |

Measurements were taken on Darwin arm64 with Node v26.5.0 and Go 1.26.5.
Heap deltas are diagnostic only; they varied with V8 garbage collection. The
results are a controlled development baseline, not a production browser or
mobile SLA.

## Commands

```sh
make typescript-test
make wasm-test
make typescript-bindings-benchmark
make wasm-bindings-benchmark
```
