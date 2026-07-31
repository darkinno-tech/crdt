/**
 * Plain-text editor bindings for the negotiated Go/Wasm RGA protocol.
 *
 * These bindings intentionally do not translate HTML, ProseMirror nodes, Slate
 * elements, embeds, or formatting marks into text. Doing so would silently
 * discard collaborative structure. Use richtext.Document's separately
 * negotiated protocol for inline marks, and supply an application-owned block
 * schema before binding a rich document tree.
 */

import { CRDTRuntimeError, RGAWasmDocument } from "./wasm.js";

export type RGAFrameListener = (frame: Uint8Array) => void;

/** The minimal contract shared by textarea, Monaco, and host editor adapters. */
export interface PlainTextEditorPort {
  readText(): string;
  writeText(value: string): void;
  observeText(listener: () => void): () => void;
}

export interface BindRGAPlainTextOptions {
  /** Receives exact RGA frames for an authenticated, durable outbox. */
  readonly onLocalFrame: RGAFrameListener;
  /** Import an existing editor value as local CRDT edits instead of overwriting it. */
  readonly initialContent?: "document" | "editor";
}

/**
 * Synchronizes one plain-text editor port with a local RGA runtime. Local
 * edits use Unicode scalar offsets, are split under the negotiated runtime
 * limits, and emit ordinary canonical RGA frames. Remote frames are applied
 * before replacing the port value and cannot echo back into the outbox.
 */
export class RGAPlainTextBinding {
  #text: string;
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
    this.#text = document.text();
    this.#unobserve = editor.observeText(() => this.#handleEditorChange());
    if (options.initialContent === "editor") {
      this.#replaceDocument(editor.readText());
    } else {
      this.#writeEditor(this.#text);
    }
  }

  /** Applies one authenticated, manifest-checked remote frame. */
  applyRemote(frame: Uint8Array): void {
    this.#assertOpen();
    this.document.applyDelta(frame);
    this.#text = this.document.text();
    this.#writeEditor(this.#text);
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
      this.#writeEditor(this.#text);
      throw error;
    }
  }

  #replaceDocument(next: string): void {
    this.#assertOpen();
    if (next === this.#text) {
      return;
    }
    const previousRunes = Array.from(this.#text);
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
    this.#text = this.document.text();
    if (this.#text !== next) {
      throw new CRDTRuntimeError("binding_diverged");
    }
    this.options.onLocalFrame(frame);
  }

  #writeEditor(value: string): void {
    this.#writing = true;
    try {
      if (this.editor.readText() !== value) {
        this.editor.writeText(value);
      }
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
  return bindRGAPlainText(document, {
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
  }, options);
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
