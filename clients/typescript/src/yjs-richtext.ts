/**
 * Native Yjs rich-text bindings for Quill-compatible Delta editors.
 *
 * This module remains entirely within Yjs: it uses Y.Text Delta operations and
 * V1/V2 Yjs updates. It neither converts to the repository's RGA protocols nor
 * accepts arbitrary Quill modules, attributes, or embeds. An application must
 * explicitly admit the formats and embeds that belong to its room schema.
 */

import * as Y from "yjs";

import {
  YjsBindingError,
  type YjsBindingErrorCode,
  type YjsUpdateFormat,
} from "./yjs.js";

/** Scalar values that may appear in one approved Quill format or embed. */
export type YjsRichTextValue = null | boolean | number | string;

/** A single-key Quill embed. The key must be admitted by `allowedEmbeds`. */
export interface YjsRichTextEmbed {
  readonly [name: string]: YjsRichTextValue;
}

/** One portable Quill/Y.Text Delta operation. */
export interface YjsRichTextDeltaOperation {
  readonly insert?: string | YjsRichTextEmbed;
  readonly delete?: number;
  readonly retain?: number;
  readonly attributes?: Readonly<Record<string, YjsRichTextValue>>;
}

/** A Quill-compatible Delta object accepted by this binding. */
export interface YjsRichTextDelta {
  readonly ops: readonly YjsRichTextDeltaOperation[];
}

/**
 * Resource and schema boundaries for one Y.Text rich-text view. All limits
 * are mandatory: rich formatting must not create an unbounded side channel
 * around the room's update and snapshot limits.
 */
export interface YjsRichTextBindingOptions {
  /** Pins V1 or V2. One room must never accept both update encodings. */
  readonly updateFormat?: YjsUpdateFormat;
  /** Rejects an inbound update before Yjs decodes it. */
  readonly maxUpdateBytes: number;
  /** Caps visible Y.Text UTF-16 units, including one unit per embed. */
  readonly maxTextUTF16: number;
  /** Caps operations copied from one editor or Y.Text event. */
  readonly maxDeltaOperations: number;
  /** Caps format keys attached to a single Delta operation. */
  readonly maxAttributesPerOperation: number;
  /** Caps an admitted format-key byte length. */
  readonly maxAttributeKeyBytes: number;
  /** Caps one admitted scalar format value after UTF-8/JSON encoding. */
  readonly maxAttributeValueBytes: number;
  /** Caps bytes in the one-key JSON embed accepted by this profile. */
  readonly maxEmbedBytes: number;
  /** Exact Quill format keys admitted by this room's editor schema. */
  readonly allowedAttributes: readonly string[];
  /** Exact single-key Quill embeds admitted by this room's editor schema. */
  readonly allowedEmbeds: readonly string[];
  /**
   * Receives local Yjs updates when this binding owns the transport. It is a
   * synchronous hand-off to an application outbox, never a durable receipt.
   */
  readonly onLocalUpdate?: (update: Uint8Array) => void;
  /** Receives a validated remote/external Y.Text Delta for the editor port. */
  readonly onRemoteDelta?: (delta: YjsRichTextDelta) => void;
  /** Reports a stable failure without exposing update bytes. */
  readonly onError?: (error: YjsBindingError) => void;
}

/** Structural subset of Quill 2 used without making Quill a package dependency. */
export interface YjsQuillRichTextPort {
  getContents(): unknown;
  setContents(delta: YjsRichTextDelta, source?: "api" | "silent" | "user"): unknown;
  updateContents(delta: YjsRichTextDelta, source?: "api" | "silent" | "user"): unknown;
  on(event: "text-change", listener: (delta: unknown, oldContents: unknown, source: string) => void): unknown;
  off?(event: "text-change", listener: (delta: unknown, oldContents: unknown, source: string) => void): unknown;
}

export interface BindYjsQuillRichTextOptions extends Omit<YjsRichTextBindingOptions, "onRemoteDelta"> {
  /** Import editor contents once only into an otherwise empty Y.Text. */
  readonly initialContent?: "document" | "editor";
}

/**
 * Bounded native Yjs rich-text view. The binding carries Quill Delta values
 * directly, preserving approved formats and one-key embeds rather than
 * flattening them into a string. Any received schema violation freezes this
 * projection; the Y.Doc is not rolled back because it may contain concurrent
 * valid Yjs changes and must be recovered through the authenticated room.
 */
export class YjsRichTextBinding {
  readonly updateFormat: YjsUpdateFormat;
  readonly #remoteUpdateOrigin = Object.freeze({ source: "crdt-yjs-richtext-remote-update" });
  readonly #localOrigin = Object.freeze({ source: "crdt-yjs-richtext-editor" });
  readonly #textObserver: (event: Y.YTextEvent) => void;
  readonly #updateObserver: (update: Uint8Array, origin: unknown) => void;
  readonly #limits: RichTextLimits;
  #closed = false;
  #projecting = true;
  #textObserved = false;
  #localUpdateFailure: Extract<YjsBindingErrorCode, "local_update_failed" | "resource_limit"> | undefined;

  constructor(
    readonly document: Y.Doc,
    readonly text: Y.Text,
    private readonly options: YjsRichTextBindingOptions,
  ) {
    if (text.doc !== document) {
      throw new YjsBindingError("document_mismatch");
    }
    this.#limits = richTextLimits(options);
    this.updateFormat = options.updateFormat ?? "v1";
    this.delta(); // Validate an already materialized Y.Text before observing it.
    this.#textObserver = (event) => this.#handleTextEvent(event);
    this.#updateObserver = (update, origin) => this.#handleDocumentUpdate(update, origin);
    text.observe(this.#textObserver);
    this.#textObserved = true;
    if (this.updateFormat === "v1") {
      document.on("update", this.#updateObserver);
    } else {
      document.on("updateV2", this.#updateObserver);
    }
  }

  /** Applies one transport-authenticated update in the room's pinned encoding. */
  applyRemoteUpdate(update: Uint8Array): void {
    this.#assertOpen();
    assertBoundedBytes(update, this.#limits.maxUpdateBytes);
    try {
      if (this.updateFormat === "v1") {
        Y.applyUpdate(this.document, update, this.#remoteUpdateOrigin);
      } else {
        Y.applyUpdateV2(this.document, update, this.#remoteUpdateOrigin);
      }
    } catch {
      throw new YjsBindingError("invalid_update");
    }
  }

  /** Returns the current approved rich-text projection as a detached Delta. */
  delta(): YjsRichTextDelta {
    this.#assertOpen();
    if (this.text.length > this.#limits.maxTextUTF16) {
      throw new YjsBindingError("resource_limit");
    }
    return normalizeRichTextDelta({ ops: this.text.toDelta() as unknown[] }, this.#limits);
  }

  /** Returns the native Yjs state vector for V1 or V2 state-vector recovery. */
  encodeStateVector(): Uint8Array {
    this.#assertOpen();
    const vector = Y.encodeStateVector(this.document);
    assertBoundedBytes(vector, this.#limits.maxUpdateBytes);
    return vector;
  }

  /** Encodes the native V1/V2 Yjs state difference for one peer vector. */
  encodeStateAsUpdate(remoteStateVector?: Uint8Array): Uint8Array {
    this.#assertOpen();
    if (remoteStateVector !== undefined) {
      assertBoundedBytes(remoteStateVector, this.#limits.maxUpdateBytes);
    }
    try {
      const update = this.updateFormat === "v1"
        ? Y.encodeStateAsUpdate(this.document, remoteStateVector)
        : Y.encodeStateAsUpdateV2(this.document, remoteStateVector);
      assertBoundedBytes(update, this.#limits.maxUpdateBytes);
      return update;
    } catch {
      throw new YjsBindingError("invalid_update");
    }
  }

  /**
   * Applies one editor-originated Quill Delta. The caller/editor has already
   * made the same change locally, so this method deliberately does not invoke
   * `onRemoteDelta` for its own transaction.
   */
  applyLocalDelta(delta: unknown): void {
    this.#assertProjection();
    this.#assertLocalUpdatePath();
    const normalized = normalizeRichTextDelta(delta, this.#limits);
    const nextLength = projectedLength(this.text.length, normalized.ops);
    if (nextLength > this.#limits.maxTextUTF16) {
      throw new YjsBindingError("resource_limit");
    }
    if (normalized.ops.length === 0) {
      return;
    }
    this.document.transact(() => {
      // Yjs owns Delta interpretation and its own transaction invariants. The
      // normalized copy above has already bounded every user-controlled value.
      this.text.applyDelta([...normalized.ops]);
    }, this.#localOrigin);
    // A manual outbox can fail only after Yjs commits. Latch before another
    // local mutation can create an additional unhanded update.
    this.#assertLocalUpdatePath();
  }

  /** Stops listeners; caller-owned Y.Doc and Y.Text instances remain open. */
  destroy(): boolean {
    if (this.#closed) {
      return false;
    }
    this.#closed = true;
    if (this.#textObserved) {
      this.text.unobserve(this.#textObserver);
      this.#textObserved = false;
    }
    if (this.updateFormat === "v1") {
      this.document.off("update", this.#updateObserver);
    } else {
      this.document.off("updateV2", this.#updateObserver);
    }
    return true;
  }

  #handleTextEvent(event: Y.YTextEvent): void {
    if (this.#closed || !this.#projecting) {
      return;
    }
    let delta: YjsRichTextDelta;
    try {
      if (this.text.length > this.#limits.maxTextUTF16) {
        throw new YjsBindingError("resource_limit");
      }
      delta = normalizeRichTextDelta({ ops: event.delta as unknown[] }, this.#limits);
    } catch (error) {
      this.#stopProjection(errorCode(error, "unsupported_rich_text"));
      return;
    }
    if (event.transaction.origin !== this.#localOrigin && delta.ops.length > 0) {
      try {
        this.options.onRemoteDelta?.(delta);
      } catch {
        this.#stopProjection("editor_update_failed");
      }
    }
  }

  #handleDocumentUpdate(update: Uint8Array, origin: unknown): void {
    if (
      this.#closed
      || this.#localUpdateFailure !== undefined
      || origin === this.#remoteUpdateOrigin
      || this.options.onLocalUpdate === undefined
    ) {
      return;
    }
    if (update.byteLength > this.#limits.maxUpdateBytes) {
      this.#failLocalUpdate("resource_limit");
      return;
    }
    try {
      this.options.onLocalUpdate(update.slice());
    } catch {
      this.#failLocalUpdate("local_update_failed");
    }
  }

  #assertOpen(): void {
    if (this.#closed) {
      throw new YjsBindingError("document_closed");
    }
  }

  #assertProjection(): void {
    this.#assertOpen();
    if (!this.#projecting) {
      throw new YjsBindingError("unsupported_rich_text");
    }
  }

  #assertLocalUpdatePath(): void {
    if (this.#localUpdateFailure !== undefined) {
      throw new YjsBindingError(this.#localUpdateFailure);
    }
  }

  #failLocalUpdate(code: Extract<YjsBindingErrorCode, "local_update_failed" | "resource_limit">): void {
    if (this.#localUpdateFailure !== undefined) {
      return;
    }
    this.#localUpdateFailure = code;
    this.#report(code);
  }

  #stopProjection(code: Extract<YjsBindingErrorCode, "editor_update_failed" | "resource_limit" | "unsupported_rich_text">): void {
    if (!this.#projecting) {
      return;
    }
    this.#projecting = false;
    if (this.#textObserved) {
      this.text.unobserve(this.#textObserver);
      this.#textObserved = false;
    }
    this.#report(code);
  }

  #report(code: YjsBindingErrorCode): void {
    try {
      this.options.onError?.(new YjsBindingError(code));
    } catch {
      // Reporting must not re-enter Yjs's synchronous observer loop.
    }
  }
}

/**
 * Quill 2 adapter over `YjsRichTextBinding`. It is structural so applications
 * retain control of their Quill version, modules, lifecycle, and provider.
 */
export class YjsQuillRichTextBinding {
  readonly #binding: YjsRichTextBinding;
  readonly #listener: (delta: unknown, oldContents: unknown, source: string) => void;
  #closed = false;

  constructor(
    readonly document: Y.Doc,
    readonly text: Y.Text,
    private readonly quill: YjsQuillRichTextPort,
    private readonly options: BindYjsQuillRichTextOptions,
  ) {
    this.#binding = new YjsRichTextBinding(document, text, {
      ...options,
      onRemoteDelta: (delta) => quill.updateContents(delta, "api"),
    });
    this.#listener = (delta, _oldContents, source) => {
      if (source !== "user" || this.#closed) {
        return;
      }
      try {
        this.#binding.applyLocalDelta(delta);
      } catch (error) {
        // A rejected local edit must not remain as a visible editor-only fork.
        // A manual-outbox failure is different: Yjs already committed the
        // matching Delta, so this projection is retained but input is latched.
        if (!(error instanceof YjsBindingError && error.code === "local_update_failed")) {
          this.#restoreEditor();
        }
        throw error;
      }
    };
    quill.on("text-change", this.#listener);
    try {
      if (options.initialContent === "editor") {
        if (text.length !== 0) {
          throw new YjsBindingError("invalid_options");
        }
        this.#binding.applyLocalDelta(quill.getContents());
      } else {
        this.#restoreEditor();
      }
    } catch (error) {
      this.destroy();
      throw error;
    }
  }

  get updateFormat(): YjsUpdateFormat {
    return this.#binding.updateFormat;
  }

  applyRemoteUpdate(update: Uint8Array): void {
    this.#binding.applyRemoteUpdate(update);
  }

  encodeStateVector(): Uint8Array {
    return this.#binding.encodeStateVector();
  }

  encodeStateAsUpdate(remoteStateVector?: Uint8Array): Uint8Array {
    return this.#binding.encodeStateAsUpdate(remoteStateVector);
  }

  delta(): YjsRichTextDelta {
    return this.#binding.delta();
  }

  destroy(): boolean {
    if (this.#closed) {
      return false;
    }
    this.#closed = true;
    this.quill.off?.("text-change", this.#listener);
    this.#binding.destroy();
    return true;
  }

  #restoreEditor(): void {
    this.quill.setContents(this.#binding.delta(), "api");
  }
}

/** Binds one Quill-compatible rich editor to a native Y.Text Delta surface. */
export function bindYjsQuillRichText(
  document: Y.Doc,
  text: Y.Text,
  quill: YjsQuillRichTextPort,
  options: BindYjsQuillRichTextOptions,
): YjsQuillRichTextBinding {
  return new YjsQuillRichTextBinding(document, text, quill, options);
}

interface RichTextLimits {
  readonly maxUpdateBytes: number;
  readonly maxTextUTF16: number;
  readonly maxDeltaOperations: number;
  readonly maxAttributesPerOperation: number;
  readonly maxAttributeKeyBytes: number;
  readonly maxAttributeValueBytes: number;
  readonly maxEmbedBytes: number;
  readonly allowedAttributes: ReadonlySet<string>;
  readonly allowedEmbeds: ReadonlySet<string>;
}

function richTextLimits(options: YjsRichTextBindingOptions): RichTextLimits {
  if (!validPositive(options.maxUpdateBytes) || !validPositive(options.maxTextUTF16) ||
    !validPositive(options.maxDeltaOperations) || !validPositive(options.maxAttributesPerOperation) ||
    !validPositive(options.maxAttributeKeyBytes) || !validPositive(options.maxAttributeValueBytes) ||
    !validPositive(options.maxEmbedBytes)) {
    throw new YjsBindingError("invalid_options");
  }
  const allowedAttributes = approvedNames(options.allowedAttributes, options.maxAttributeKeyBytes);
  const allowedEmbeds = approvedNames(options.allowedEmbeds, options.maxAttributeKeyBytes);
  if (allowedAttributes === undefined || allowedEmbeds === undefined) {
    throw new YjsBindingError("invalid_options");
  }
  return {
    maxUpdateBytes: options.maxUpdateBytes,
    maxTextUTF16: options.maxTextUTF16,
    maxDeltaOperations: options.maxDeltaOperations,
    maxAttributesPerOperation: options.maxAttributesPerOperation,
    maxAttributeKeyBytes: options.maxAttributeKeyBytes,
    maxAttributeValueBytes: options.maxAttributeValueBytes,
    maxEmbedBytes: options.maxEmbedBytes,
    allowedAttributes,
    allowedEmbeds,
  };
}

function normalizeRichTextDelta(value: unknown, limits: RichTextLimits): YjsRichTextDelta {
  const source = deltaOperations(value);
  if (source === undefined) {
    throw new YjsBindingError("unsupported_rich_text");
  }
  if (source.length > limits.maxDeltaOperations) {
    throw new YjsBindingError("resource_limit");
  }
  const operations = source.map((operation) => normalizeRichTextOperation(operation, limits));
  return Object.freeze({ ops: Object.freeze(operations) });
}

function deltaOperations(value: unknown): readonly unknown[] | undefined {
  if (Array.isArray(value)) {
    return value;
  }
  // Quill 2 passes a Delta class instance, not a plain object, to its
  // text-change callback and getContents(). Its public `ops` field is the
  // contract we consume; individual operations remain strict plain records.
  if (typeof value !== "object" || value === null || !Object.hasOwn(value, "ops") ||
    !hasOnlyKeys(value as Record<string, unknown>, ["ops"]) || !Array.isArray((value as { ops?: unknown }).ops)) {
    return undefined;
  }
  return (value as { ops: readonly unknown[] }).ops;
}

function normalizeRichTextOperation(value: unknown, limits: RichTextLimits): YjsRichTextDeltaOperation {
  if (!plainRecord(value) || !hasOnlyKeys(value, ["insert", "retain", "delete", "attributes"])) {
    throw new YjsBindingError("unsupported_rich_text");
  }
  const actions = Number(Object.hasOwn(value, "insert")) + Number(Object.hasOwn(value, "retain")) + Number(Object.hasOwn(value, "delete"));
  if (actions !== 1) {
    throw new YjsBindingError("unsupported_rich_text");
  }
  const attributes = value.attributes === undefined ? undefined : normalizeAttributes(value.attributes, limits);
  if (Object.hasOwn(value, "insert")) {
    const insert = typeof value.insert === "string" ? value.insert : normalizeEmbed(value.insert, limits);
    if (typeof insert === "string") {
      if (insert.length === 0) {
        throw new YjsBindingError("unsupported_rich_text");
      }
      if (insert.length > limits.maxTextUTF16) {
        throw new YjsBindingError("resource_limit");
      }
    }
    return Object.freeze({ ...(attributes === undefined ? {} : { attributes }), insert });
  }
  if (Object.hasOwn(value, "retain")) {
    if (!validPositive(value.retain)) {
      throw new YjsBindingError("unsupported_rich_text");
    }
    return Object.freeze({ ...(attributes === undefined ? {} : { attributes }), retain: value.retain });
  }
  if (!validPositive(value.delete) || attributes !== undefined) {
    throw new YjsBindingError("unsupported_rich_text");
  }
  return Object.freeze({ delete: value.delete });
}

function normalizeAttributes(value: unknown, limits: RichTextLimits): Readonly<Record<string, YjsRichTextValue>> {
  if (!plainRecord(value)) {
    throw new YjsBindingError("unsupported_rich_text");
  }
  const entries = Object.entries(value);
  if (entries.length > limits.maxAttributesPerOperation) {
    throw new YjsBindingError("resource_limit");
  }
  const attributes: Record<string, YjsRichTextValue> = {};
  for (const [key, raw] of entries) {
    if (!limits.allowedAttributes.has(key) || utf8Bytes(key) > limits.maxAttributeKeyBytes || !isRichTextValue(raw)) {
      throw new YjsBindingError("unsupported_rich_text");
    }
    if (utf8Bytes(JSON.stringify(raw)) > limits.maxAttributeValueBytes) {
      throw new YjsBindingError("resource_limit");
    }
    attributes[key] = raw;
  }
  return Object.freeze(attributes);
}

function normalizeEmbed(value: unknown, limits: RichTextLimits): YjsRichTextEmbed {
  if (!plainRecord(value)) {
    throw new YjsBindingError("unsupported_rich_text");
  }
  const entries = Object.entries(value);
  if (entries.length !== 1) {
    throw new YjsBindingError("unsupported_rich_text");
  }
  const entry = entries[0];
  if (entry === undefined) {
    throw new YjsBindingError("unsupported_rich_text");
  }
  const [key, raw] = entry;
  if (!limits.allowedEmbeds.has(key) || utf8Bytes(key) > limits.maxAttributeKeyBytes || !isRichTextValue(raw)) {
    throw new YjsBindingError("unsupported_rich_text");
  }
  if (utf8Bytes(JSON.stringify({ [key]: raw })) > limits.maxEmbedBytes) {
    throw new YjsBindingError("resource_limit");
  }
  return Object.freeze({ [key]: raw });
}

function projectedLength(currentLength: number, operations: readonly YjsRichTextDeltaOperation[]): number {
  // Delta retain/delete lengths consume the pre-change Y.Text. An insert adds
  // output but does not consume source content; treating it as a source offset
  // would incorrectly reject valid `{ insert, retain }` editor transactions.
  let sourceCursor = 0;
  let nextLength = currentLength;
  for (const operation of operations) {
    if (operation.retain !== undefined) {
      if (operation.retain > currentLength - sourceCursor) {
        throw new YjsBindingError("invalid_selection");
      }
      sourceCursor += operation.retain;
      continue;
    }
    if (operation.delete !== undefined) {
      if (operation.delete > currentLength - sourceCursor) {
        throw new YjsBindingError("invalid_selection");
      }
      nextLength -= operation.delete;
      sourceCursor += operation.delete;
      continue;
    }
    const added = typeof operation.insert === "string" ? operation.insert.length : 1;
    nextLength += added;
  }
  return nextLength;
}

function approvedNames(value: readonly string[], maximumBytes: number): ReadonlySet<string> | undefined {
  if (!Array.isArray(value)) {
    return undefined;
  }
  const names = new Set<string>();
  for (const name of value) {
    if (typeof name !== "string" || !validSchemaName(name) || utf8Bytes(name) > maximumBytes || names.has(name)) {
      return undefined;
    }
    names.add(name);
  }
  return names;
}

function plainRecord(value: unknown): value is Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return false;
  }
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function hasOnlyKeys(value: Record<string, unknown>, keys: readonly string[]): boolean {
  return Object.keys(value).every((key) => keys.includes(key));
}

function isRichTextValue(value: unknown): value is YjsRichTextValue {
  return value === null || typeof value === "string" || typeof value === "boolean" ||
    typeof value === "number" && Number.isFinite(value);
}

function errorCode(error: unknown, fallback: Extract<YjsBindingErrorCode, "resource_limit" | "unsupported_rich_text">): Extract<YjsBindingErrorCode, "resource_limit" | "unsupported_rich_text"> {
  return error instanceof YjsBindingError && error.code === "resource_limit" ? "resource_limit" : fallback;
}

function assertBoundedBytes(value: Uint8Array, maximumBytes: number): void {
  if (value.byteLength > maximumBytes) {
    throw new YjsBindingError("resource_limit");
  }
}

function utf8Bytes(value: string): number {
  return new TextEncoder().encode(value).byteLength;
}

function validSchemaName(value: string): boolean {
  return value.length > 0 && /^[A-Za-z0-9._:-]+$/.test(value);
}

function validPositive(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0;
}
