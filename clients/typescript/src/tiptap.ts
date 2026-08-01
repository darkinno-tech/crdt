/**
 * A bounded Tiptap / ProseMirror rich-text binding.
 *
 * The stable richtext v1 wire format stores an RGA sequence plus attributes,
 * not a ProseMirror tree. This adapter therefore exposes one deliberately
 * small, reversible profile instead of serialising arbitrary Tiptap JSON:
 * paragraphs, headings, one-paragraph block quotes, code blocks, five common
 * marks, hard breaks, and application-approved atomic inline embeds.
 *
 * Rich block trees (lists, tables, nested editors, and block embeds) belong in
 * a separately negotiated documenttree group. Tiptap node identities,
 * NodeViews, selections, plugins, and undo history remain local state.
 */

import { CRDTRuntimeError } from "./wasm.js";
import type {
  RGAFrameListener,
  RichTextAttributeChange,
  RichTextEditorDocument,
  RichTextEditorOperation,
  RichTextSpan,
  TiptapJSONNode,
  TiptapTextPort,
} from "./bindings.js";

/** Bind this exact application schema ID in the authenticated rich-text Manifest. */
export const TIPTAP_CORE_RICH_TEXT_SCHEMA_ID = "darkinno:tiptap-core-richtext-v1";

const BLOCK_ATTRIBUTE = "rt.block";
const EMBED_KIND_ATTRIBUTE = "rt.embed.kind";
const EMBED_DATA_ATTRIBUTE = "rt.embed.data";
const HARD_BREAK_ATTRIBUTE = "rt.tiptap.hard-break";
const OBJECT_REPLACEMENT_CHARACTER = "\uFFFC";
const DEFAULT_MAX_BLOCKS = 4_096;
const DEFAULT_MAX_INLINE_NODES = 16_384;
const DEFAULT_MAX_TEXT_BYTES = 64 << 10;
const DEFAULT_MAX_TEXT_RUNES = 16 << 10;
const DEFAULT_MAX_EMBED_BYTES = 64 << 10;
const DEFAULT_MAX_OPERATIONS = 512;
const MAX_JSON_DEPTH = 32;

const MARK_ATTRIBUTES: Readonly<Record<string, string>> = {
  bold: "rt.bold",
  italic: "rt.italic",
  underline: "rt.underline",
  strike: "rt.strike",
  code: "rt.code",
};
const MARK_NAMES = new Set(Object.keys(MARK_ATTRIBUTES));
const PROFILE_ATTRIBUTE_KEYS = new Set([
  BLOCK_ATTRIBUTE,
  ...Object.values(MARK_ATTRIBUTES),
  EMBED_KIND_ATTRIBUTE,
  EMBED_DATA_ATTRIBUTE,
  HARD_BREAK_ATTRIBUTE,
]);

type AttributeMap = Readonly<Record<string, string>>;
type BlockKind = "paragraph" | "heading" | "quote" | "code";

interface RichTextRun {
  readonly text: string;
  readonly attributes: AttributeMap;
  readonly runes: number;
}

interface BindingLimits {
  readonly maxBlocks: number;
  readonly maxInlineNodes: number;
  readonly maxTextBytes: number;
  readonly maxTextRunes: number;
  readonly maxEmbedBytes: number;
  readonly maxOperations: number;
}

interface BindingBudget {
  blocks: number;
  inlineNodes: number;
  textBytes: number;
  textRunes: number;
  embedBytes: number;
}

interface BlockDescriptor {
  readonly kind: BlockKind;
  readonly level?: number;
}

/** JSON values accepted as the structured payload of one atomic embed. */
export type TiptapEmbedValue = null | boolean | number | string | TiptapEmbedArray | TiptapEmbedObject;
export interface TiptapEmbedArray extends ReadonlyArray<TiptapEmbedValue> {}
export interface TiptapEmbedObject {
  readonly [key: string]: TiptapEmbedValue;
}

/**
 * Converts exactly one application-owned atomic inline Tiptap node to the
 * structured payload of an `rt.embed.*` pair. Both functions are required:
 * the binding verifies the remote `decode` result by encoding it again before
 * it reaches `setContent`, preventing a permissive renderer from widening the
 * negotiated schema.
 */
export interface TiptapEmbedCodec {
  /** Canonical semantic kind stored in `rt.embed.kind`; it is part of the schema contract. */
  readonly kind: string;
  /** Exact Tiptap node name accepted as an inline atom for this kind. */
  readonly nodeType: string;
  /** Validates and returns the JSON-object payload for a local editor node. */
  encode(node: TiptapJSONNode): Readonly<Record<string, TiptapEmbedValue>>;
  /** Produces one validated local atomic node from a received JSON-object payload. */
  decode(payload: Readonly<Record<string, TiptapEmbedValue>>): TiptapJSONNode;
}

/** The structural Tiptap contract; no `@tiptap/*` runtime dependency is bundled. */
export interface TiptapRichTextPort extends TiptapTextPort {}

/**
 * An application-owned ProseMirror bridge for the same fixed rich-text
 * profile. `replaceJSON` must dispatch a tagged remote transaction and its
 * `observeUpdate` callback must omit that transaction; ProseMirror selections,
 * plugins, history, and NodeViews stay in the application layer.
 */
export interface ProseMirrorRichTextPort {
  readJSON(): TiptapJSONNode;
  replaceJSON(content: TiptapJSONNode): boolean;
  observeUpdate(listener: () => void): () => void;
}

export interface BindTiptapRichTextOptions {
  /** Receives one canonical rich-text frame for each accepted local transaction. */
  readonly onLocalFrame: RGAFrameListener;
  /** Import the current Tiptap document once, otherwise render the CRDT projection. */
  readonly initialContent?: "document" | "editor";
  /** Only these atomic inline node codecs may cross the rich-text boundary. */
  readonly embeds?: readonly TiptapEmbedCodec[];
  /** Bounds root-level rich-text blocks before a local CRDT transaction exists. */
  readonly maxBlocks?: number;
  /** Bounds Tiptap text, hard-break, and embed nodes before mutation. */
  readonly maxInlineNodes?: number;
  /** Matches the browser rich-text runtime's conservative local text byte budget by default. */
  readonly maxTextBytes?: number;
  /** Matches the browser rich-text runtime's conservative local Unicode-scalar budget by default. */
  readonly maxTextRunes?: number;
  /** Bounds the canonical JSON payload of one atomic embed. */
  readonly maxEmbedBytes?: number;
  /** Matches the browser rich-text runtime's accepted editor-operation count by default. */
  readonly maxOperations?: number;
}

/**
 * Binds the fixed `tiptap-core-richtext-v1` projection to an authenticated
 * rich-text v1 document. A local update is diffed against the previous span
 * projection so unchanged RGA positions keep their identities; frame creation
 * remains one atomic `applyEditorDelta` call. Unknown nodes, marks, attrs,
 * embeds, non-canonical remote payloads, or over-budget input fail closed.
 * Local failures restore the last replicated Tiptap projection. A remote
 * profile violation freezes the binding after restoring that safe projection:
 * its host must recover from an authenticated checkpoint because a concurrent
 * CRDT merge cannot be rolled back safely here.
 */
export class TiptapRichTextBinding {
  #runs: readonly RichTextRun[];
  #writing = false;
  #closed = false;
  #failed = false;
  readonly #limits: BindingLimits;
  readonly #codecs: EmbedCodecs;
  readonly #unobserve: () => void;

  constructor(
    readonly document: RichTextEditorDocument,
    private readonly editor: TiptapRichTextPort,
    private readonly options: BindTiptapRichTextOptions,
  ) {
    if (typeof options.onLocalFrame !== "function") {
      throw new CRDTRuntimeError("invalid_binding_options");
    }
    this.#limits = limitsFromOptions(options);
    this.#codecs = new EmbedCodecs(options.embeds ?? [], this.#limits);
    this.#runs = runsFromSpans(document.spans(), this.#codecs, this.#limits);
    const handler = (_event: unknown) => this.#handleEditorChange();
    editor.on("update", handler);
    this.#unobserve = () => editor.off?.("update", handler);
    if (options.initialContent === "editor") {
      this.#replaceDocumentFromEditor();
    } else {
      this.#writeEditor();
    }
  }

  /** Applies one authenticated, Manifest-bound remote rich-text frame. */
  applyRemote(frame: Uint8Array): void {
    this.#assertOpen();
    this.document.applyDelta(frame);
    try {
      this.#runs = runsFromSpans(this.document.spans(), this.#codecs, this.#limits);
      this.#writeEditor();
    } catch (error) {
      // The rich-text core deliberately accepts generic, Manifest-authorized
      // attributes. A host that failed to validate this profile cannot be
      // rolled back safely after a concurrent merge, so freeze this binding,
      // restore the last safe editor projection, and require recovery from an
      // authenticated checkpoint instead of rendering or re-emitting it.
      this.#failed = true;
      this.#unobserve();
      this.#writeEditor();
      throw error;
    }
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

  #handleEditorChange(): void {
    if (this.#closed || this.#failed || this.#writing) {
      return;
    }
    try {
      this.#replaceDocumentFromEditor();
    } catch (error) {
      // An extension, paste, or NodeView can produce a schema-valid Tiptap
      // document outside this profile. Restore the canonical replicated view
      // so it cannot become an editor-only fork.
      this.#writeEditor();
      throw error;
    }
  }

  #replaceDocumentFromEditor(): void {
    this.#assertOpen();
    const next = runsFromTiptapDocument(this.editor.getJSON(), this.#codecs, this.#limits);
    const operations = operationsBetween(this.#runs, next);
    if (operations.length === 0) {
      return;
    }
    if (operations.length > this.#limits.maxOperations) {
      throw new CRDTRuntimeError("resource_limit");
    }
    const frame = this.document.applyEditorDelta(operations);
    this.#runs = runsFromSpans(this.document.spans(), this.#codecs, this.#limits);
    this.#writeEditor();
    this.options.onLocalFrame(frame);
  }

  #writeEditor(): void {
    const content = tiptapDocumentFromRuns(this.#runs, this.#codecs, this.#limits);
    this.#writing = true;
    try {
      const accepted = this.editor.commands.setContent(content as never, {
        emitUpdate: false,
        errorOnInvalidContent: true,
      });
      if (accepted === false) {
        throw new CRDTRuntimeError("binding_write_failed");
      }
    } finally {
      this.#writing = false;
    }
  }

  #assertOpen(): void {
    if (this.#closed || this.#failed) {
      throw new CRDTRuntimeError("binding_closed");
    }
  }
}

export function bindTiptapRichText(
  document: RichTextEditorDocument,
  editor: TiptapRichTextPort,
  options: BindTiptapRichTextOptions,
): TiptapRichTextBinding {
  return new TiptapRichTextBinding(document, editor, options);
}

/**
 * Binds an application-owned ProseMirror JSON port to the Tiptap-compatible
 * profile. It intentionally accepts no raw `EditorState` or Step stream: the
 * host owns schema validation and must filter tagged remote transactions from
 * `observeUpdate` to preserve undo semantics and prevent feedback loops.
 */
export function bindProseMirrorRichText(
  document: RichTextEditorDocument,
  port: ProseMirrorRichTextPort,
  options: BindTiptapRichTextOptions,
): TiptapRichTextBinding {
  if (port === null || typeof port !== "object" || typeof port.readJSON !== "function" || typeof port.replaceJSON !== "function" || typeof port.observeUpdate !== "function") {
    throw new CRDTRuntimeError("invalid_binding_options");
  }
  const subscriptions = new Map<(event: unknown) => void, () => void>();
  const adapter: TiptapRichTextPort = {
    getJSON: () => port.readJSON(),
    commands: {
      setContent(content: never) {
        return port.replaceJSON(content as TiptapJSONNode);
      },
    },
    on(event: "update", listener: (event: unknown) => void): void {
      if (event !== "update") {
        throw new CRDTRuntimeError("invalid_binding_options");
      }
      subscriptions.get(listener)?.();
      subscriptions.set(listener, port.observeUpdate(() => listener(undefined)));
    },
    off(event: "update", listener: (event: unknown) => void): void {
      if (event !== "update") {
        return;
      }
      subscriptions.get(listener)?.();
      subscriptions.delete(listener);
    },
  };
  return bindTiptapRichText(document, adapter, options);
}

class EmbedCodecs {
  readonly #byKind = new Map<string, TiptapEmbedCodec>();
  readonly #byNodeType = new Map<string, TiptapEmbedCodec>();

  constructor(codecs: readonly TiptapEmbedCodec[], private readonly limits: BindingLimits) {
    if (!Array.isArray(codecs)) {
      throw new CRDTRuntimeError("invalid_binding_options");
    }
    for (const codec of codecs) {
      if (!isEmbedCodec(codec) || !validSemanticKind(codec.kind) || !validNodeType(codec.nodeType) ||
        this.#byKind.has(codec.kind) || this.#byNodeType.has(codec.nodeType)) {
        throw new CRDTRuntimeError("invalid_binding_options");
      }
      this.#byKind.set(codec.kind, codec);
      this.#byNodeType.set(codec.nodeType, codec);
    }
  }

  encode(node: TiptapJSONNode): AttributeMap {
    const type = node.type;
    if (typeof type !== "string") {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    const codec = this.#byNodeType.get(type);
    if (codec === undefined || !hasOnlyKeys(node, ["type", "attrs"])) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    const payload = canonicalEmbedPayload(codec.encode(node), this.limits.maxEmbedBytes);
    return { [EMBED_KIND_ATTRIBUTE]: codec.kind, [EMBED_DATA_ATTRIBUTE]: payload };
  }

  decode(attributes: AttributeMap): TiptapJSONNode {
    const kind = attributes[EMBED_KIND_ATTRIBUTE];
    const encoded = attributes[EMBED_DATA_ATTRIBUTE];
    if (kind === undefined || encoded === undefined) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    const codec = this.#byKind.get(kind);
    if (codec === undefined || encoded.length > this.limits.maxEmbedBytes) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    const payload = parseCanonicalEmbedPayload(encoded, this.limits.maxEmbedBytes);
    let node: TiptapJSONNode;
    try {
      node = codec.decode(payload);
    } catch {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    if (!hasOnlyKeys(node, ["type", "attrs"]) || node.type !== codec.nodeType) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    let roundTrip: string;
    try {
      roundTrip = canonicalEmbedPayload(codec.encode(node), this.limits.maxEmbedBytes);
    } catch {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    if (roundTrip !== encoded) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    return node;
  }
}

function limitsFromOptions(options: BindTiptapRichTextOptions): BindingLimits {
  return {
    maxBlocks: boundedOption(options.maxBlocks, DEFAULT_MAX_BLOCKS, DEFAULT_MAX_BLOCKS),
    maxInlineNodes: boundedOption(options.maxInlineNodes, DEFAULT_MAX_INLINE_NODES, DEFAULT_MAX_INLINE_NODES),
    maxTextBytes: boundedOption(options.maxTextBytes, DEFAULT_MAX_TEXT_BYTES, DEFAULT_MAX_TEXT_BYTES),
    maxTextRunes: boundedOption(options.maxTextRunes, DEFAULT_MAX_TEXT_RUNES, DEFAULT_MAX_TEXT_RUNES),
    maxEmbedBytes: boundedOption(options.maxEmbedBytes, DEFAULT_MAX_EMBED_BYTES, DEFAULT_MAX_EMBED_BYTES),
    maxOperations: boundedOption(options.maxOperations, DEFAULT_MAX_OPERATIONS, DEFAULT_MAX_OPERATIONS),
  };
}

function boundedOption(value: number | undefined, fallback: number, maximum: number): number {
  if (value === undefined) {
    return fallback;
  }
  if (!Number.isSafeInteger(value) || value <= 0 || value > maximum) {
    throw new CRDTRuntimeError("invalid_binding_options");
  }
  return value;
}

function runsFromTiptapDocument(source: TiptapJSONNode, codecs: EmbedCodecs, limits: BindingLimits): RichTextRun[] {
  if (!hasOnlyKeys(source, ["type", "content"]) || source.type !== "doc" || !Array.isArray(source.content)) {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  const budget: BindingBudget = { blocks: 0, inlineNodes: 0, textBytes: 0, textRunes: 0, embedBytes: 0 };
  const runs: RichTextRun[] = [];
  if (source.content.length === 0) {
    appendRun(runs, "\n", { [BLOCK_ATTRIBUTE]: "paragraph" });
    return runs;
  }
  for (const rawBlock of source.content) {
    if (++budget.blocks > limits.maxBlocks) {
      throw new CRDTRuntimeError("resource_limit");
    }
    appendBlock(runs, rawBlock, codecs, limits, budget);
  }
  return runs;
}

function appendBlock(
  target: RichTextRun[],
  source: TiptapJSONNode,
  codecs: EmbedCodecs,
  limits: BindingLimits,
  budget: BindingBudget,
): void {
  const descriptor = descriptorFromTiptapBlock(source);
  const attributes: Record<string, string> = { [BLOCK_ATTRIBUTE]: markerFromDescriptor(descriptor) };
  const content = blockInlineContent(source, descriptor);
  appendInlineNodes(target, content, descriptor, attributes, codecs, limits, budget);
  appendRun(target, "\n", attributes);
}

function descriptorFromTiptapBlock(source: TiptapJSONNode): BlockDescriptor {
  if (!isPlainRecord(source) || typeof source.type !== "string") {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  switch (source.type) {
    case "paragraph":
      if (!hasOnlyKeys(source, ["type", "content"]) || !arrayOrUndefined(source.content)) {
        throw new CRDTRuntimeError("unsupported_rich_text");
      }
      return { kind: "paragraph" };
    case "heading": {
      if (!hasOnlyKeys(source, ["type", "attrs", "content"]) || !arrayOrUndefined(source.content) || !isPlainRecord(source.attrs) ||
        !hasOnlyKeys(source.attrs, ["level"]) || !validHeadingLevel(source.attrs.level)) {
        throw new CRDTRuntimeError("unsupported_rich_text");
      }
      return { kind: "heading", level: source.attrs.level };
    }
    case "blockquote":
      if (!hasOnlyKeys(source, ["type", "content"]) || !Array.isArray(source.content) || source.content.length !== 1 ||
        !hasOnlyKeys(source.content[0], ["type", "content"]) || source.content[0].type !== "paragraph" || !arrayOrUndefined(source.content[0].content)) {
        throw new CRDTRuntimeError("unsupported_rich_text");
      }
      return { kind: "quote" };
    case "codeBlock":
      if (!hasOnlyKeys(source, ["type", "content"]) || !arrayOrUndefined(source.content)) {
        throw new CRDTRuntimeError("unsupported_rich_text");
      }
      return { kind: "code" };
    default:
      throw new CRDTRuntimeError("unsupported_rich_text");
  }
}

function blockInlineContent(source: TiptapJSONNode, descriptor: BlockDescriptor): readonly TiptapJSONNode[] {
  if (descriptor.kind === "quote") {
    return source.content?.[0]?.content ?? [];
  }
  return source.content ?? [];
}

function appendInlineNodes(
  target: RichTextRun[],
  source: readonly TiptapJSONNode[],
  descriptor: BlockDescriptor,
  blockAttributes: AttributeMap,
  codecs: EmbedCodecs,
  limits: BindingLimits,
  budget: BindingBudget,
): void {
  for (const node of source) {
    if (++budget.inlineNodes > limits.maxInlineNodes || !isPlainRecord(node) || typeof node.type !== "string") {
      throw new CRDTRuntimeError("resource_limit");
    }
    switch (node.type) {
      case "text": {
        if (!hasOnlyKeys(node, ["type", "text", "marks"]) || typeof node.text !== "string" || !isUnicodeScalarString(node.text)) {
          throw new CRDTRuntimeError("unsupported_rich_text");
        }
        const marks = attributesFromTiptapMarks(node.marks);
        if (descriptor.kind === "code" && Object.keys(marks).length !== 0) {
          throw new CRDTRuntimeError("unsupported_rich_text");
        }
        appendTextWithBreaks(target, node.text, { ...blockAttributes, ...marks }, descriptor.kind, limits, budget);
        break;
      }
      case "hardBreak":
        if (!hasOnlyKeys(node, ["type"]) || descriptor.kind === "code") {
          throw new CRDTRuntimeError("unsupported_rich_text");
        }
        appendRun(target, "\n", { ...blockAttributes, [HARD_BREAK_ATTRIBUTE]: "true" });
        break;
      default: {
        if (descriptor.kind === "code") {
          throw new CRDTRuntimeError("unsupported_rich_text");
        }
        const embed = codecs.encode(node);
        addEmbedBudget(embed[EMBED_DATA_ATTRIBUTE]!, limits, budget);
        appendRun(target, OBJECT_REPLACEMENT_CHARACTER, { ...blockAttributes, ...embed });
      }
    }
  }
}

function appendTextWithBreaks(
  target: RichTextRun[],
  value: string,
  attributes: AttributeMap,
  kind: BlockKind,
  limits: BindingLimits,
  budget: BindingBudget,
): void {
  if (kind !== "code" && (value.includes("\n") || value.includes("\r"))) {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  let start = 0;
  for (let index = 0; index < value.length; index += 1) {
    if (value[index] !== "\n") {
      continue;
    }
    const before = value.slice(start, index);
    addTextBudget(before, limits, budget);
    appendRun(target, before, attributes);
    addTextBudget("\n", limits, budget);
    appendRun(target, "\n", { ...attributes, [HARD_BREAK_ATTRIBUTE]: "true" });
    start = index + 1;
  }
  const trailing = value.slice(start);
  addTextBudget(trailing, limits, budget);
  appendRun(target, trailing, attributes);
}

function attributesFromTiptapMarks(raw: unknown): AttributeMap {
  if (raw === undefined) {
    return {};
  }
  if (!Array.isArray(raw)) {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  const attributes: Record<string, string> = {};
  for (const mark of raw) {
    if (!isPlainRecord(mark) || !hasOnlyKeys(mark, ["type"]) || typeof mark.type !== "string" || !MARK_NAMES.has(mark.type)) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    const key = MARK_ATTRIBUTES[mark.type];
    if (key === undefined || attributes[key] !== undefined) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    attributes[key] = "true";
  }
  return attributes;
}

function runsFromSpans(source: readonly RichTextSpan[], codecs: EmbedCodecs, limits: BindingLimits): RichTextRun[] {
  if (!Array.isArray(source) || source.length > limits.maxInlineNodes) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  const budget: BindingBudget = { blocks: 0, inlineNodes: 0, textBytes: 0, textRunes: 0, embedBytes: 0 };
  const runs: RichTextRun[] = [];
  for (const rawSpan of source) {
    if (!isPlainRecord(rawSpan) || !hasOnlyKeys(rawSpan, ["text", "attributes"]) || typeof rawSpan.text !== "string" ||
      !isPlainRecord(rawSpan.attributes) || !isUnicodeScalarString(rawSpan.text)) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    const attributes = validatedProjectionAttributes(rawSpan.attributes, codecs, limits);
    addTextBudget(rawSpan.text, limits, budget);
    if (attributes[EMBED_DATA_ATTRIBUTE] !== undefined) {
      for (const rune of rawSpan.text) {
        if (rune === OBJECT_REPLACEMENT_CHARACTER) {
          addEmbedBudget(attributes[EMBED_DATA_ATTRIBUTE], limits, budget);
        }
      }
    }
    appendRun(runs, rawSpan.text, attributes);
  }
  // Decode before writing to Tiptap. This prevents a received but unrecognised
  // schema value from being rendered as a different local rich document.
  tiptapDocumentFromRuns(runs, codecs, limits);
  return runs;
}

function validatedProjectionAttributes(source: Record<string, unknown>, codecs: EmbedCodecs, limits: BindingLimits): AttributeMap {
  const marker = source[BLOCK_ATTRIBUTE];
  if (typeof marker !== "string" || parseBlockMarker(marker) === undefined) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  const attributes: Record<string, string> = { [BLOCK_ATTRIBUTE]: marker };
  for (const key of Object.values(MARK_ATTRIBUTES)) {
    const value = source[key];
    if (value === undefined) {
      continue;
    }
    if (value !== "true") {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    attributes[key] = value;
  }
  const hardBreak = source[HARD_BREAK_ATTRIBUTE];
  if (hardBreak !== undefined) {
    if (hardBreak !== "true") {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    attributes[HARD_BREAK_ATTRIBUTE] = hardBreak;
  }
  const kind = source[EMBED_KIND_ATTRIBUTE];
  const data = source[EMBED_DATA_ATTRIBUTE];
  if ((kind === undefined) !== (data === undefined)) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  if (kind !== undefined && data !== undefined) {
    if (typeof kind !== "string" || typeof data !== "string") {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    attributes[EMBED_KIND_ATTRIBUTE] = kind;
    attributes[EMBED_DATA_ATTRIBUTE] = data;
    // Verify the payload now instead of deferring an untrusted render failure.
    codecs.decode(attributes);
  }
  if (Object.keys(source).length !== Object.keys(attributes).length || Object.keys(source).some((key) => !PROFILE_ATTRIBUTE_KEYS.has(key))) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  return attributes;
}

function tiptapDocumentFromRuns(runs: readonly RichTextRun[], codecs: EmbedCodecs, limits: BindingLimits): TiptapJSONNode {
  if (runs.length === 0) {
    return { type: "doc", content: [{ type: "paragraph" }] };
  }
  const blocks: TiptapJSONNode[] = [];
  let marker: string | undefined;
  let descriptor: BlockDescriptor | undefined;
  let content: TiptapJSONNode[] = [];
  for (const run of runs) {
    const currentMarker = run.attributes[BLOCK_ATTRIBUTE];
    const currentDescriptor = currentMarker === undefined ? undefined : parseBlockMarker(currentMarker);
    if (currentDescriptor === undefined) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    for (const rune of run.text) {
      if (marker === undefined) {
        marker = currentMarker;
        descriptor = currentDescriptor;
        content = [];
      } else if (marker !== currentMarker) {
        throw new CRDTRuntimeError("invalid_rich_text_projection");
      }
      const hardBreak = run.attributes[HARD_BREAK_ATTRIBUTE] === "true";
      if (rune === "\n" && !hardBreak) {
        if (!isPlainBlockBoundaryAttributes(run.attributes) || descriptor === undefined || blocks.length >= limits.maxBlocks) {
          throw new CRDTRuntimeError("invalid_rich_text_projection");
        }
        blocks.push(blockFromDescriptor(descriptor, content));
        marker = undefined;
        descriptor = undefined;
        content = [];
        continue;
      }
      appendProjectedInline(content, rune, run.attributes, descriptor, codecs, limits);
    }
  }
  if (marker !== undefined || blocks.length === 0) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  if (inlineNodeCount(blocks) > limits.maxInlineNodes) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  return { type: "doc", content: blocks };
}

function appendProjectedInline(
  target: TiptapJSONNode[],
  rune: string,
  attributes: AttributeMap,
  descriptor: BlockDescriptor | undefined,
  codecs: EmbedCodecs,
  limits: BindingLimits,
): void {
  if (descriptor === undefined) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  const hardBreak = attributes[HARD_BREAK_ATTRIBUTE] === "true";
  const hasEmbed = attributes[EMBED_KIND_ATTRIBUTE] !== undefined;
  if (rune === OBJECT_REPLACEMENT_CHARACTER) {
    if (descriptor.kind === "code" || hardBreak || !hasEmbed || hasAnyMark(attributes)) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    target.push(codecs.decode(attributes));
    return;
  }
  if (hasEmbed) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  if (hardBreak) {
    if (rune !== "\n" || hasAnyMark(attributes)) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    if (descriptor.kind === "code") {
      appendTextNode(target, "\n", []);
    } else {
      target.push({ type: "hardBreak" });
    }
    return;
  }
  if (rune === "\n" || rune === "\r") {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  const marks = tiptapMarksFromAttributes(attributes);
  if (descriptor.kind === "code" && marks.length !== 0) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  appendTextNode(target, rune, marks);
  if (target.length > limits.maxInlineNodes) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
}

function blockFromDescriptor(descriptor: BlockDescriptor, content: readonly TiptapJSONNode[]): TiptapJSONNode {
  switch (descriptor.kind) {
    case "paragraph":
      return content.length === 0 ? { type: "paragraph" } : { type: "paragraph", content };
    case "heading":
      return content.length === 0 ? { type: "heading", attrs: { level: descriptor.level } } : { type: "heading", attrs: { level: descriptor.level }, content };
    case "quote": {
      const paragraph = content.length === 0 ? { type: "paragraph" } : { type: "paragraph", content };
      return { type: "blockquote", content: [paragraph] };
    }
    case "code":
      return content.length === 0 ? { type: "codeBlock" } : { type: "codeBlock", content };
  }
}

function inlineNodeCount(blocks: readonly TiptapJSONNode[]): number {
  let count = 0;
  for (const block of blocks) {
    const source = block.type === "blockquote" ? block.content?.[0]?.content : block.content;
    count += source?.length ?? 0;
  }
  return count;
}

function appendTextNode(target: TiptapJSONNode[], value: string, marks: readonly TiptapJSONNode[]): void {
  const previous = target.at(-1);
  if (previous !== undefined && previous.type === "text" && sameMarks(previous.marks, marks)) {
    target[target.length - 1] = marks.length === 0
      ? { type: "text", text: `${previous.text ?? ""}${value}` }
      : { type: "text", text: `${previous.text ?? ""}${value}`, marks };
    return;
  }
  target.push(marks.length === 0 ? { type: "text", text: value } : { type: "text", text: value, marks });
}

function tiptapMarksFromAttributes(attributes: AttributeMap): TiptapJSONNode[] {
  const marks: TiptapJSONNode[] = [];
  for (const name of Object.keys(MARK_ATTRIBUTES).sort()) {
    const key = MARK_ATTRIBUTES[name];
    if (key !== undefined && attributes[key] === "true") {
      marks.push({ type: name });
    }
  }
  return marks;
}

function isPlainBlockBoundaryAttributes(attributes: AttributeMap): boolean {
  return Object.keys(attributes).length === 1 && attributes[BLOCK_ATTRIBUTE] !== undefined;
}

function hasAnyMark(attributes: AttributeMap): boolean {
  return Object.values(MARK_ATTRIBUTES).some((key) => attributes[key] !== undefined);
}

function markerFromDescriptor(descriptor: BlockDescriptor): string {
  switch (descriptor.kind) {
    case "paragraph":
      return "paragraph";
    case "heading":
      return `heading:${descriptor.level}`;
    case "quote":
      return "quote";
    case "code":
      return "code";
  }
}

function parseBlockMarker(marker: string): BlockDescriptor | undefined {
  switch (marker) {
    case "paragraph":
      return { kind: "paragraph" };
    case "quote":
      return { kind: "quote" };
    case "code":
      return { kind: "code" };
    default:
      if (!marker.startsWith("heading:")) {
        return undefined;
      }
      const level = Number(marker.slice("heading:".length));
      if (!validHeadingLevel(level) || marker !== `heading:${level}`) {
        return undefined;
      }
      return { kind: "heading", level };
  }
}

function operationsBetween(previous: readonly RichTextRun[], next: readonly RichTextRun[]): RichTextEditorOperation[] {
  if (sameRuns(previous, next)) {
    return [];
  }
  const previousRunes = totalRunes(previous);
  const nextRunes = totalRunes(next);
  const prefix = commonPrefixRunes(previous, next);
  const suffix = commonSuffixRunes(previous, next, prefix);
  const operations: RichTextEditorOperation[] = [];
  appendRetainChanges(operations, previous, next, 0, 0, prefix);
  const previousMiddle = previousRunes - prefix - suffix;
  if (previousMiddle > 0) {
    operations.push({ delete: previousMiddle });
  }
  appendInsertions(operations, next, prefix, nextRunes - prefix - suffix);
  appendRetainChanges(operations, previous, next, previousRunes - suffix, nextRunes - suffix, suffix);
  return operations;
}

function appendRetainChanges(
  operations: RichTextEditorOperation[],
  previous: readonly RichTextRun[],
  next: readonly RichTextRun[],
  previousStart: number,
  nextStart: number,
  count: number,
): void {
  if (count === 0) {
    return;
  }
  const oldCursor = new RunSliceCursor(previous, previousStart, count);
  const nextCursor = new RunSliceCursor(next, nextStart, count);
  while (oldCursor.remaining > 0 && nextCursor.remaining > 0) {
    const length = Math.min(oldCursor.remaining, nextCursor.remaining, oldCursor.available, nextCursor.available);
    appendRetain(operations, length, changesBetween(oldCursor.attributes, nextCursor.attributes));
    oldCursor.advance(length);
    nextCursor.advance(length);
  }
}

function appendInsertions(operations: RichTextEditorOperation[], runs: readonly RichTextRun[], start: number, count: number): void {
  if (count === 0) {
    return;
  }
  const cursor = new RunSliceCursor(runs, start, count);
  while (cursor.remaining > 0) {
    const length = Math.min(cursor.remaining, cursor.available);
    appendInsert(operations, cursor.text(length), cursor.attributes);
    cursor.advance(length);
  }
}

function appendRetain(operations: RichTextEditorOperation[], count: number, changes: readonly RichTextAttributeChange[]): void {
  const previous = operations.at(-1);
  if (previous?.retain !== undefined && sameChanges(previous.changes ?? [], changes)) {
    operations[operations.length - 1] = changes.length === 0 ? { retain: previous.retain + count } : { retain: previous.retain + count, changes };
    return;
  }
  operations.push(changes.length === 0 ? { retain: count } : { retain: count, changes });
}

function appendInsert(operations: RichTextEditorOperation[], value: string, attributes: AttributeMap): void {
  const changes = changesFromAttributes(attributes);
  const previous = operations.at(-1);
  if (previous?.insert !== undefined && sameChanges(previous.changes ?? [], changes)) {
    operations[operations.length - 1] = { insert: `${previous.insert}${value}`, changes };
    return;
  }
  operations.push({ insert: value, changes });
}

function changesBetween(previous: AttributeMap, next: AttributeMap): RichTextAttributeChange[] {
  const changes: RichTextAttributeChange[] = [];
  const keys = new Set([...Object.keys(previous), ...Object.keys(next)]);
  for (const key of [...keys].sort()) {
    if (previous[key] === next[key]) {
      continue;
    }
    const value = next[key];
    changes.push(value === undefined ? { key, remove: true } : { key, value });
  }
  return changes;
}

function changesFromAttributes(attributes: AttributeMap): RichTextAttributeChange[] {
  return Object.keys(attributes).sort().map((key) => ({ key, value: attributes[key]! }));
}

class RunSliceCursor {
  #run = 0;
  #offset = 0;
  remaining: number;

  constructor(private readonly runs: readonly RichTextRun[], start: number, count: number) {
    this.remaining = count;
    let skipped = 0;
    while (this.#run < runs.length) {
      const run = runs[this.#run]!;
      if (skipped+run.runes > start) {
        this.#offset = codeUnitOffsetAtRune(run.text, start-skipped);
        break;
      }
      skipped += run.runes;
      this.#run++;
    }
  }

  get attributes(): AttributeMap {
    const run = this.runs[this.#run];
    if (run === undefined) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    return run.attributes;
  }

  get available(): number {
    const run = this.runs[this.#run];
    if (run === undefined) {
      return 0;
    }
    return run.runes-runeCount(run.text.slice(0, this.#offset));
  }

  text(count: number): string {
    const run = this.runs[this.#run];
    if (run === undefined || count <= 0 || count > this.available) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    const end = codeUnitOffsetAtRune(run.text, runeCount(run.text.slice(0, this.#offset))+count);
    return run.text.slice(this.#offset, end);
  }

  advance(count: number): void {
    if (!Number.isSafeInteger(count) || count <= 0 || count > this.available || count > this.remaining) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    const run = this.runs[this.#run]!;
    this.#offset = codeUnitOffsetAtRune(run.text, runeCount(run.text.slice(0, this.#offset))+count);
    this.remaining -= count;
    if (this.#offset === run.text.length) {
      this.#run++;
      this.#offset = 0;
    }
  }
}

function commonPrefixRunes(left: readonly RichTextRun[], right: readonly RichTextRun[]): number {
  const leftCursor = new RuneCursor(left, false);
  const rightCursor = new RuneCursor(right, false);
  let count = 0;
  while (leftCursor.peek() !== undefined && leftCursor.peek() === rightCursor.peek()) {
    leftCursor.advance();
    rightCursor.advance();
    count++;
  }
  return count;
}

function commonSuffixRunes(left: readonly RichTextRun[], right: readonly RichTextRun[], prefix: number): number {
  const maximum = Math.min(totalRunes(left), totalRunes(right))-prefix;
  const leftCursor = new RuneCursor(left, true);
  const rightCursor = new RuneCursor(right, true);
  let count = 0;
  while (count < maximum && leftCursor.peek() !== undefined && leftCursor.peek() === rightCursor.peek()) {
    leftCursor.advance();
    rightCursor.advance();
    count++;
  }
  return count;
}

class RuneCursor {
  #run: number;
  #offset: number;

  constructor(private readonly runs: readonly RichTextRun[], private readonly reverse: boolean) {
    this.#run = reverse ? runs.length-1 : 0;
    this.#offset = reverse && this.#run >= 0 ? runs[this.#run]!.text.length : 0;
    this.#skipEmptyRuns();
  }

  peek(): number | undefined {
    if (this.#run < 0 || this.#run >= this.runs.length) {
      return undefined;
    }
    const text = this.runs[this.#run]!.text;
    return this.reverse ? text.codePointAt(previousCodePointIndex(text, this.#offset)) : text.codePointAt(this.#offset);
  }

  advance(): void {
    if (this.#run < 0 || this.#run >= this.runs.length) {
      return;
    }
    const text = this.runs[this.#run]!.text;
    if (this.reverse) {
      this.#offset = previousCodePointIndex(text, this.#offset);
      if (this.#offset === 0) {
        this.#run--;
        this.#offset = this.#run >= 0 ? this.runs[this.#run]!.text.length : 0;
      }
    } else {
      const point = text.codePointAt(this.#offset);
      this.#offset += point === undefined || point > 0xffff ? 2 : 1;
      if (this.#offset >= text.length) {
        this.#run++;
        this.#offset = 0;
      }
    }
    this.#skipEmptyRuns();
  }

  #skipEmptyRuns(): void {
    while (this.#run >= 0 && this.#run < this.runs.length && this.runs[this.#run]!.text === "") {
      this.#run += this.reverse ? -1 : 1;
      this.#offset = this.reverse && this.#run >= 0 ? this.runs[this.#run]!.text.length : 0;
    }
  }
}

function appendRun(target: RichTextRun[], text: string, attributes: AttributeMap): void {
  if (text === "") {
    return;
  }
  const previous = target.at(-1);
  if (previous !== undefined && sameAttributes(previous.attributes, attributes)) {
    target[target.length-1] = { text: `${previous.text}${text}`, attributes: previous.attributes, runes: previous.runes+runeCount(text) };
    return;
  }
  target.push({ text, attributes: { ...attributes }, runes: runeCount(text) });
}

function sameRuns(left: readonly RichTextRun[], right: readonly RichTextRun[]): boolean {
  return left.length === right.length && left.every((run, index) => run.text === right[index]?.text && sameAttributes(run.attributes, right[index]!.attributes));
}

function sameAttributes(left: AttributeMap, right: AttributeMap): boolean {
  const leftKeys = Object.keys(left);
  return leftKeys.length === Object.keys(right).length && leftKeys.every((key) => left[key] === right[key]);
}

function sameChanges(left: readonly RichTextAttributeChange[], right: readonly RichTextAttributeChange[]): boolean {
  return left.length === right.length && left.every((change, index) => change.key === right[index]?.key && change.value === right[index]?.value && change.remove === right[index]?.remove);
}

function sameMarks(left: unknown, right: readonly TiptapJSONNode[]): boolean {
  if (left === undefined) {
    return right.length === 0;
  }
  return Array.isArray(left) && left.length === right.length && left.every((mark, index) => isPlainRecord(mark) && mark.type === right[index]?.type);
}

function totalRunes(runs: readonly RichTextRun[]): number {
  return runs.reduce((total, run) => total+run.runes, 0);
}

function addTextBudget(value: string, limits: BindingLimits, budget: BindingBudget): void {
  budget.textBytes += new TextEncoder().encode(value).byteLength;
  budget.textRunes += runeCount(value);
  if (budget.textBytes > limits.maxTextBytes || budget.textRunes > limits.maxTextRunes) {
    throw new CRDTRuntimeError("resource_limit");
  }
}

function addEmbedBudget(value: string, limits: BindingLimits, budget: BindingBudget): void {
  const bytes = new TextEncoder().encode(value).byteLength;
  if (bytes > limits.maxEmbedBytes) {
    throw new CRDTRuntimeError("resource_limit");
  }
  budget.embedBytes += bytes;
  if (budget.embedBytes > limits.maxEmbedBytes) {
    throw new CRDTRuntimeError("resource_limit");
  }
}

function canonicalEmbedPayload(value: unknown, maxBytes: number): string {
  const normalized = canonicalJSONValue(value, 0);
  if (!isPlainRecord(normalized)) {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  const encoded = JSON.stringify(normalized);
  if (new TextEncoder().encode(encoded).byteLength > maxBytes) {
    throw new CRDTRuntimeError("resource_limit");
  }
  return encoded;
}

function parseCanonicalEmbedPayload(value: string, maxBytes: number): Readonly<Record<string, TiptapEmbedValue>> {
  if (new TextEncoder().encode(value).byteLength > maxBytes) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  let canonical: string;
  try {
    canonical = canonicalEmbedPayload(parsed, maxBytes);
  } catch {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  if (canonical !== value || !isPlainRecord(parsed)) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  return parsed as Readonly<Record<string, TiptapEmbedValue>>;
}

function canonicalJSONValue(value: unknown, depth: number): TiptapEmbedValue {
  if (depth > MAX_JSON_DEPTH) {
    throw new CRDTRuntimeError("resource_limit");
  }
  if (value === null || typeof value === "boolean" || typeof value === "string") {
    return value;
  }
  if (typeof value === "number") {
    if (!Number.isFinite(value)) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    return value;
  }
  if (Array.isArray(value)) {
    return value.map((entry) => canonicalJSONValue(entry, depth+1));
  }
  if (!isPlainRecord(value)) {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  const result: Record<string, TiptapEmbedValue> = Object.create(null) as Record<string, TiptapEmbedValue>;
  for (const key of Object.keys(value).sort()) {
    result[key] = canonicalJSONValue(value[key], depth+1);
  }
  return result;
}

function validHeadingLevel(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 1 && value <= 6;
}

function validSemanticKind(value: string): boolean {
  return value.length > 0 && value.length <= 128 && /^[a-z0-9._-]+$/.test(value);
}

function validNodeType(value: string): boolean {
  return value.length > 0 && value.length <= 128 && /^[A-Za-z][A-Za-z0-9_-]*$/.test(value);
}

function isEmbedCodec(value: unknown): value is TiptapEmbedCodec {
  return isPlainRecord(value) && typeof value.kind === "string" && typeof value.nodeType === "string" &&
    typeof value.encode === "function" && typeof value.decode === "function";
}

function isUnicodeScalarString(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (code >= 0xd800 && code <= 0xdbff) {
      if (index+1 >= value.length) {
        return false;
      }
      const trail = value.charCodeAt(index+1);
      if (trail < 0xdc00 || trail > 0xdfff) {
        return false;
      }
      index++;
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      return false;
    }
  }
  return true;
}

function runeCount(value: string): number {
  return Array.from(value).length;
}

function codeUnitOffsetAtRune(value: string, offset: number): number {
  if (!Number.isSafeInteger(offset) || offset < 0) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  let runes = 0;
  let codeUnits = 0;
  for (const rune of value) {
    if (runes === offset) {
      return codeUnits;
    }
    codeUnits += rune.length;
    runes++;
  }
  if (runes === offset) {
    return codeUnits;
  }
  throw new CRDTRuntimeError("invalid_rich_text_projection");
}

function previousCodePointIndex(value: string, offset: number): number {
  if (offset <= 0) {
    return 0;
  }
  const index = offset-1;
  return index > 0 && value.charCodeAt(index) >= 0xdc00 && value.charCodeAt(index) <= 0xdfff &&
    value.charCodeAt(index-1) >= 0xd800 && value.charCodeAt(index-1) <= 0xdbff ? index-1 : index;
}

function arrayOrUndefined(value: unknown): value is readonly TiptapJSONNode[] | undefined {
  return value === undefined || Array.isArray(value);
}

function hasOnlyKeys(value: unknown, allowed: readonly string[]): value is TiptapJSONNode {
  return isPlainRecord(value) && Object.keys(value).every((key) => allowed.includes(key));
}

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value) && (Object.getPrototypeOf(value) === Object.prototype || Object.getPrototypeOf(value) === null);
}
