/**
 * Plain-text editor bindings for the negotiated Go/Wasm RGA protocol.
 *
 * These bindings intentionally do not translate HTML, ProseMirror nodes, Slate
 * elements, embeds, or formatting marks into text. Doing so would silently
 * discard collaborative structure. Use richtext.Document's separately
 * negotiated protocol for inline marks, and supply an application-owned block
 * schema before binding a rich document tree.
 */

import { CRDTRuntimeError, RGAAnchor, RGAWasmDocument } from "./wasm.js";

export type RGAFrameListener = (frame: Uint8Array) => void;

/** The minimal contract shared by textarea, Monaco, and host editor adapters. */
export interface PlainTextEditorPort {
  readText(): string;
  writeText(value: string): void;
  observeText(listener: () => void): () => void;
}

/** A UTF-16 selection as used by DOM, Quill, and CodeMirror editor APIs. */
export interface EditorTextSelection {
  readonly anchor: number;
  readonly head: number;
}

/** Optional editor capability for preserving a selection through remote RGA merges. */
export interface SelectionEditorPort extends PlainTextEditorPort {
  readSelection(): EditorTextSelection | undefined;
  writeSelection(selection: EditorTextSelection): void;
}

/** A stable RGA Position/Tag selection suitable for local editor state or authenticated presence. */
export interface RGASelection {
  readonly anchor: RGAAnchor;
  readonly head: RGAAnchor;
}

export interface BindRGAPlainTextOptions {
  /** Receives exact RGA frames for an authenticated, durable outbox. */
  readonly onLocalFrame: RGAFrameListener;
  /** Import an existing editor value as local CRDT edits instead of overwriting it. */
  readonly initialContent?: "document" | "editor";
  /** Reports a malformed or compacted local selection without blocking a remote document merge. */
  readonly onSelectionError?: (error: unknown) => void;
}

/**
 * Synchronizes one plain-text editor port with a local RGA runtime. Local
 * edits use Unicode scalar offsets, are split under the negotiated runtime
 * limits, and emit ordinary canonical RGA frames. Remote frames are applied
 * before replacing the port value and cannot echo back into the outbox.
 */
export class RGAPlainTextBinding {
  readonly #projection: IncrementalTextProjection;
  #writing = false;
  #closed = false;
  readonly #unobserve: () => void;

  constructor(
    readonly document: RGAWasmDocument,
    private readonly editor: PlainTextEditorPort,
    private readonly options: BindRGAPlainTextOptions,
  ) {
    if (typeof options.onLocalFrame !== "function") {
      throw new CRDTRuntimeError("invalid_binding_options");
    }
    this.#projection = new IncrementalTextProjection(document.text());
    this.#unobserve = editor.observeText(() => this.#handleEditorChange());
    if (options.initialContent === "editor") {
      this.#replaceDocument(editor.readText());
    } else {
      this.#writeEditor(this.#projection.text());
    }
  }

  /** Applies one authenticated, manifest-checked remote frame. */
  applyRemote(frame: Uint8Array): void {
    this.#assertOpen();
    const selection = this.#captureSelectionSafely();
    this.document.applyDelta(frame);
    this.#projection.reset(this.document.text());
    this.#writeEditor(this.#projection.text());
    if (selection !== undefined) {
      this.#restoreSelectionSafely(selection);
    }
  }

  /** Captures the editor's current UTF-16 selection as stable RGA Position/Tag anchors. */
  captureSelection(): RGASelection | undefined {
    this.#assertOpen();
    const port = selectionPort(this.editor);
    if (port === undefined) {
      return undefined;
    }
    const selection = port.readSelection();
    if (selection === undefined) {
      return undefined;
    }
    return {
      anchor: this.document.anchorAt(this.#projection.runeOffsetAtUTF16(selection.anchor)),
      head: this.document.anchorAt(this.#projection.runeOffsetAtUTF16(selection.head)),
    };
  }

  /** Restores a retained RGA Position/Tag selection to the editor's UTF-16 offsets. */
  restoreSelection(selection: RGASelection): boolean {
    this.#assertOpen();
    const port = selectionPort(this.editor);
    if (port === undefined) {
      return false;
    }
    if (!isRGASelection(selection)) {
      throw new CRDTRuntimeError("invalid_anchor");
    }
    port.writeSelection({
      anchor: this.#projection.utf16OffsetAtRune(this.document.resolveAnchor(selection.anchor)),
      head: this.#projection.utf16OffsetAtRune(this.document.resolveAnchor(selection.head)),
    });
    return true;
  }

  /** Stops observation; it never closes the caller-owned RGA document. */
  destroy(): boolean {
    if (this.#closed) {
      return false;
    }
    this.#closed = true;
    this.#unobserve();
    return true;
  }

  #handleEditorChange(): void {
    if (this.#closed || this.#writing) {
      return;
    }
    try {
      this.#replaceDocument(this.editor.readText());
    } catch (error) {
      // The editor has already accepted this local input, while the RGA may
      // have rejected it during frame/state preflight. Restore the last
      // replicated projection so a rejected edit cannot remain as an
      // unreplicable editor-only fork.
      this.#writeEditor(this.#projection.text());
      throw error;
    }
  }

  /**
   * Applies one editor-native UTF-16 replacement without re-reading or
   * comparing the full editor projection. It is used only when an adapter can
   * prove that its update contains exactly one coherent replacement range.
   */
  applyUTF16Replacement(change: EditorUTF16Replacement): void {
    if (this.#closed || this.#writing) {
      return;
    }
    try {
      const replacement = this.#projection.prepareReplacement(change.from, change.to, change.insert);
      if (change.newLength !== this.#projection.utf16Length() - (change.to - change.from) + change.insert.length) {
        this.#handleEditorChange();
        return;
      }
      if (replacement.isNoop) {
        return;
      }
      assertBoundedReplacement(change.insert, this.document);
      const frame = this.document.replace(
        replacement.runeFrom,
        replacement.runeTo - replacement.runeFrom,
        change.insert,
      );
      this.#projection.commitReplacement(replacement);
      this.options.onLocalFrame(frame);
    } catch (error) {
      // The editor already accepted this transaction. Restore the last
      // replicated projection if its corresponding local RGA replacement is
      // rejected, so no editor-only fork remains visible.
      this.#writeEditor(this.#projection.text());
      throw error;
    }
  }

  #replaceDocument(next: string): void {
    this.#assertOpen();
    const previousText = this.#projection.text();
    if (next === previousText) {
      return;
    }
    const previousRunes = Array.from(previousText);
    const nextRunes = Array.from(next);
    let prefix = 0;
    while (prefix < previousRunes.length && prefix < nextRunes.length && previousRunes[prefix] === nextRunes[prefix]) {
      prefix += 1;
    }
    let previousEnd = previousRunes.length;
    let nextEnd = nextRunes.length;
    while (previousEnd > prefix && nextEnd > prefix && previousRunes[previousEnd - 1] === nextRunes[nextEnd - 1]) {
      previousEnd -= 1;
      nextEnd -= 1;
    }

    const replacement = nextRunes.slice(prefix, nextEnd).join("");
    assertBoundedReplacement(replacement, this.document);
    const frame = this.document.replace(prefix, previousEnd - prefix, replacement);
    const committed = this.document.text();
    if (committed !== next) {
      throw new CRDTRuntimeError("binding_diverged");
    }
    this.#projection.reset(committed);
    this.options.onLocalFrame(frame);
  }

  #writeEditor(value: string): void {
    this.#writing = true;
    try {
      let current: string | undefined;
      try {
        current = this.editor.readText();
      } catch {
        // A schema-validating editor adapter can reject the just-entered rich
        // value. It still needs a route back to the last valid RGA text.
        current = undefined;
      }
      if (current !== value) {
        this.editor.writeText(value);
      }
    } finally {
      this.#writing = false;
    }
  }

  #captureSelectionSafely(): RGASelection | undefined {
    try {
      return this.captureSelection();
    } catch (error) {
      this.options.onSelectionError?.(error);
      return undefined;
    }
  }

  #restoreSelectionSafely(selection: RGASelection): void {
    try {
      this.restoreSelection(selection);
    } catch (error) {
      // A compacted marker must clear or refresh the cursor instead of
      // guessing a nearby editor offset. The document merge remains valid.
      this.options.onSelectionError?.(error);
    }
  }

  #assertOpen(): void {
    if (this.#closed) {
      throw new CRDTRuntimeError("document_closed");
    }
  }
}

export function bindRGAPlainText(
  document: RGAWasmDocument,
  editor: PlainTextEditorPort,
  options: BindRGAPlainTextOptions,
): RGAPlainTextBinding {
  return new RGAPlainTextBinding(document, editor, options);
}

/** Structural type for Quill 2 without adding Quill as a package dependency. */
export interface QuillTextPort {
  getText(): string;
  setText(value: string, source?: "api" | "silent" | "user"): unknown;
  getSelection?(focus?: boolean): { readonly index: number; readonly length: number } | null;
  setSelection?(index: number, length: number, source?: "api" | "silent" | "user"): unknown;
  on(event: "text-change", listener: (delta: unknown, oldContents: unknown, source: string) => void): unknown;
  off?(event: "text-change", listener: (delta: unknown, oldContents: unknown, source: string) => void): unknown;
}

/**
 * Binds Quill's text surface. Quill's mandatory trailing newline remains part
 * of the RGA content. Formatting and embeds are intentionally out of scope.
 */
export function bindQuillPlainText(
  document: RGAWasmDocument,
  quill: QuillTextPort,
  options: BindRGAPlainTextOptions,
): RGAPlainTextBinding {
  const port: SelectionEditorPort = {
    readText: () => quill.getText(),
    writeText: (value) => {
      quill.setText(value, "api");
    },
    observeText: (listener) => {
      const handler = (_delta: unknown, _old: unknown, source: string) => {
        if (source === "user") {
          listener();
        }
      };
      quill.on("text-change", handler);
      return () => quill.off?.("text-change", handler);
    },
    readSelection: () => {
      const selection = quill.getSelection?.();
      return selection === null || selection === undefined
        ? undefined
        : { anchor: selection.index, head: selection.index + selection.length };
    },
    writeSelection: (selection: EditorTextSelection) => {
      if (quill.setSelection !== undefined) {
        quill.setSelection(selection.anchor, selection.head - selection.anchor, "api");
      }
    },
  };
  return bindRGAPlainText(document, port, options);
}

/** Structural type for a Monaco ITextModel without a Monaco dependency. */
export interface MonacoTextPort {
  getValue(): string;
  setValue(value: string): void;
  onDidChangeContent(listener: () => void): { dispose(): void };
}

/** Binds a Monaco text model; the host owns transport, awareness, and cursors. */
export function bindMonacoPlainText(
  document: RGAWasmDocument,
  model: MonacoTextPort,
  options: BindRGAPlainTextOptions,
): RGAPlainTextBinding {
  return bindRGAPlainText(document, {
    readText: () => model.getValue(),
    writeText: (value) => model.setValue(value),
    observeText: (listener) => {
      const subscription = model.onDidChangeContent(listener);
      return () => subscription.dispose();
    },
  }, options);
}

/** The subset of a CodeMirror 6 `EditorView` used by this binding. */
export interface CodeMirrorTextPort {
  readonly state: {
    readonly doc: {
      readonly length: number;
      toString(): string;
    };
    readonly selection?: {
      readonly main: EditorTextSelection;
    };
  };
  dispatch(spec: {
    readonly changes?: {
      readonly from: number;
      readonly to: number;
      readonly insert: string;
    };
    readonly selection?: EditorTextSelection;
  }): void;
}

/** One CodeMirror change set, retained structurally to avoid a runtime dependency. */
export interface CodeMirrorChangeSet {
  iterChanges(listener: (
    fromA: number,
    toA: number,
    fromB: number,
    toB: number,
    inserted: { toString(): string },
  ) => void): void;
}

/** The CodeMirror update detail used by the plain-text binding. */
export interface CodeMirrorViewUpdate {
  readonly docChanged: boolean;
  /** The editor-native change coordinates; absent for backwards-compatible ports. */
  readonly changes?: CodeMirrorChangeSet;
}

/**
 * Binds a CodeMirror 6 view without taking a runtime dependency on
 * `@codemirror/view`. Configure the host's `EditorView.updateListener` to
 * call `applyViewUpdate(update)` for every view update (see the integration
 * guide). Single-range CodeMirror transactions use their native UTF-16 change
 * coordinates directly. Multi-range and legacy structural updates retain the
 * full-document atomic fallback, rather than publishing a partial transaction.
 */
export class CodeMirrorPlainTextBinding {
  #listener: (() => void) | undefined;
  readonly #binding: RGAPlainTextBinding;

  constructor(
    document: RGAWasmDocument,
    private readonly view: CodeMirrorTextPort,
    options: BindRGAPlainTextOptions,
  ) {
    const port: SelectionEditorPort = {
      readText: () => view.state.doc.toString(),
      writeText: (value) => {
        view.dispatch({
          changes: {
            from: 0,
            to: view.state.doc.length,
            insert: value,
          },
        });
      },
      observeText: (listener) => {
        this.#listener = listener;
        return () => {
          if (this.#listener === listener) {
            this.#listener = undefined;
          }
        };
      },
      readSelection: () => view.state.selection?.main,
      writeSelection: (selection: EditorTextSelection) => view.dispatch({ selection }),
    };
    this.#binding = bindRGAPlainText(document, port, options);
  }

  /** Pass every `EditorView.updateListener` event through this method. */
  applyViewUpdate(update: CodeMirrorViewUpdate): void {
    if (!update.docChanged) {
      return;
    }
    const change = singleCodeMirrorReplacement(update, this.view.state.doc.length);
    if (change !== undefined) {
      this.#binding.applyUTF16Replacement(change);
    } else {
      this.#listener?.();
    }
  }

  applyRemote(frame: Uint8Array): void {
    this.#binding.applyRemote(frame);
  }

  /** Captures CodeMirror's selection as stable RGA Position/Tag anchors. */
  captureSelection(): RGASelection | undefined {
    return this.#binding.captureSelection();
  }

  /** Restores retained RGA Position/Tag anchors to CodeMirror's UTF-16 selection. */
  restoreSelection(selection: RGASelection): boolean {
    return this.#binding.restoreSelection(selection);
  }

  destroy(): boolean {
    return this.#binding.destroy();
  }
}

export function bindCodeMirrorPlainText(
  document: RGAWasmDocument,
  view: CodeMirrorTextPort,
  options: BindRGAPlainTextOptions,
): CodeMirrorPlainTextBinding {
  return new CodeMirrorPlainTextBinding(document, view, options);
}

/** JSON shape shared by Tiptap's `Editor#getJSON()` and `setContent()`. */
export interface TiptapJSONNode {
  readonly type?: string;
  readonly text?: string;
  readonly content?: readonly TiptapJSONNode[];
  readonly [key: string]: unknown;
}

/** Minimal Tiptap editor contract; no `@tiptap/*` package is bundled here. */
export interface TiptapTextPort {
  getJSON(): TiptapJSONNode;
  readonly commands: {
    /** Tiptap's concrete content union stays application-owned. */
    setContent(content: never, options: {
      readonly emitUpdate: boolean;
      readonly errorOnInvalidContent: boolean;
    }): boolean;
  };
  on(event: "update", listener: (event: unknown) => void): unknown;
  off?(event: "update", listener: (event: unknown) => void): unknown;
}

/**
 * Binds the explicit plain-text subset of a Tiptap document. Only a `doc`
 * containing unmarked `paragraph` and `text` nodes is accepted; marks,
 * embeds, attributes, and other node types are rejected and restored instead
 * of being silently discarded. Use `richtext.Document` for rich Tiptap data.
 */
export function bindTiptapPlainText(
  document: RGAWasmDocument,
  editor: TiptapTextPort,
  options: BindRGAPlainTextOptions,
): RGAPlainTextBinding {
  return bindRGAPlainText(document, {
    readText: () => textFromTiptapJSON(editor.getJSON()),
    writeText: (value) => {
      const accepted = editor.commands.setContent(tiptapJSONFromText(value) as never, {
        emitUpdate: false,
        errorOnInvalidContent: true,
      });
      if (accepted === false) {
        throw new CRDTRuntimeError("binding_write_failed");
      }
    },
    observeText: (listener) => {
      const handler = (_event: unknown) => listener();
      editor.on("update", handler);
      return () => editor.off?.("update", handler);
    },
  }, options);
}

/**
 * Application-provided Lexical text leaf adapter. `replaceText` must update
 * the caller's approved plain-text root schema, and `readText` must read the
 * same canonical leaf projection. This keeps Lexical nodes/marks out of an
 * RGA text document instead of flattening a rich tree without agreement.
 */
export interface LexicalTextPort {
  readText(): string;
  replaceText(value: string): void;
  registerTextContentListener(listener: (textContent: string) => void): () => void;
}

export function bindLexicalPlainText(
  document: RGAWasmDocument,
  editor: LexicalTextPort,
  options: BindRGAPlainTextOptions,
): RGAPlainTextBinding {
  return bindRGAPlainText(document, {
    readText: () => editor.readText(),
    writeText: (value) => editor.replaceText(value),
    observeText: (listener) => editor.registerTextContentListener(() => listener()),
  }, options);
}

/**
 * ProseMirror/Tiptap and Slate applications should expose their already
 * schema-preserving text leaf adapter through this port. This alias keeps the
 * binding dependency-free while making the boundary explicit: a whole-document
 * string replacement is not a valid rich-document conversion.
 */
export function bindProseMirrorPlainText(
  document: RGAWasmDocument,
  port: PlainTextEditorPort,
  options: BindRGAPlainTextOptions,
): RGAPlainTextBinding {
  return bindRGAPlainText(document, port, options);
}

/** See bindProseMirrorPlainText for the required schema-preserving adapter. */
export function bindSlatePlainText(
  document: RGAWasmDocument,
  port: PlainTextEditorPort,
  options: BindRGAPlainTextOptions,
): RGAPlainTextBinding {
  return bindRGAPlainText(document, port, options);
}

/** One renderable rich-text span from the separately negotiated richtext v1 CRDT. */
export interface RichTextSpan {
  readonly text: string;
  readonly attributes?: Readonly<Record<string, string>>;
}

/** One named rich-text attribute assignment or removal. */
export interface RichTextAttributeChange {
  readonly key: string;
  readonly value?: string;
  readonly remove?: boolean;
}

/**
 * The small, offset-based editor transaction accepted by richtext.Document.
 * Every operation has exactly one of retain, delete, or insert. Offsets use
 * Unicode scalar values, so a backend must not pass raw UTF-16 positions.
 */
export interface RichTextEditorOperation {
  readonly retain?: number;
  readonly delete?: number;
  readonly insert?: string;
  readonly changes?: readonly RichTextAttributeChange[];
}

/**
 * The browser-facing rich-text runtime boundary. It is intentionally separate
 * from RGAWasmDocument: rich-text v1 uses its own TypeIDs, manifest schema,
 * atomic state/clock persistence unit, and frame decoder.
 */
export interface RichTextEditorDocument {
  spans(): readonly RichTextSpan[];
  applyEditorDelta(operations: readonly RichTextEditorOperation[]): Uint8Array;
  applyDelta(frame: Uint8Array): void;
}

/** Quill's public Delta shape, kept structural to avoid a Quill dependency. */
export interface QuillRichTextDelta {
  readonly ops?: readonly QuillRichTextDeltaOperation[];
}

export interface QuillRichTextDeltaOperation {
  readonly retain?: number;
  readonly delete?: number;
  readonly insert?: unknown;
  readonly attributes?: Readonly<Record<string, unknown>>;
}

/** The subset of Quill 2 used by the rich-text delta binding. */
export interface QuillRichTextPort {
  getContents(): QuillRichTextDelta;
  setContents(value: QuillRichTextDelta, source?: "api" | "silent" | "user"): unknown;
  on(event: "text-change", listener: (delta: QuillRichTextDelta, oldContents: QuillRichTextDelta, source: string) => void): unknown;
  off?(event: "text-change", listener: (delta: QuillRichTextDelta, oldContents: QuillRichTextDelta, source: string) => void): unknown;
}

/**
 * Converts only an application-approved Quill attribute schema. The binding
 * never guesses how a mark, link, block, mention, or embed should be mapped.
 * The same codec must be selected by the rich-text Manifest SchemaID.
 */
export interface RichTextAttributeCodec {
  toDocumentChanges(
    attributes: Readonly<Record<string, unknown>>,
    operation: "insert" | "retain",
  ): readonly RichTextAttributeChange[];
  toEditorAttributes(
    attributes: Readonly<Record<string, string>>,
    text: string,
  ): Readonly<Record<string, unknown>> | undefined;
}

export interface BindQuillRichTextOptions {
  /** Receives one atomic rich-text frame per accepted Quill transaction. */
  readonly onLocalFrame: (frame: Uint8Array) => void;
  /** The manifest-selected formatter/schema conversion. */
  readonly attributes: RichTextAttributeCodec;
  /** Import Quill's initial Delta once; otherwise render the document projection. */
  readonly initialContent?: "document" | "editor";
}

/**
 * Binds Quill's Delta API to an atomic rich-text v1 document transaction.
 * Inline attributes are preserved through the mandatory codec. Unsupported
 * embeds and non-string inserts are rejected and the last replicated
 * projection is restored instead of flattening them into text.
 *
 * Quill requires a terminal newline. Every participating rich-text document
 * must therefore include that newline in its CRDT projection; use
 * initialContent: "editor" when importing a new Quill document.
 */
export class QuillRichTextBinding {
  #writing = false;
  #closed = false;
  readonly #unobserve: () => void;

  constructor(
    readonly document: RichTextEditorDocument,
    private readonly quill: QuillRichTextPort,
    private readonly options: BindQuillRichTextOptions,
  ) {
    if (typeof options.onLocalFrame !== "function" || options.attributes === undefined ||
      typeof options.attributes.toDocumentChanges !== "function" || typeof options.attributes.toEditorAttributes !== "function") {
      throw new CRDTRuntimeError("invalid_binding_options");
    }
    const handler = (delta: QuillRichTextDelta, _old: QuillRichTextDelta, source: string) => {
      if (source === "user") {
        this.#handleLocalDelta(delta);
      }
    };
    quill.on("text-change", handler);
    this.#unobserve = () => quill.off?.("text-change", handler);
    if (options.initialContent === "document") {
      this.#writeProjection();
    } else {
      this.#importEditor();
    }
  }

  /** Applies one authenticated, manifest-checked remote rich-text frame. */
  applyRemote(frame: Uint8Array): void {
    this.#assertOpen();
    this.document.applyDelta(frame);
    this.#writeProjection();
  }

  /** Stops observation; it never closes the caller-owned rich-text document. */
  destroy(): boolean {
    if (this.#closed) {
      return false;
    }
    this.#closed = true;
    this.#unobserve();
    return true;
  }

  #importEditor(): void {
    try {
      const frame = this.document.applyEditorDelta(quillDeltaToDocumentOperations(this.quill.getContents(), this.options.attributes));
      this.#writeProjection();
      this.options.onLocalFrame(frame);
    } catch (error) {
      this.#writeProjection();
      throw error;
    }
  }

  #handleLocalDelta(delta: QuillRichTextDelta): void {
    if (this.#closed || this.#writing) {
      return;
    }
    try {
      const frame = this.document.applyEditorDelta(quillDeltaToDocumentOperations(delta, this.options.attributes));
      this.#writeProjection();
      this.options.onLocalFrame(frame);
    } catch (error) {
      // ApplyEditorDelta is atomic. Re-render the authoritative projection so
      // unsupported or over-limit local input cannot remain editor-only.
      this.#writeProjection();
      throw error;
    }
  }

  #writeProjection(): void {
    const projection = quillDeltaFromSpans(this.document.spans(), this.options.attributes);
    if (!hasTerminalNewline(projection)) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    this.#writing = true;
    try {
      this.quill.setContents(projection, "api");
    } finally {
      this.#writing = false;
    }
  }

  #assertOpen(): void {
    if (this.#closed) {
      throw new CRDTRuntimeError("document_closed");
    }
  }
}

export function bindQuillRichText(
  document: RichTextEditorDocument,
  quill: QuillRichTextPort,
  options: BindQuillRichTextOptions,
): QuillRichTextBinding {
  return new QuillRichTextBinding(document, quill, options);
}

// Kept on the editor-binding entrypoint so consumers do not need BlockNote as
// a production dependency merely to import the structural adapter contract.
export {
  bindBlockNoteRichText,
  BlockNoteRichTextBinding,
  BLOCKNOTE_RICH_TEXT_SCHEMA_ID,
} from "./blocknote.js";
export type {
  BindBlockNoteRichTextOptions,
  BlockNoteRichTextPort,
} from "./blocknote.js";

// Kept on the editor-binding entrypoint so a consumer can use a structural
// Tiptap / ProseMirror port without importing Tiptap itself at runtime.
export {
  bindProseMirrorRichText,
  bindTiptapRichText,
  TiptapRichTextBinding,
  TIPTAP_CORE_RICH_TEXT_SCHEMA_ID,
} from "./tiptap.js";
export type {
  BindTiptapRichTextOptions,
  ProseMirrorRichTextPort,
  TiptapEmbedCodec,
  TiptapEmbedArray,
  TiptapEmbedObject,
  TiptapEmbedValue,
  TiptapRichTextPort,
} from "./tiptap.js";

function quillDeltaToDocumentOperations(delta: QuillRichTextDelta, codec: RichTextAttributeCodec): RichTextEditorOperation[] {
  if (!isRecord(delta) || (delta.ops !== undefined && !Array.isArray(delta.ops))) {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  const operations: RichTextEditorOperation[] = [];
  for (const operation of delta.ops ?? []) {
    if (!isRecord(operation)) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    const attributes = quillAttributes(operation.attributes);
    const actionCount = Number(typeof operation.retain === "number" && operation.retain > 0)
      + Number(typeof operation.delete === "number" && operation.delete > 0)
      + Number(typeof operation.insert === "string" && operation.insert.length > 0);
    if (actionCount !== 1 || !validRichTextLength(operation.retain) || !validRichTextLength(operation.delete)) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    if (typeof operation.insert === "string" && operation.insert.length > 0) {
      operations.push({ insert: operation.insert, changes: codec.toDocumentChanges(attributes, "insert") });
    } else if (typeof operation.retain === "number" && operation.retain > 0) {
      operations.push({ retain: operation.retain, changes: codec.toDocumentChanges(attributes, "retain") });
    } else if (typeof operation.delete === "number") {
      if (Object.keys(attributes).length !== 0) {
        throw new CRDTRuntimeError("unsupported_rich_text");
      }
      operations.push({ delete: operation.delete });
    } else {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
  }
  return operations;
}

function quillDeltaFromSpans(spans: readonly RichTextSpan[], codec: RichTextAttributeCodec): QuillRichTextDelta {
  return {
    ops: spans.flatMap((span) => richTextSpanToQuillOperations(span, codec)),
  };
}

function richTextSpanToQuillOperations(span: RichTextSpan, codec: RichTextAttributeCodec): QuillRichTextDeltaOperation[] {
  if (typeof span.text !== "string" || span.text.length === 0) {
    return [];
  }
  const sourceAttributes = span.attributes ?? {};
  const fragments = sourceAttributes["rt.block"] === undefined ? [span.text] : splitAfterNewlines(span.text);
  return fragments.map((text) => {
    const attributes = codec.toEditorAttributes(sourceAttributes, text);
    return attributes === undefined || Object.keys(attributes).length === 0 ? { insert: text } : { insert: text, attributes };
  });
}

function splitAfterNewlines(value: string): string[] {
  const fragments: string[] = [];
  let start = 0;
  for (let index = 0; index < value.length; index += 1) {
    if (value[index] !== "\n") {
      continue;
    }
    fragments.push(value.slice(start, index + 1));
    start = index + 1;
  }
  if (start < value.length) {
    fragments.push(value.slice(start));
  }
  return fragments;
}

function hasTerminalNewline(delta: QuillRichTextDelta): boolean {
  const last = delta.ops?.at(-1);
  return typeof last?.insert === "string" && last.insert.endsWith("\n");
}

function quillAttributes(value: unknown): Readonly<Record<string, unknown>> {
  if (value === undefined) {
    return {};
  }
  if (!isRecord(value)) {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  return value;
}

function validRichTextLength(value: unknown): boolean {
  return value === undefined || (typeof value === "number" && Number.isSafeInteger(value) && value > 0);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function textFromTiptapJSON(value: TiptapJSONNode): string {
  if (!hasOnlyKeys(value, ["type", "content"]) || value.type !== "doc") {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  const paragraphs = value.content ?? [];
  if (!Array.isArray(paragraphs)) {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  return paragraphs.map((paragraph) => {
    if (!hasOnlyKeys(paragraph, ["type", "content"]) || paragraph.type !== "paragraph") {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    const textNodes = paragraph.content ?? [];
    if (!Array.isArray(textNodes)) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    return textNodes.map((text) => {
      if (!hasOnlyKeys(text, ["type", "text"]) || text.type !== "text" || typeof text.text !== "string") {
        throw new CRDTRuntimeError("unsupported_rich_text");
      }
      return text.text;
    }).join("");
  }).join("\n");
}

function tiptapJSONFromText(value: string): TiptapJSONNode {
  return {
    type: "doc",
    content: value.split("\n").map((paragraph) => ({
      type: "paragraph",
      content: paragraph === "" ? [] : [{ type: "text", text: paragraph }],
    })),
  };
}

function hasOnlyKeys(value: unknown, allowed: readonly string[]): value is TiptapJSONNode {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    && Object.keys(value).every((key) => allowed.includes(key));
}

/** One coherent native editor replacement expressed in UTF-16 offsets. */
export interface EditorUTF16Replacement {
  readonly from: number;
  readonly to: number;
  readonly insert: string;
  readonly newLength: number;
}

interface PreparedProjectionReplacement {
  readonly runeFrom: number;
  readonly runeTo: number;
  readonly startChunk: number;
  readonly removedChunks: number;
  readonly replacement: readonly ProjectionChunk[];
  readonly isNoop: boolean;
}

interface ProjectionChunk {
  readonly text: string;
  readonly runes: number;
}

const projectionChunkUTF16 = 4096;

/**
 * Maintains bounded-size UTF-16/rune chunks for local editor transactions.
 * Updating one normal typing range changes one chunk and two Fenwick entries;
 * rebuilding is reserved for a range that crosses chunk boundaries.
 */
class IncrementalTextProjection {
  #chunks: ProjectionChunk[] = [];
  #utf16 = new FenwickIndex([]);
  #runes = new FenwickIndex([]);

  constructor(value: string) {
    this.reset(value);
  }

  reset(value: string): void {
    this.#chunks = projectionChunks(value);
    this.#rebuildIndexes();
  }

  text(): string {
    return this.#chunks.map((chunk) => chunk.text).join("");
  }

  utf16Length(): number {
    return this.#utf16.total();
  }

  runeOffsetAtUTF16(offset: number): number {
    if (!Number.isSafeInteger(offset) || offset < 0 || offset > this.#utf16.total()) {
      throw new CRDTRuntimeError("range_error");
    }
    if (offset === this.#utf16.total()) {
      return this.#runes.total();
    }
    const location = this.#locateUTF16(offset);
    return this.#runes.prefix(location.chunk) + runeOffsetAtUTF16(projectionChunkAt(this.#chunks, location.chunk).text, location.offset);
  }

  utf16OffsetAtRune(offset: number): number {
    if (!Number.isSafeInteger(offset) || offset < 0 || offset > this.#runes.total()) {
      throw new CRDTRuntimeError("range_error");
    }
    if (offset === this.#runes.total()) {
      return this.#utf16.total();
    }
    const location = this.#runes.findContaining(offset);
    return this.#utf16.prefix(location.index) + utf16OffsetAtRune(projectionChunkAt(this.#chunks, location.index).text, location.offset);
  }

  prepareReplacement(from: number, to: number, insert: string): PreparedProjectionReplacement {
    if (!Number.isSafeInteger(from) || !Number.isSafeInteger(to) || from < 0 || to < from || to > this.#utf16.total() || typeof insert !== "string") {
      throw new CRDTRuntimeError("range_error");
    }
    const runeFrom = this.runeOffsetAtUTF16(from);
    const runeTo = this.runeOffsetAtUTF16(to);
    const start = this.#locateUTF16(from, true);
    const end = this.#locateUTF16(to, true);
    const startText = start.chunk === this.#chunks.length ? "" : projectionChunkAt(this.#chunks, start.chunk).text;
    const endText = end.chunk === this.#chunks.length ? "" : projectionChunkAt(this.#chunks, end.chunk).text;
    const prefix = startText.slice(0, start.offset);
    const suffix = end.chunk === this.#chunks.length ? "" : endText.slice(end.offset);
    const removedChunks = start.chunk === this.#chunks.length ? 0 : end.chunk - start.chunk + 1;
    const replacement = projectionChunks(`${prefix}${insert}${suffix}`);
    const oldLength = to - from;
    const isNoop = oldLength === insert.length && this.#sliceUTF16(from, to) === insert;
    return {
      runeFrom,
      runeTo,
      startChunk: start.chunk,
      removedChunks,
      replacement,
      isNoop,
    };
  }

  commitReplacement(replacement: PreparedProjectionReplacement): void {
    const changed = replacement.replacement;
    if (replacement.removedChunks === changed.length) {
      for (let index = 0; index < changed.length; index += 1) {
        const chunkIndex = replacement.startChunk + index;
        const previous = projectionChunkAt(this.#chunks, chunkIndex);
        const next = projectionChunkAt(changed, index);
        this.#chunks[chunkIndex] = next;
        this.#utf16.add(chunkIndex, next.text.length - previous.text.length);
        this.#runes.add(chunkIndex, next.runes - previous.runes);
      }
      return;
    }
    this.#chunks.splice(replacement.startChunk, replacement.removedChunks, ...changed);
    this.#rebuildIndexes();
  }

  #locateUTF16(offset: number, allowEnd = false): { readonly chunk: number; readonly offset: number } {
    if (!Number.isSafeInteger(offset) || offset < 0 || offset > this.#utf16.total()) {
      throw new CRDTRuntimeError("range_error");
    }
    if (offset === this.#utf16.total()) {
      if (allowEnd || this.#chunks.length === 0) {
        return { chunk: this.#chunks.length, offset: 0 };
      }
      throw new CRDTRuntimeError("range_error");
    }
    const location = this.#utf16.findContaining(offset);
    return { chunk: location.index, offset: location.offset };
  }

  #sliceUTF16(from: number, to: number): string {
    if (from === to) {
      return "";
    }
    const start = this.#locateUTF16(from, true);
    const end = this.#locateUTF16(to, true);
    if (start.chunk === end.chunk) {
      return projectionChunkAt(this.#chunks, start.chunk).text.slice(start.offset, end.offset);
    }
    const values = [projectionChunkAt(this.#chunks, start.chunk).text.slice(start.offset)];
    for (let index = start.chunk + 1; index < end.chunk; index += 1) {
      values.push(projectionChunkAt(this.#chunks, index).text);
    }
    if (end.chunk < this.#chunks.length) {
      values.push(projectionChunkAt(this.#chunks, end.chunk).text.slice(0, end.offset));
    }
    return values.join("");
  }

  #rebuildIndexes(): void {
    this.#utf16 = new FenwickIndex(this.#chunks.map((chunk) => chunk.text.length));
    this.#runes = new FenwickIndex(this.#chunks.map((chunk) => chunk.runes));
  }
}

class FenwickIndex {
  readonly #tree: number[];

  constructor(values: readonly number[]) {
    this.#tree = Array(values.length + 1).fill(0);
    for (let index = 0; index < values.length; index += 1) {
      this.add(index, values[index]!);
    }
  }

  total(): number {
    return this.prefix(this.#tree.length - 1);
  }

  prefix(end: number): number {
    let total = 0;
    for (let index = end; index > 0; index -= index & -index) {
      total += this.#tree[index]!;
    }
    return total;
  }

  add(index: number, delta: number): void {
    for (let current = index + 1; current < this.#tree.length; current += current & -current) {
      this.#tree[current]! += delta;
    }
  }

  findContaining(offset: number): { readonly index: number; readonly offset: number } {
    let index = 0;
    let total = 0;
    for (let bit = highestPowerOfTwoAtMost(this.#tree.length - 1); bit > 0; bit >>= 1) {
      const next = index + bit;
      if (next < this.#tree.length && total + this.#tree[next]! <= offset) {
        index = next;
        total += this.#tree[next]!;
      }
    }
    if (index >= this.#tree.length - 1) {
      throw new CRDTRuntimeError("range_error");
    }
    return { index, offset: offset - total };
  }
}

function highestPowerOfTwoAtMost(value: number): number {
  let result = 1;
  while (result * 2 <= value) {
    result *= 2;
  }
  return result;
}

function projectionChunkAt(chunks: readonly ProjectionChunk[], index: number): ProjectionChunk {
  const chunk = chunks[index];
  if (chunk === undefined) {
    throw new CRDTRuntimeError("range_error");
  }
  return chunk;
}

function projectionChunks(value: string): ProjectionChunk[] {
  const chunks: ProjectionChunk[] = [];
  for (let start = 0; start < value.length;) {
    let end = Math.min(start + projectionChunkUTF16, value.length);
    if (end < value.length && isHighSurrogate(value.charCodeAt(end - 1)) && isLowSurrogate(value.charCodeAt(end))) {
      end -= 1;
    }
    if (end === start) {
      end = Math.min(start + 2, value.length);
    }
    const text = value.slice(start, end);
    chunks.push({ text, runes: runeCount(text) });
    start = end;
  }
  return chunks;
}

function runeCount(value: string): number {
  let count = 0;
  for (const _ of value) {
    count += 1;
  }
  return count;
}

function isHighSurrogate(value: number): boolean {
  return value >= 0xd800 && value <= 0xdbff;
}

function isLowSurrogate(value: number): boolean {
  return value >= 0xdc00 && value <= 0xdfff;
}

function singleCodeMirrorReplacement(
  update: CodeMirrorViewUpdate,
  documentLength: number,
): EditorUTF16Replacement | undefined {
  const changes = update.changes;
  if (changes === undefined || typeof changes.iterChanges !== "function" || !Number.isSafeInteger(documentLength) || documentLength < 0) {
    return undefined;
  }
  let result: EditorUTF16Replacement | undefined;
  let invalid = false;
  try {
    changes.iterChanges((fromA, toA, fromB, toB, inserted) => {
      if (result !== undefined || invalid || !Number.isSafeInteger(fromA) || !Number.isSafeInteger(toA) ||
        !Number.isSafeInteger(fromB) || !Number.isSafeInteger(toB) || fromA < 0 || toA < fromA || fromB !== fromA || toB < fromB ||
        inserted === null || inserted === undefined || typeof inserted.toString !== "function") {
        invalid = true;
        return;
      }
      const insert = inserted.toString();
      if (typeof insert !== "string" || toB - fromB !== insert.length) {
        invalid = true;
        return;
      }
      result = { from: fromA, to: toA, insert, newLength: documentLength };
    });
  } catch {
    return undefined;
  }
  return invalid ? undefined : result;
}

function selectionPort(value: PlainTextEditorPort): SelectionEditorPort | undefined {
  if (
    typeof (value as Partial<SelectionEditorPort>).readSelection !== "function" ||
    typeof (value as Partial<SelectionEditorPort>).writeSelection !== "function"
  ) {
    return undefined;
  }
  return value as SelectionEditorPort;
}

function isRGASelection(value: unknown): value is RGASelection {
  return typeof value === "object" && value !== null && "anchor" in value && "head" in value;
}

function runeOffsetAtUTF16(value: string, offset: number): number {
  if (!Number.isSafeInteger(offset) || offset < 0 || offset > value.length) {
    throw new CRDTRuntimeError("range_error");
  }
  let runes = 0;
  let utf16 = 0;
  for (const rune of value) {
    if (utf16 === offset) {
      return runes;
    }
    utf16 += rune.length;
    runes += 1;
    if (utf16 > offset) {
      throw new CRDTRuntimeError("invalid_selection");
    }
  }
  if (utf16 === offset) {
    return runes;
  }
  throw new CRDTRuntimeError("range_error");
}

function utf16OffsetAtRune(value: string, offset: number): number {
  if (!Number.isSafeInteger(offset) || offset < 0) {
    throw new CRDTRuntimeError("range_error");
  }
  let runes = 0;
  let utf16 = 0;
  for (const rune of value) {
    if (runes === offset) {
      return utf16;
    }
    utf16 += rune.length;
    runes += 1;
  }
  if (runes === offset) {
    return utf16;
  }
  throw new CRDTRuntimeError("range_error");
}

/**
 * RGA replacement is atomic only when its inserted text fits one negotiated
 * local edit. Splitting a replacement into delete/insert frames can leave a
 * delete-only state if a later insertion is rejected, so callers must split
 * large editor operations before they reach this binding.
 */
function assertBoundedReplacement(value: string, document: RGAWasmDocument): void {
  const { maxLocalEditBytes, maxLocalEditRunes } = document.protocol;
  const encoder = new TextEncoder();
  let runes = 0;
  let bytes = 0;
  for (const rune of value) {
    runes += 1;
    bytes += encoder.encode(rune).byteLength;
    if (runes > maxLocalEditRunes || bytes > maxLocalEditBytes) {
      throw new CRDTRuntimeError("resource_limit");
    }
  }
}
