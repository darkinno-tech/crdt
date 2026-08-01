/**
 * A bounded, schema-specific BlockNote text-block binding.
 *
 * BlockNote's public document API contains more than text: custom blocks,
 * tables, media, links, and arbitrary inline content are all valid there.
 * This adapter deliberately accepts only the reversible default text-block
 * subset. It never turns an unknown block into text. Rich document objects
 * belong in a separately negotiated documenttree group instead.
 */

import { CRDTRuntimeError } from "./wasm.js";
import type {
  RGAFrameListener,
  RichTextAttributeChange,
  RichTextEditorDocument,
  RichTextEditorOperation,
  RichTextSpan,
} from "./bindings.js";

/** Bind this exact application schema ID in the authenticated rich-text Manifest. */
export const BLOCKNOTE_RICH_TEXT_SCHEMA_ID = "darkinno:blocknote-text-v1";

const BLOCK_ATTRIBUTE = "rt.block";
const MARKER_PREFIX = "bn1";
const DEFAULT_MAX_BLOCKS = 4_096;
const DEFAULT_MAX_BLOCK_DEPTH = 64;
const DEFAULT_MAX_INLINE_CONTENT = 16_384;
const DEFAULT_MAX_TEXT_BYTES = 1 << 20;
const DEFAULT_MAX_TEXT_RUNES = 1 << 18;

const COLORS = new Set([
  "default",
  "gray",
  "brown",
  "red",
  "orange",
  "yellow",
  "green",
  "blue",
  "purple",
  "pink",
]);
const ALIGNMENTS = new Set(["left", "center", "right", "justify"]);
const BLOCK_TYPES = new Set([
  "paragraph",
  "heading",
  "bulletListItem",
  "numberedListItem",
  "checkListItem",
  "toggleListItem",
  "quote",
  "codeBlock",
]);
const BOOLEAN_STYLES = ["bold", "italic", "underline", "strike", "code"] as const;
const STYLE_ATTRIBUTES = ["rt.bold", "rt.italic", "rt.underline", "rt.strike", "rt.code", "rt.textColor", "rt.backgroundColor"] as const;

type BlockType = "paragraph" | "heading" | "bulletListItem" | "numberedListItem" | "checkListItem" | "toggleListItem" | "quote" | "codeBlock";
type AttributeMap = Readonly<Record<string, string>>;

interface RichTextRun {
  readonly text: string;
  readonly attributes: AttributeMap;
  readonly runes: number;
}

interface BindingLimits {
  readonly maxBlocks: number;
  readonly maxBlockDepth: number;
  readonly maxInlineContent: number;
  readonly maxTextBytes: number;
  readonly maxTextRunes: number;
}

interface BindingBudget {
  blocks: number;
  inlineContent: number;
  textBytes: number;
  textRunes: number;
}

interface BlockDescriptor {
  readonly type: BlockType;
  readonly depth: number;
  readonly props: Readonly<Record<string, unknown>>;
}

/**
 * The deliberately small structural subset of BlockNote's public editor API.
 * It is compatible with `BlockNoteEditor` without making BlockNote a runtime
 * dependency of this package. The binding validates `document` as `unknown`
 * before looking at any field, so custom schemas cannot bypass its boundary.
 */
export interface BlockNoteRichTextPort {
  readonly document: readonly unknown[];
  // `any[]` keeps the interface structurally compatible with BlockNote's
  // generic BlockIdentifier[]/PartialBlock[] public method. Values passed by
  // this binding are fully validated, local partial blocks.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  replaceBlocks(blocksToRemove: any[], blocksToInsert: any[]): unknown;
  onChange(callback: () => void): () => void;
}

export interface BindBlockNoteRichTextOptions {
  /** Receives one canonical rich-text frame for each accepted local transaction. */
  readonly onLocalFrame: RGAFrameListener;
  /** Import the current BlockNote document once, otherwise render the CRDT projection. */
  readonly initialContent?: "document" | "editor";
  /** Bounds all source blocks, including nested blocks, before conversion. */
  readonly maxBlocks?: number;
  /** Bounds nesting reconstructed from BlockNote children and CRDT block markers. */
  readonly maxBlockDepth?: number;
  /** Bounds inline text runs before conversion into rich-text operations. */
  readonly maxInlineContent?: number;
  /** Bounds the BlockNote projection before it can allocate a large local transaction. */
  readonly maxTextBytes?: number;
  /** Bounds Unicode scalar positions used by the rich-text protocol. */
  readonly maxTextRunes?: number;
}

/**
 * Binds BlockNote's default text blocks to a `blocknote-text-v1` rich-text
 * document. Paragraphs, headings, bullet/numbered/check/toggle lists, quotes,
 * code blocks, nesting, default colors/alignment, and default inline styles
 * round-trip. Links, tables, files, media, custom blocks, and arbitrary props
 * fail closed instead of being flattened or serialised as CRDT identities.
 */
export class BlockNoteRichTextBinding {
  #runs: readonly RichTextRun[];
  #writing = false;
  #closed = false;
  readonly #limits: BindingLimits;
  readonly #unobserve: () => void;

  constructor(
    readonly document: RichTextEditorDocument,
    private readonly editor: BlockNoteRichTextPort,
    private readonly options: BindBlockNoteRichTextOptions,
  ) {
    if (typeof options.onLocalFrame !== "function") {
      throw new CRDTRuntimeError("invalid_binding_options");
    }
    this.#limits = limitsFromOptions(options);
    this.#runs = runsFromSpans(document.spans(), this.#limits);
    this.#unobserve = editor.onChange(() => this.#handleEditorChange());
    if (options.initialContent === "editor") {
      this.#replaceDocumentFromEditor();
    } else {
      this.#writeEditor(blocksFromRuns(this.#runs, this.#limits));
    }
  }

  /** Applies one authenticated, Manifest-bound remote rich-text frame. */
  applyRemote(frame: Uint8Array): void {
    this.#assertOpen();
    this.document.applyDelta(frame);
    this.#runs = runsFromSpans(this.document.spans(), this.#limits);
    this.#writeEditor(blocksFromRuns(this.#runs, this.#limits));
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
    if (this.#closed || this.#writing) {
      return;
    }
    try {
      this.#replaceDocumentFromEditor();
    } catch (error) {
      // A plugin, paste, or custom BlockNote schema may have produced a shape
      // outside blocknote-text-v1. Restore the last replicated projection so
      // it cannot remain a local-only document fork.
      this.#writeEditor(blocksFromRuns(this.#runs, this.#limits));
      throw error;
    }
  }

  #replaceDocumentFromEditor(): void {
    this.#assertOpen();
    const next = runsFromBlockNoteDocument(this.editor.document, this.#limits);
    const operations = operationsBetween(this.#runs, next);
    if (operations.length === 0) {
      return;
    }
    const frame = this.document.applyEditorDelta(operations);
    this.#runs = runsFromSpans(this.document.spans(), this.#limits);
    this.options.onLocalFrame(frame);
  }

  #writeEditor(blocks: readonly unknown[]): void {
    this.#writing = true;
    try {
      this.editor.replaceBlocks([...this.editor.document], [...blocks]);
    } finally {
      this.#writing = false;
    }
  }

  #assertOpen(): void {
    if (this.#closed) {
      throw new CRDTRuntimeError("binding_closed");
    }
  }
}

export function bindBlockNoteRichText(
  document: RichTextEditorDocument,
  editor: BlockNoteRichTextPort,
  options: BindBlockNoteRichTextOptions,
): BlockNoteRichTextBinding {
  return new BlockNoteRichTextBinding(document, editor, options);
}

function limitsFromOptions(options: BindBlockNoteRichTextOptions): BindingLimits {
  return {
    maxBlocks: boundedOption(options.maxBlocks, DEFAULT_MAX_BLOCKS, DEFAULT_MAX_BLOCKS),
    maxBlockDepth: boundedOption(options.maxBlockDepth, DEFAULT_MAX_BLOCK_DEPTH, DEFAULT_MAX_BLOCK_DEPTH),
    maxInlineContent: boundedOption(options.maxInlineContent, DEFAULT_MAX_INLINE_CONTENT, DEFAULT_MAX_INLINE_CONTENT),
    maxTextBytes: boundedOption(options.maxTextBytes, DEFAULT_MAX_TEXT_BYTES, DEFAULT_MAX_TEXT_BYTES),
    maxTextRunes: boundedOption(options.maxTextRunes, DEFAULT_MAX_TEXT_RUNES, DEFAULT_MAX_TEXT_RUNES),
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

function runsFromBlockNoteDocument(source: readonly unknown[], limits: BindingLimits): RichTextRun[] {
  if (!Array.isArray(source) || source.length === 0) {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  const budget: BindingBudget = { blocks: 0, inlineContent: 0, textBytes: 0, textRunes: 0 };
  const runs: RichTextRun[] = [];
  appendBlocks(runs, source, 0, limits, budget);
  return runs;
}

function appendBlocks(
  runs: RichTextRun[],
  source: readonly unknown[],
  depth: number,
  limits: BindingLimits,
  budget: BindingBudget,
): void {
  if (depth > limits.maxBlockDepth) {
    throw new CRDTRuntimeError("resource_limit");
  }
  for (const rawBlock of source) {
    if (++budget.blocks > limits.maxBlocks) {
      throw new CRDTRuntimeError("resource_limit");
    }
    const block = blockRecord(rawBlock, depth);
    const markerAttributes: AttributeMap = { [BLOCK_ATTRIBUTE]: markerFromDescriptor(block) };
    appendInlineContent(runs, rawBlock, markerAttributes, limits, budget);
    appendRun(runs, "\n", markerAttributes);
    const children = objectField(rawBlock, "children");
    if (!Array.isArray(children)) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    appendBlocks(runs, children, depth + 1, limits, budget);
  }
}

function appendInlineContent(
  runs: RichTextRun[],
  rawBlock: unknown,
  markerAttributes: AttributeMap,
  limits: BindingLimits,
  budget: BindingBudget,
): void {
  const content = objectField(rawBlock, "content");
  if (!Array.isArray(content)) {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  for (const rawInline of content) {
    if (++budget.inlineContent > limits.maxInlineContent || !isPlainRecord(rawInline) || !hasOnlyKeys(rawInline, ["type", "text", "styles"]) || rawInline.type !== "text" ||
      typeof rawInline.text !== "string" || !isPlainRecord(rawInline.styles)) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    if (rawInline.text.includes("\n") || rawInline.text.includes("\r")) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    if (!isUnicodeScalarString(rawInline.text)) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    const attributes = attributesFromBlockNoteStyles(rawInline.styles, markerAttributes);
    addTextBudget(rawInline.text, limits, budget);
    appendRun(runs, rawInline.text, attributes);
  }
}

function blockRecord(rawBlock: unknown, depth: number): BlockDescriptor {
  if (!isPlainRecord(rawBlock) || !hasOnlyKeys(rawBlock, ["id", "type", "props", "content", "children"]) ||
    typeof rawBlock.type !== "string" || !BLOCK_TYPES.has(rawBlock.type) || !isPlainRecord(rawBlock.props) ||
    (rawBlock.id !== undefined && (typeof rawBlock.id !== "string" || rawBlock.id.length > 256))) {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  const type = rawBlock.type as BlockType;
  const props = propsForBlock(type, rawBlock.props);
  return { type, depth, props };
}

function propsForBlock(type: BlockType, source: Record<string, unknown>): Readonly<Record<string, unknown>> {
  switch (type) {
    case "paragraph":
    case "bulletListItem":
    case "numberedListItem":
    case "toggleListItem":
      return commonTextProps(source, false);
    case "checkListItem": {
      const common = commonTextProps(source, true);
      if (typeof common.checked !== "boolean") {
        throw new CRDTRuntimeError("unsupported_rich_text");
      }
      return common;
    }
    case "heading": {
      if (!hasOnlyKeys(source, ["backgroundColor", "textColor", "textAlignment", "level", "isToggleable"]) ||
        !isColor(source.backgroundColor) || !isColor(source.textColor) || !isAlignment(source.textAlignment) ||
        typeof source.level !== "number" || !Number.isSafeInteger(source.level) || source.level < 1 || source.level > 6 || typeof source.isToggleable !== "boolean") {
        throw new CRDTRuntimeError("unsupported_rich_text");
      }
      return {
        backgroundColor: source.backgroundColor,
        textColor: source.textColor,
        textAlignment: source.textAlignment,
        level: source.level,
        isToggleable: source.isToggleable,
      };
    }
    case "quote":
      if (!hasOnlyKeys(source, ["backgroundColor", "textColor"]) || !isColor(source.backgroundColor) || !isColor(source.textColor)) {
        throw new CRDTRuntimeError("unsupported_rich_text");
      }
      return { backgroundColor: source.backgroundColor, textColor: source.textColor };
    case "codeBlock":
      if (!hasOnlyKeys(source, ["language"]) || (source.language !== undefined && !isLanguage(source.language))) {
        throw new CRDTRuntimeError("unsupported_rich_text");
      }
      return source.language === undefined ? {} : { language: source.language };
  }
}

function commonTextProps(source: Record<string, unknown>, checked: boolean): Readonly<Record<string, unknown>> {
  const keys = checked ? ["backgroundColor", "textColor", "textAlignment", "checked"] : ["backgroundColor", "textColor", "textAlignment"];
  if (!hasOnlyKeys(source, keys) || !isColor(source.backgroundColor) || !isColor(source.textColor) || !isAlignment(source.textAlignment) ||
    (checked && typeof source.checked !== "boolean")) {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  return checked
    ? { backgroundColor: source.backgroundColor, textColor: source.textColor, textAlignment: source.textAlignment, checked: source.checked }
    : { backgroundColor: source.backgroundColor, textColor: source.textColor, textAlignment: source.textAlignment };
}

function markerFromDescriptor(block: BlockDescriptor): string {
  const props = block.props;
  const shared = [
    MARKER_PREFIX,
    block.type,
    String(block.depth),
    stringOrDash(props.backgroundColor),
    stringOrDash(props.textColor),
    stringOrDash(props.textAlignment),
    numberOrDash(props.level),
    booleanOrDash(props.checked),
    booleanOrDash(props.isToggleable),
    stringOrDash(props.language),
  ];
  return shared.join(":");
}

function attributesFromBlockNoteStyles(styles: Record<string, unknown>, marker: AttributeMap): AttributeMap {
  if (!hasOnlyKeys(styles, ["bold", "italic", "underline", "strike", "code", "textColor", "backgroundColor"])) {
    throw new CRDTRuntimeError("unsupported_rich_text");
  }
  const attributes: Record<string, string> = { [BLOCK_ATTRIBUTE]: marker[BLOCK_ATTRIBUTE]! };
  for (const style of BOOLEAN_STYLES) {
    if (styles[style] === undefined) {
      continue;
    }
    if (styles[style] !== true) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    attributes[`rt.${style}`] = "true";
  }
  if (styles.textColor !== undefined) {
    if (!isColor(styles.textColor)) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    attributes["rt.textColor"] = styles.textColor;
  }
  if (styles.backgroundColor !== undefined) {
    if (!isColor(styles.backgroundColor)) {
      throw new CRDTRuntimeError("unsupported_rich_text");
    }
    attributes["rt.backgroundColor"] = styles.backgroundColor;
  }
  return attributes;
}

function runsFromSpans(source: readonly RichTextSpan[], limits: BindingLimits): RichTextRun[] {
  if (!Array.isArray(source)) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  const budget: BindingBudget = { blocks: 0, inlineContent: 0, textBytes: 0, textRunes: 0 };
  const runs: RichTextRun[] = [];
  for (const rawSpan of source) {
    if (!isPlainRecord(rawSpan) || !hasOnlyKeys(rawSpan, ["text", "attributes"]) || typeof rawSpan.text !== "string" || !isPlainRecord(rawSpan.attributes)) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    if (!isUnicodeScalarString(rawSpan.text)) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    const attributes = validatedProjectionAttributes(rawSpan.attributes);
    addTextBudget(rawSpan.text, limits, budget);
    appendRun(runs, rawSpan.text, attributes);
  }
  // Decode once here, before any write to BlockNote, so an unrecognised remote
  // schema cannot be rendered as a different local document.
  blocksFromRuns(runs, limits);
  return runs;
}

function validatedProjectionAttributes(source: Record<string, unknown>): AttributeMap {
  if (Object.keys(source).length === 0 || typeof source[BLOCK_ATTRIBUTE] !== "string") {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  const attributes: Record<string, string> = { [BLOCK_ATTRIBUTE]: source[BLOCK_ATTRIBUTE] };
  for (const key of STYLE_ATTRIBUTES) {
    const value = source[key];
    if (value === undefined) {
      continue;
    }
    if (typeof value !== "string") {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    if (key === "rt.textColor" || key === "rt.backgroundColor") {
      if (!isColor(value)) {
        throw new CRDTRuntimeError("invalid_rich_text_projection");
      }
    } else if (value !== "true") {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    attributes[key] = value;
  }
  if (Object.keys(source).length !== Object.keys(attributes).length) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  return attributes;
}

function blocksFromRuns(runs: readonly RichTextRun[], limits: BindingLimits): readonly unknown[] {
  if (runs.length === 0) {
    return [partialBlock({ type: "paragraph", depth: 0, props: defaultTextProps() }, [])];
  }
  const flat: Array<{ descriptor: BlockDescriptor; content: Array<Record<string, unknown>> }> = [];
  let marker: string | undefined;
  let descriptor: BlockDescriptor | undefined;
  let content: Array<Record<string, unknown>> = [];
  let finished = false;

  for (const run of runs) {
    if (run.text.includes("\r")) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    const runMarker = run.attributes[BLOCK_ATTRIBUTE];
    if (runMarker === undefined) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    const runDescriptor = descriptorFromMarker(runMarker, limits);
    const styles = blockNoteStylesFromAttributes(run.attributes);
    let cursor = 0;
    while (cursor < run.text.length) {
      const newline = run.text.indexOf("\n", cursor);
      const end = newline === -1 ? run.text.length : newline;
      const text = run.text.slice(cursor, end);
      if (marker === undefined) {
        marker = runMarker;
        descriptor = runDescriptor;
        content = [];
      } else if (marker !== runMarker) {
        throw new CRDTRuntimeError("invalid_rich_text_projection");
      }
      if (text !== "") {
        appendInlineText(content, text, styles);
      }
      if (newline === -1) {
        finished = false;
        break;
      }
      if (Object.keys(styles).length !== 0 || descriptor === undefined) {
        throw new CRDTRuntimeError("invalid_rich_text_projection");
      }
      flat.push({ descriptor, content });
      marker = undefined;
      descriptor = undefined;
      content = [];
      cursor = newline + 1;
      finished = true;
    }
  }
  if (!finished || marker !== undefined || flat.length === 0 || flat.length > limits.maxBlocks) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  return nestedBlocks(flat, limits);
}

function nestedBlocks(
  flat: ReadonlyArray<{ descriptor: BlockDescriptor; content: Array<Record<string, unknown>> }>,
  limits: BindingLimits,
): readonly unknown[] {
  const roots: Array<Record<string, unknown>> = [];
  const ancestors: Array<Record<string, unknown> | undefined> = [];
  for (const value of flat) {
    const { descriptor } = value;
    if (descriptor.depth > limits.maxBlockDepth) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    const block = partialBlock(descriptor, value.content);
    if (descriptor.depth === 0) {
      roots.push(block);
    } else {
      const parent = ancestors[descriptor.depth - 1];
      if (parent === undefined) {
        throw new CRDTRuntimeError("invalid_rich_text_projection");
      }
      (parent.children as Array<Record<string, unknown>>).push(block);
    }
    ancestors.length = descriptor.depth;
    ancestors[descriptor.depth] = block;
  }
  return roots;
}

function partialBlock(descriptor: BlockDescriptor, content: Array<Record<string, unknown>>): Record<string, unknown> {
  return { type: descriptor.type, props: { ...descriptor.props }, content, children: [] };
}

function descriptorFromMarker(marker: string, limits: BindingLimits): BlockDescriptor {
  const values = marker.split(":");
  if (values.length !== 10 || values[0] !== MARKER_PREFIX || values[1] === undefined || !BLOCK_TYPES.has(values[1])) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  const type = values[1] as BlockType;
  const depth = positiveOrZero(values[2]);
  if (depth === undefined || depth > limits.maxBlockDepth) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  const [backgroundColor, textColor, textAlignment, level, checked, isToggleable, language] = values.slice(3);
  const props = propsFromMarker(type, backgroundColor, textColor, textAlignment, level, checked, isToggleable, language);
  return { type, depth, props };
}

function propsFromMarker(
  type: BlockType,
  backgroundColor: string | undefined,
  textColor: string | undefined,
  textAlignment: string | undefined,
  level: string | undefined,
  checked: string | undefined,
  isToggleable: string | undefined,
  language: string | undefined,
): Readonly<Record<string, unknown>> {
  if (backgroundColor === undefined || textColor === undefined || textAlignment === undefined || level === undefined || checked === undefined || isToggleable === undefined || language === undefined) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  switch (type) {
    case "paragraph":
    case "bulletListItem":
    case "numberedListItem":
    case "toggleListItem":
      if (!isColor(backgroundColor) || !isColor(textColor) || !isAlignment(textAlignment) || level !== "-" || checked !== "-" || isToggleable !== "-" || language !== "-") {
        throw new CRDTRuntimeError("invalid_rich_text_projection");
      }
      return { backgroundColor, textColor, textAlignment };
    case "checkListItem": {
      if (!isColor(backgroundColor) || !isColor(textColor) || !isAlignment(textAlignment) || level !== "-" || isToggleable !== "-" || language !== "-") {
        throw new CRDTRuntimeError("invalid_rich_text_projection");
      }
      const check = markerBoolean(checked);
      if (check === undefined) {
        throw new CRDTRuntimeError("invalid_rich_text_projection");
      }
      return { backgroundColor, textColor, textAlignment, checked: check };
    }
    case "heading": {
      const headingLevel = positiveOrZero(level);
      const toggleable = markerBoolean(isToggleable);
      if (!isColor(backgroundColor) || !isColor(textColor) || !isAlignment(textAlignment) || headingLevel === undefined || headingLevel < 1 || headingLevel > 6 ||
        toggleable === undefined || checked !== "-" || language !== "-") {
        throw new CRDTRuntimeError("invalid_rich_text_projection");
      }
      return { backgroundColor, textColor, textAlignment, level: headingLevel, isToggleable: toggleable };
    }
    case "quote":
      if (!isColor(backgroundColor) || !isColor(textColor) || textAlignment !== "-" || level !== "-" || checked !== "-" || isToggleable !== "-" || language !== "-") {
        throw new CRDTRuntimeError("invalid_rich_text_projection");
      }
      return { backgroundColor, textColor };
    case "codeBlock":
      if (backgroundColor !== "-" || textColor !== "-" || textAlignment !== "-" || level !== "-" || checked !== "-" || isToggleable !== "-" ||
        (language !== "-" && !isLanguage(language))) {
        throw new CRDTRuntimeError("invalid_rich_text_projection");
      }
      return language === "-" ? {} : { language };
  }
}

function blockNoteStylesFromAttributes(attributes: AttributeMap): Record<string, unknown> {
  const styles: Record<string, unknown> = {};
  for (const style of BOOLEAN_STYLES) {
    if (attributes[`rt.${style}`] === "true") {
      styles[style] = true;
    }
  }
  if (attributes["rt.textColor"] !== undefined) {
    styles.textColor = attributes["rt.textColor"];
  }
  if (attributes["rt.backgroundColor"] !== undefined) {
    styles.backgroundColor = attributes["rt.backgroundColor"];
  }
  return styles;
}

function appendInlineText(target: Array<Record<string, unknown>>, text: string, styles: Record<string, unknown>): void {
  const previous = target.at(-1);
  if (previous !== undefined && isPlainRecord(previous.styles) && sameUnknownRecord(previous.styles, styles)) {
    previous.text = `${previous.text as string}${text}`;
    return;
  }
  target.push({ type: "text", text, styles: { ...styles } });
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
  const newCursor = new RunSliceCursor(next, nextStart, count);
  while (oldCursor.remaining > 0 && newCursor.remaining > 0) {
    const length = Math.min(oldCursor.remaining, newCursor.remaining, oldCursor.available, newCursor.available);
    const changes = changesBetween(oldCursor.attributes, newCursor.attributes);
    appendRetain(operations, length, changes);
    oldCursor.advance(length);
    newCursor.advance(length);
  }
}

function appendInsertions(operations: RichTextEditorOperation[], runs: readonly RichTextRun[], start: number, count: number): void {
  if (count === 0) {
    return;
  }
  const cursor = new RunSliceCursor(runs, start, count);
  while (cursor.remaining > 0) {
    const length = Math.min(cursor.remaining, cursor.available);
    const value = cursor.text(length);
    appendInsert(operations, value, cursor.attributes);
    cursor.advance(length);
  }
}

function appendRetain(operations: RichTextEditorOperation[], count: number, changes: readonly RichTextAttributeChange[]): void {
  const previous = operations.at(-1);
  if (previous?.retain !== undefined && sameChanges(previous.changes ?? [], changes)) {
    operations[operations.length - 1] = changes.length === 0
      ? { retain: previous.retain + count }
      : { retain: previous.retain + count, changes };
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
  for (const key of [BLOCK_ATTRIBUTE, ...STYLE_ATTRIBUTES]) {
    if (previous[key] === next[key]) {
      continue;
    }
    if (next[key] === undefined) {
      changes.push({ key, remove: true });
    } else {
      changes.push({ key, value: next[key] });
    }
  }
  return changes;
}

function changesFromAttributes(attributes: AttributeMap): RichTextAttributeChange[] {
  const changes: RichTextAttributeChange[] = [];
  for (const key of [BLOCK_ATTRIBUTE, ...STYLE_ATTRIBUTES]) {
    if (attributes[key] !== undefined) {
      changes.push({ key, value: attributes[key] });
    }
  }
  return changes;
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
  const maximum = Math.min(totalRunes(left), totalRunes(right)) - prefix;
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
    this.#run = reverse ? runs.length - 1 : 0;
    this.#offset = reverse && this.#run >= 0 ? runs[this.#run]!.text.length : 0;
    this.#skipEmptyRuns();
  }

  peek(): number | undefined {
    if (this.#run < 0 || this.#run >= this.runs.length) {
      return undefined;
    }
    const text = this.runs[this.#run]!.text;
    if (this.reverse) {
      const index = previousCodePointIndex(text, this.#offset);
      return index < 0 ? undefined : text.codePointAt(index);
    }
    return text.codePointAt(this.#offset);
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

class RunSliceCursor {
  #runIndex = 0;
  #offsetInRun = 0;
  remaining: number;

  constructor(private readonly runs: readonly RichTextRun[], start: number, count: number) {
    this.remaining = count;
    let skipped = 0;
    while (this.#runIndex < runs.length && skipped + runs[this.#runIndex]!.runes <= start) {
      skipped += runs[this.#runIndex]!.runes;
      this.#runIndex++;
    }
    this.#offsetInRun = start - skipped;
    if (count > 0 && this.#runIndex >= runs.length) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
  }

  get attributes(): AttributeMap {
    return this.runs[this.#runIndex]!.attributes;
  }

  get available(): number {
    return this.runs[this.#runIndex]!.runes - this.#offsetInRun;
  }

  text(count: number): string {
    const run = this.runs[this.#runIndex]!;
    if (count > run.runes - this.#offsetInRun) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    return sliceRunes(run.text, this.#offsetInRun, count);
  }

  advance(count: number): void {
    if (!Number.isSafeInteger(count) || count <= 0 || count > this.remaining) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    const run = this.runs[this.#runIndex]!;
    if (count > run.runes - this.#offsetInRun) {
      throw new CRDTRuntimeError("invalid_rich_text_projection");
    }
    this.#offsetInRun += count;
    this.remaining -= count;
    if (this.#offsetInRun === run.runes && this.remaining > 0) {
      this.#runIndex++;
      this.#offsetInRun = 0;
    }
  }
}

function appendRun(target: RichTextRun[], text: string, attributes: AttributeMap): void {
  if (text === "") {
    return;
  }
  const runes = runeLength(text);
  const previous = target.at(-1);
  if (previous !== undefined && sameAttributes(previous.attributes, attributes)) {
    target[target.length - 1] = { text: `${previous.text}${text}`, attributes: previous.attributes, runes: previous.runes + runes };
    return;
  }
  target.push({ text, attributes, runes });
}

function addTextBudget(text: string, limits: BindingLimits, budget: BindingBudget): void {
  const bytes = utf8ByteLength(text);
  const runes = runeLength(text);
  if (bytes > limits.maxTextBytes - budget.textBytes || runes > limits.maxTextRunes - budget.textRunes) {
    throw new CRDTRuntimeError("resource_limit");
  }
  budget.textBytes += bytes;
  budget.textRunes += runes;
}

function sameRuns(left: readonly RichTextRun[], right: readonly RichTextRun[]): boolean {
  return left.length === right.length && left.every((run, index) => run.text === right[index]?.text && sameAttributes(run.attributes, right[index]!.attributes));
}

function sameAttributes(left: AttributeMap, right: AttributeMap): boolean {
  const leftKeys = Object.keys(left);
  return leftKeys.length === Object.keys(right).length && leftKeys.every((key) => left[key] === right[key]);
}

function sameUnknownRecord(left: Record<string, unknown>, right: Record<string, unknown>): boolean {
  const leftKeys = Object.keys(left);
  return leftKeys.length === Object.keys(right).length && leftKeys.every((key) => left[key] === right[key]);
}

function sameChanges(left: readonly RichTextAttributeChange[], right: readonly RichTextAttributeChange[]): boolean {
  return left.length === right.length && left.every((change, index) => change.key === right[index]?.key && change.value === right[index]?.value && change.remove === right[index]?.remove);
}

function totalRunes(runs: readonly RichTextRun[]): number {
  return runs.reduce((total, run) => total + run.runes, 0);
}

function sliceRunes(text: string, start: number, count: number): string {
  return text.slice(codeUnitIndexAtRune(text, start), codeUnitIndexAtRune(text, start + count));
}

function codeUnitIndexAtRune(text: string, offset: number): number {
  let index = 0;
  let seen = 0;
  while (index < text.length && seen < offset) {
    const point = text.codePointAt(index);
    index += point !== undefined && point > 0xffff ? 2 : 1;
    seen++;
  }
  if (seen !== offset) {
    throw new CRDTRuntimeError("invalid_rich_text_projection");
  }
  return index;
}

function previousCodePointIndex(text: string, end: number): number {
  if (end <= 0) {
    return -1;
  }
  const last = text.charCodeAt(end - 1);
  return end > 1 && last >= 0xdc00 && last <= 0xdfff && text.charCodeAt(end - 2) >= 0xd800 && text.charCodeAt(end - 2) <= 0xdbff ? end - 2 : end - 1;
}

function runeLength(value: string): number {
  let count = 0;
  for (let index = 0; index < value.length; count++) {
    const point = value.codePointAt(index);
    index += point !== undefined && point > 0xffff ? 2 : 1;
  }
  return count;
}

function utf8ByteLength(value: string): number {
  let bytes = 0;
  for (let index = 0; index < value.length;) {
    const point = value.codePointAt(index);
    if (point === undefined) {
      break;
    }
    bytes += point <= 0x7f ? 1 : point <= 0x7ff ? 2 : point <= 0xffff ? 3 : 4;
    index += point > 0xffff ? 2 : 1;
  }
  return bytes;
}

function isUnicodeScalarString(value: string): boolean {
  for (let index = 0; index < value.length; index++) {
    const codeUnit = value.charCodeAt(index);
    if (codeUnit >= 0xd800 && codeUnit <= 0xdbff) {
      const next = value.charCodeAt(index + 1);
      if (next < 0xdc00 || next > 0xdfff) {
        return false;
      }
      index++;
    } else if (codeUnit >= 0xdc00 && codeUnit <= 0xdfff) {
      return false;
    }
  }
  return true;
}

function objectField(value: unknown, key: string): unknown {
  return isPlainRecord(value) ? value[key] : undefined;
}

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    return false;
  }
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function hasOnlyKeys(value: Record<string, unknown>, allowed: readonly string[]): boolean {
  const keys = Object.keys(value);
  return keys.every((key) => allowed.includes(key));
}

function isColor(value: unknown): value is string {
  return typeof value === "string" && COLORS.has(value);
}

function isAlignment(value: unknown): value is string {
  return typeof value === "string" && ALIGNMENTS.has(value);
}

function isLanguage(value: unknown): value is string {
  return typeof value === "string" && /^[a-z0-9][a-z0-9+._-]{0,31}$/i.test(value);
}

function stringOrDash(value: unknown): string {
  return typeof value === "string" ? value : "-";
}

function numberOrDash(value: unknown): string {
  return typeof value === "number" ? String(value) : "-";
}

function booleanOrDash(value: unknown): string {
  return typeof value === "boolean" ? (value ? "1" : "0") : "-";
}

function positiveOrZero(value: string | undefined): number | undefined {
  if (value === undefined || !/^(0|[1-9][0-9]*)$/.test(value)) {
    return undefined;
  }
  const number = Number(value);
  return Number.isSafeInteger(number) ? number : undefined;
}

function markerBoolean(value: string): boolean | undefined {
  return value === "0" ? false : value === "1" ? true : undefined;
}

function defaultTextProps(): Readonly<Record<string, unknown>> {
  return { backgroundColor: "default", textColor: "default", textAlignment: "left" };
}
