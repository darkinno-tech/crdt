/**
 * Native Yjs text and presence bindings.
 *
 * This module deliberately operates on Yjs documents, updates, relative
 * positions, and y-protocols awareness directly. It is not an RGA-to-Yjs
 * conversion layer. Use it with a y-websocket-compatible provider (including
 * extensions.YJSHandler) or with the explicit update/awareness callbacks
 * below, but never mix those transport ownership models for one Y.Doc.
 */

import * as decoding from "lib0/decoding";
import * as encoding from "lib0/encoding";
import * as Y from "yjs";
import {
  applyAwarenessUpdate,
  Awareness,
  encodeAwarenessUpdate,
} from "y-protocols/awareness.js";

/** The Yjs binary-update encoding pinned for one room. */
export type YjsUpdateFormat = "v1" | "v2";

export type YjsBindingErrorCode =
  | "document_mismatch"
  | "document_closed"
  | "editor_update_failed"
  | "invalid_relative_position"
  | "invalid_options"
  | "invalid_selection"
  | "invalid_update"
  | "local_awareness_failed"
  | "local_update_failed"
  | "observer_failed"
  | "resource_limit"
  | "sync_mismatch"
  | "unsupported_text";

/** A stable error code for callers that need to close or recover a binding. */
export class YjsBindingError extends Error {
  constructor(readonly code: YjsBindingErrorCode) {
    super(code);
    this.name = "YjsBindingError";
  }
}

/** One UTF-16 editor replacement. Y.Text and CodeMirror both use UTF-16 indices. */
export interface YjsTextChange {
  readonly from: number;
  readonly to: number;
  readonly insert: string;
}

/** A live selection represented by Yjs relative positions, not ephemeral offsets. */
export interface YjsTextSelection {
  readonly anchor: number;
  readonly head: number;
}

/** A bounded binary Yjs relative position for this binding's exact Y.Text. */
export interface YjsTextRelativePosition {
  readonly version: 1;
  readonly encoded: string;
}

/** Yjs distinguishes the character before (-1) or at/after (0) an index. */
export type YjsRelativePositionAssociation = -1 | 0;

/** One peer cursor resolved against the current local Y.Text projection. */
export interface YjsRemoteCursor {
  readonly clientID: number;
  readonly selection: YjsTextSelection;
}

/** Optional local-history settings for one plain-text binding. */
export interface YjsTextUndoManagerOptions {
  /** Milliseconds in which adjacent local replacements are coalesced. */
  readonly captureTimeout?: number;
  /**
   * Caps local undo and redo stack items. When a new local replacement would
   * exceed this cap, the binding safely drops its existing local history
   * before recording that replacement.
   */
  readonly maxStackItems?: number;
}

/** A safe, minimal projection of one synchronous Yjs observeDeep event. */
export interface YjsDeepChange {
  /** Immutable path from the observed root to the changed shared type. */
  readonly path: readonly (string | number)[];
  /** The changed live Yjs type; read it synchronously rather than retaining raw Y.Event data. */
  readonly target: Y.AbstractType<unknown>;
}

/** Resource limits for an observeDeep callback owned by this library. */
export interface YjsDeepObserverOptions {
  /** Caps the number of changed descendants delivered in one transaction. */
  readonly maxEventsPerTransaction: number;
  /** Caps copied path segments for each changed descendant. */
  readonly maxPathDepth: number;
  /** Receives the bounded path/target projection synchronously after a transaction. */
  readonly onChanges: (changes: readonly YjsDeepChange[]) => void;
  /** Reports a boundedness or callback failure after the Yjs transaction commits. */
  readonly onError?: (error: YjsBindingError) => void;
}

/** Envelope limit for one unwrapped y-protocols sync submessage. */
export interface YjsSyncProtocolOptions {
  /** Caps the complete y-protocols sync submessage before decoder allocation. */
  readonly maxMessageBytes: number;
}

export interface YjsTextBindingOptions {
  /** Pins V1 or V2; one document room must never accept both encodings. */
  readonly updateFormat?: YjsUpdateFormat;
  /** Rejects one inbound update before handing it to the Yjs decoder. */
  readonly maxUpdateBytes: number;
  /** Rejects one inbound y-protocols awareness message before decoding it. */
  readonly maxAwarenessBytes: number;
  /** Caps local text growth before a local transaction mutates the Y.Doc. */
  readonly maxTextUTF16: number;
  /** Caps each encoded relative position retained in an awareness state. */
  readonly maxCursorBytes: number;
  /** Optional field name for this binding's JSON awareness cursor payload. */
  readonly cursorField?: string;
  /**
   * Receives local Yjs document updates when this binding owns transport.
   * It must synchronously make an application-owned retry record when durable
   * delivery is required. A thrown error latches this local-update path.
   */
  readonly onLocalUpdate?: (update: Uint8Array) => void;
  /**
   * Receives local y-protocols awareness updates when this binding owns
   * transport. A thrown error latches this local-awareness path.
   */
  readonly onLocalAwarenessUpdate?: (update: Uint8Array) => void;
  /** Receives incremental remote or external Y.Text changes. */
  readonly onTextChanges?: (changes: readonly YjsTextChange[]) => void;
  /** Reports a boundedness, callback, or unsupported-text failure without exposing document bytes. */
  readonly onError?: (error: YjsBindingError) => void;
}

interface EncodedYjsCursor {
  readonly version: 1;
  readonly anchor: string;
  readonly head: string;
}

interface YjsDeltaOperation {
  readonly insert?: unknown | undefined;
  readonly delete?: number | undefined;
  readonly retain?: number | undefined;
  readonly attributes?: Readonly<Record<string, unknown>> | undefined;
}

const defaultCursorField = "crdt.yjs.cursor.v1";
const defaultUndoStackItems = 256;
type YjsLocalUpdateFailureCode = Extract<YjsBindingErrorCode, "local_update_failed" | "resource_limit">;
type YjsLocalAwarenessFailureCode = Extract<YjsBindingErrorCode, "local_awareness_failed" | "resource_limit">;

/**
 * Binds one already-integrated Y.Text to incremental editor changes and the
 * exact Yjs update/awareness APIs. It intentionally accepts only unformatted
 * string Y.Text content. Rich-text schemas need a schema-aware Yjs binding;
 * this class stops projecting instead of flattening formats or embeds.
 */
export class YjsTextBinding {
  readonly updateFormat: YjsUpdateFormat;
  readonly cursorField: string;
  readonly #remoteUpdateOrigin = Object.freeze({ source: "crdt-yjs-remote-update" });
  readonly #remoteAwarenessOrigin = Object.freeze({ source: "crdt-yjs-remote-awareness" });
  readonly #localTextOrigin = Object.freeze({ source: "crdt-yjs-editor" });
  readonly #observeText: (event: Y.YTextEvent) => void;
  readonly #observeUpdate: (update: Uint8Array, origin: unknown) => void;
  readonly #observeAwareness: (_change: unknown, origin: unknown) => void;
  #closed = false;
  #projectingText = true;
  #textObserved = false;
  #localUpdateFailure: YjsLocalUpdateFailureCode | undefined;
  #localAwarenessFailure: YjsLocalAwarenessFailureCode | undefined;
  readonly #undoManagers = new Set<YjsTextUndoManager>();

  constructor(
    readonly document: Y.Doc,
    readonly text: Y.Text,
    private readonly options: YjsTextBindingOptions,
    readonly awareness?: Awareness,
  ) {
    if (text.doc !== document) {
      throw new YjsBindingError("document_mismatch");
    }
    validateOptions(options);
    this.updateFormat = options.updateFormat ?? "v1";
    this.cursorField = options.cursorField ?? defaultCursorField;
    if (!validCursorField(this.cursorField)) {
      throw new YjsBindingError("invalid_options");
    }
    if (text.length > options.maxTextUTF16 || !isPlainYText(text)) {
      throw new YjsBindingError(text.length > options.maxTextUTF16 ? "resource_limit" : "unsupported_text");
    }

    this.#observeText = (event) => this.#handleTextEvent(event);
    this.#observeUpdate = (update, origin) => this.#handleDocumentUpdate(update, origin);
    this.#observeAwareness = (_change, origin) => this.#handleAwarenessUpdate(origin);
    text.observe(this.#observeText);
    this.#textObserved = true;
    if (this.updateFormat === "v1") {
      document.on("update", this.#observeUpdate);
    } else {
      document.on("updateV2", this.#observeUpdate);
    }
    awareness?.on("update", this.#observeAwareness);
  }

  /** Applies one transport-authenticated Yjs update and projects only its changed ranges. */
  applyRemoteUpdate(update: Uint8Array): void {
    this.#assertOpen();
    assertBoundedBytes(update, this.options.maxUpdateBytes);
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

  /** Returns the state vector used by a y-protocols sync Step 1. */
  encodeStateVector(): Uint8Array {
    this.#assertOpen();
    const stateVector = Y.encodeStateVector(this.document);
    assertBoundedBytes(stateVector, this.options.maxUpdateBytes);
    return stateVector;
  }

  /** Encodes the V1/V2 state difference for a peer's state vector. */
  encodeStateAsUpdate(remoteStateVector?: Uint8Array): Uint8Array {
    this.#assertOpen();
    if (remoteStateVector !== undefined) {
      assertBoundedBytes(remoteStateVector, this.options.maxUpdateBytes);
    }
    let update: Uint8Array;
    try {
      update = this.updateFormat === "v1"
        ? Y.encodeStateAsUpdate(this.document, remoteStateVector)
        : Y.encodeStateAsUpdateV2(this.document, remoteStateVector);
    } catch {
      throw new YjsBindingError("invalid_update");
    }
    assertBoundedBytes(update, this.options.maxUpdateBytes);
    return update;
  }

  /** The pre-decode bound shared by update and state-vector messages. */
  get maxUpdateBytes(): number {
    return this.options.maxUpdateBytes;
  }

  /** Creates a stable, bounded Yjs position tied to this exact plain Y.Text. */
  createRelativePosition(
    index: number,
    association: YjsRelativePositionAssociation = 0,
  ): YjsTextRelativePosition {
    this.#assertTextProjection();
    if (!validOffset(index, this.text.length) || !validRelativePositionAssociation(association)) {
      throw new YjsBindingError("invalid_selection");
    }
    return {
      version: 1,
      encoded: encodeRelativePosition(
        Y.createRelativePositionFromTypeIndex(this.text, index, association),
        this.options.maxCursorBytes,
      ),
    };
  }

  /** Resolves a position only when it still names this binding's current Y.Text. */
  resolveRelativePosition(position: YjsTextRelativePosition): number {
    this.#assertTextProjection();
    if (!isTextRelativePosition(position)) {
      throw new YjsBindingError("invalid_relative_position");
    }
    try {
      const absolute = Y.createAbsolutePositionFromRelativePosition(
        Y.decodeRelativePosition(fromBase64(position.encoded, this.options.maxCursorBytes)),
        this.document,
      );
      if (absolute === null || absolute.type !== this.text || !validOffset(absolute.index, this.text.length)) {
        throw new YjsBindingError("invalid_relative_position");
      }
      return absolute.index;
    } catch (error) {
      if (error instanceof YjsBindingError) {
        throw error;
      }
      throw new YjsBindingError("invalid_relative_position");
    }
  }

  /** Creates local-only undo/redo history for replacements made through this binding. */
  createUndoManager(options: YjsTextUndoManagerOptions = {}): YjsTextUndoManager {
    this.#assertTextProjection();
    validateUndoOptions(options);
    const manager = new YjsTextUndoManager(
      this.text,
      this.#localTextOrigin,
      options,
      () => this.#assertTextProjection(),
      () => this.#assertLocalUpdatePath(),
      (released) => this.#undoManagers.delete(released),
    );
    this.#undoManagers.add(manager);
    return manager;
  }

  /** Creates a bounded V1 y-protocols sync helper for a transport this binding owns. */
  createSyncProtocol(options: YjsSyncProtocolOptions): YjsSyncProtocol {
    this.#assertOpen();
    if (this.updateFormat !== "v1") {
      throw new YjsBindingError("sync_mismatch");
    }
    return new YjsSyncProtocol(this, options);
  }

  /** Applies one inbound y-protocols awareness message without treating client IDs as identities. */
  applyRemoteAwarenessUpdate(update: Uint8Array): void {
    this.#assertOpen();
    if (this.awareness === undefined) {
      throw new YjsBindingError("invalid_options");
    }
    assertBoundedBytes(update, this.options.maxAwarenessBytes);
    try {
      applyAwarenessUpdate(this.awareness, update, this.#remoteAwarenessOrigin);
    } catch {
      throw new YjsBindingError("invalid_update");
    }
  }

  /** Encodes selected awareness states for an explicit transport. */
  encodeAwarenessUpdate(clientIDs: readonly number[]): Uint8Array {
    this.#assertOpen();
    if (this.awareness === undefined || !clientIDs.every(validClientID)) {
      throw new YjsBindingError("invalid_options");
    }
    const update = encodeAwarenessUpdate(this.awareness, [...clientIDs]);
    assertBoundedBytes(update, this.options.maxAwarenessBytes);
    return update;
  }

  /** Stores the local cursor using encoded Yjs relative positions in awareness JSON. */
  setLocalCursor(selection: YjsTextSelection): void {
    this.#assertTextProjection();
    if (this.awareness === undefined || !validSelection(selection, this.text.length)) {
      throw new YjsBindingError("invalid_selection");
    }
    this.#assertLocalAwarenessPath();
    const cursor: EncodedYjsCursor = {
      version: 1,
      anchor: this.createRelativePosition(selection.anchor).encoded,
      head: this.createRelativePosition(selection.head).encoded,
    };
    this.awareness.setLocalStateField(this.cursorField, cursor);
    this.#assertLocalAwarenessPath();
  }

  /** Clears only this binding's cursor field; other host-owned awareness fields remain intact. */
  clearLocalCursor(): void {
    this.#assertOpen();
    if (this.awareness === undefined) {
      throw new YjsBindingError("invalid_options");
    }
    this.#assertLocalAwarenessPath();
    this.awareness.setLocalStateField(this.cursorField, undefined);
    this.#assertLocalAwarenessPath();
  }

  /** Resolves all valid peer cursors against this exact Y.Text, skipping malformed foreign state. */
  remoteCursors(includeLocal = false): readonly YjsRemoteCursor[] {
    this.#assertOpen();
    if (this.awareness === undefined) {
      return [];
    }
    const cursors: YjsRemoteCursor[] = [];
    for (const [clientID, state] of this.awareness.getStates()) {
      if ((!includeLocal && clientID === this.document.clientID) || !validClientID(clientID)) {
        continue;
      }
      const selection = decodeCursor(state?.[this.cursorField], this.document, this.text, this.options.maxCursorBytes);
      if (selection !== undefined) {
        cursors.push({ clientID, selection });
      }
    }
    return cursors.sort((left, right) => left.clientID - right.clientID);
  }

  /**
   * Applies a local editor replacement as one Yjs transaction. The caller must
   * already have made the same replacement in its editor, so this binding does
   * not echo the resulting Y.Text delta back to that editor.
   */
  applyLocalReplacement(change: YjsTextChange): void {
    this.#assertTextProjection();
    this.#assertLocalUpdatePath();
    if (!validTextChange(change, this.text.length)) {
      throw new YjsBindingError("invalid_selection");
    }
    const nextLength = this.text.length - (change.to - change.from) + change.insert.length;
    if (nextLength > this.options.maxTextUTF16) {
      throw new YjsBindingError("resource_limit");
    }
    for (const manager of this.#undoManagers) {
      manager.prepareForLocalReplacement();
    }
    this.document.transact(() => {
      if (change.to > change.from) {
        this.text.delete(change.from, change.to - change.from);
      }
      if (change.insert.length > 0) {
        this.text.insert(change.from, change.insert);
      }
    }, this.#localTextOrigin);
    // Yjs emits document updates after committing this transaction. If the
    // caller-owned manual outbox rejects that update synchronously, surface a
    // stable error rather than the callback's arbitrary exception. The caller
    // must recover or resync because this transaction cannot be rolled back.
    this.#assertLocalUpdatePath();
  }

  /** Stops listeners without destroying caller-owned Y.Doc, Y.Text, or Awareness instances. */
  destroy(): boolean {
    if (this.#closed) {
      return false;
    }
    this.#closed = true;
    for (const manager of [...this.#undoManagers]) {
      manager.destroy();
    }
    if (this.#textObserved) {
      this.text.unobserve(this.#observeText);
      this.#textObserved = false;
    }
    if (this.updateFormat === "v1") {
      this.document.off("update", this.#observeUpdate);
    } else {
      this.document.off("updateV2", this.#observeUpdate);
    }
    this.awareness?.off("update", this.#observeAwareness);
    return true;
  }

  #handleTextEvent(event: Y.YTextEvent): void {
    if (this.#closed || !this.#projectingText) {
      return;
    }
    const changes = changesFromYjsDelta(event.delta);
    if (changes === undefined || this.text.length > this.options.maxTextUTF16 || !isPlainYjsDelta(event.delta)) {
      this.#stopTextProjection(this.text.length > this.options.maxTextUTF16 ? "resource_limit" : "unsupported_text");
      return;
    }
    if (event.transaction.origin !== this.#localTextOrigin && changes.length > 0) {
      try {
        this.options.onTextChanges?.(changes);
      } catch {
        // The Yjs transaction is already committed. Stop this view binding so
        // it cannot claim a projection it failed to apply; the caller still
        // owns the Y.Doc and may attach a recovery-capable editor surface.
        this.#stopTextProjection("editor_update_failed");
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
    if (update.byteLength > this.options.maxUpdateBytes) {
      this.#failLocalUpdate("resource_limit");
      return;
    }
    try {
      this.options.onLocalUpdate(update.slice());
    } catch {
      this.#failLocalUpdate("local_update_failed");
    }
  }

  #handleAwarenessUpdate(origin: unknown): void {
    if (this.#closed || this.awareness === undefined || origin === this.#remoteAwarenessOrigin) {
      return;
    }
    // Every local awareness update, including a heartbeat, is transportable.
    // Remote updates use a distinct origin and never echo into a manual outbox.
    if (this.#localAwarenessFailure !== undefined || this.options.onLocalAwarenessUpdate === undefined) {
      return;
    }
    try {
      const update = encodeAwarenessUpdate(this.awareness, [this.awareness.clientID]);
      if (update.byteLength > this.options.maxAwarenessBytes) {
        this.#failLocalAwareness("resource_limit");
        return;
      }
      this.options.onLocalAwarenessUpdate(update.slice());
    } catch {
      this.#failLocalAwareness("local_awareness_failed");
    }
  }

  #assertOpen(): void {
    if (this.#closed) {
      throw new YjsBindingError("document_closed");
    }
  }

  #assertTextProjection(): void {
    this.#assertOpen();
    if (!this.#projectingText) {
      throw new YjsBindingError("unsupported_text");
    }
  }

  #assertLocalUpdatePath(): void {
    if (this.#localUpdateFailure !== undefined) {
      throw new YjsBindingError(this.#localUpdateFailure);
    }
  }

  #assertLocalAwarenessPath(): void {
    if (this.#localAwarenessFailure !== undefined) {
      throw new YjsBindingError(this.#localAwarenessFailure);
    }
  }

  #failLocalUpdate(code: YjsLocalUpdateFailureCode): void {
    if (this.#localUpdateFailure !== undefined) {
      return;
    }
    this.#localUpdateFailure = code;
    this.#report(code);
  }

  #failLocalAwareness(code: YjsLocalAwarenessFailureCode): void {
    if (this.#localAwarenessFailure !== undefined) {
      return;
    }
    this.#localAwarenessFailure = code;
    this.#report(code);
  }

  #stopTextProjection(code: Extract<YjsBindingErrorCode, "editor_update_failed" | "resource_limit" | "unsupported_text">): void {
    this.#projectingText = false;
    if (this.#textObserved) {
      this.text.unobserve(this.#observeText);
      this.#textObserved = false;
    }
    this.#report(code);
  }

  #report(code: YjsBindingErrorCode): void {
    try {
      this.options.onError?.(new YjsBindingError(code));
    } catch {
      // Error reporting must not re-enter a synchronous Yjs observer loop.
    }
  }
}

/**
 * Selective undo/redo over only replacements emitted by one YjsTextBinding.
 * Undo and redo are new Yjs transactions: callers must send the resulting
 * local update through their authenticated transport like any other edit.
 */
export class YjsTextUndoManager {
  readonly #manager: Y.UndoManager;
  readonly maxStackItems: number;
  #closed = false;

  constructor(
    text: Y.Text,
    localOrigin: unknown,
    options: YjsTextUndoManagerOptions,
    private readonly assertActive: () => void,
    private readonly assertLocalUpdatePath: () => void,
    private readonly release: (manager: YjsTextUndoManager) => void,
  ) {
    this.maxStackItems = options.maxStackItems ?? defaultUndoStackItems;
    const managerOptions = {
      trackedOrigins: new Set<unknown>([localOrigin]),
      ...(options.captureTimeout === undefined ? {} : { captureTimeout: options.captureTimeout }),
    };
    this.#manager = new Y.UndoManager(text, managerOptions);
  }

  canUndo(): boolean {
    this.#assertActive();
    return this.#manager.canUndo();
  }

  canRedo(): boolean {
    this.#assertActive();
    return this.#manager.canRedo();
  }

  /** Applies a compensating Yjs transaction for the latest captured local edit. */
  undo(): boolean {
    this.#assertActive();
    this.assertLocalUpdatePath();
    const changed = this.#manager.undo() !== null;
    this.assertLocalUpdatePath();
    return changed;
  }

  /** Reapplies the latest locally undone edit as a new Yjs transaction. */
  redo(): boolean {
    this.#assertActive();
    this.assertLocalUpdatePath();
    const changed = this.#manager.redo() !== null;
    this.assertLocalUpdatePath();
    return changed;
  }

  /** Starts a new capture group before the next local editor replacement. */
  stopCapturing(): void {
    this.#assertActive();
    this.#manager.stopCapturing();
  }

  /** Drops retained local history without modifying the shared Y.Text. */
  clear(): void {
    this.#assertActive();
    this.#manager.clear();
  }

  /**
   * Prepares bounded history for a binding-owned local replacement. This is
   * intentionally all-or-nothing: Yjs only exposes safe release of complete
   * undo/redo stacks, so retaining an arbitrary suffix could keep deleted
   * structs alive or corrupt the stack's GC bookkeeping.
   */
  prepareForLocalReplacement(): void {
    this.#assertActive();
    if (this.#manager.undoStack.length >= this.maxStackItems) {
      this.#manager.clear();
    }
  }

  /** Releases Yjs listeners; it never destroys the caller-owned Y.Doc. */
  destroy(): boolean {
    if (this.#closed) {
      return false;
    }
    this.#closed = true;
    this.#manager.destroy();
    this.release(this);
    return true;
  }

  #assertActive(): void {
    if (this.#closed) {
      throw new YjsBindingError("document_closed");
    }
    this.assertActive();
  }
}

/**
 * Bounded wrapper for Yjs observeDeep. It deliberately copies only paths and
 * live targets, not lazy Y.Event internals or arbitrary user values.
 */
export class YjsDeepObserver {
  readonly #observer: (events: Y.YEvent<any>[]) => void;
  #closed = false;

  constructor(
    private readonly root: Y.AbstractType<unknown>,
    private readonly options: YjsDeepObserverOptions,
  ) {
    validateDeepObserverOptions(options);
    this.#observer = (events) => this.#handle(events);
    root.observeDeep(this.#observer);
  }

  destroy(): boolean {
    if (this.#closed) {
      return false;
    }
    this.#closed = true;
    this.root.unobserveDeep(this.#observer);
    return true;
  }

  #handle(events: readonly Y.YEvent<any>[]): void {
    if (this.#closed) {
      return;
    }
    if (events.length > this.options.maxEventsPerTransaction) {
      this.#fail("resource_limit");
      return;
    }
    const changes: YjsDeepChange[] = [];
    for (const event of events) {
      const path = event.path;
      if (path.length > this.options.maxPathDepth) {
        this.#fail("resource_limit");
        return;
      }
      changes.push({
        path: Object.freeze([...path]),
        target: event.target as Y.AbstractType<unknown>,
      });
    }
    try {
      this.options.onChanges(Object.freeze(changes));
    } catch {
      // Yjs already committed. Unsubscribe so the host cannot keep claiming a
      // view is current after its own deep-observation callback failed.
      this.#fail("observer_failed");
    }
  }

  #fail(code: Extract<YjsBindingErrorCode, "observer_failed" | "resource_limit">): void {
    if (this.#closed) {
      return;
    }
    this.#closed = true;
    this.root.unobserveDeep(this.#observer);
    try {
      this.options.onError?.(new YjsBindingError(code));
    } catch {
      // Error reporting must not re-enter the synchronous Yjs observer loop.
    }
  }
}

/** Starts a bounded synchronous deep observer rooted at one Yjs shared type. */
export function observeYjsDeep(
  root: Y.AbstractType<unknown>,
  options: YjsDeepObserverOptions,
): YjsDeepObserver {
  return new YjsDeepObserver(root, options);
}

const yjsSyncStep1 = 0;
const yjsSyncStep2 = 1;
const yjsSyncUpdate = 2;

/**
 * A V1 y-protocols sync submessage helper for a caller-owned authenticated
 * transport. It deliberately does not add y-websocket's outer message type,
 * a room name, authentication, or receipt semantics.
 */
export class YjsSyncProtocol {
  constructor(
    private readonly binding: YjsTextBinding,
    private readonly options: YjsSyncProtocolOptions,
  ) {
    validateSyncProtocolOptions(options);
  }

  /** Encodes standard y-protocols SyncStep1 from this binding's state vector. */
  encodeSyncStep1(): Uint8Array {
    return encodeYjsSyncMessage(yjsSyncStep1, this.binding.encodeStateVector(), this.options.maxMessageBytes);
  }

  /**
   * Reads exactly one unwrapped y-protocols V1 sync submessage. A SyncStep1
   * returns SyncStep2; SyncStep2 and update messages return undefined.
   */
  receive(message: Uint8Array): Uint8Array | undefined {
    this.binding.encodeStateVector(); // validates that the owning binding remains open
    assertBoundedBytes(message, this.options.maxMessageBytes);
    try {
      const decoder = decoding.createDecoder(message);
      const type = decoding.readVarUint(decoder);
      if (type !== yjsSyncStep1 && type !== yjsSyncStep2 && type !== yjsSyncUpdate) {
        throw new YjsBindingError("invalid_update");
      }
      const payload = decoding.readVarUint8Array(decoder);
      if (decoding.hasContent(decoder)) {
        throw new YjsBindingError("invalid_update");
      }
      assertBoundedBytes(payload, this.binding.maxUpdateBytes);
      if (type === yjsSyncStep1) {
        return encodeYjsSyncMessage(
          yjsSyncStep2,
          this.binding.encodeStateAsUpdate(payload),
          this.options.maxMessageBytes,
        );
      }
      this.binding.applyRemoteUpdate(payload);
      return undefined;
    } catch (error) {
      if (error instanceof YjsBindingError) {
        throw error;
      }
      throw new YjsBindingError("invalid_update");
    }
  }
}

/** The CodeMirror 6 surface used by the incremental native Yjs text binding. */
export interface YjsCodeMirrorTextPort {
  readonly state: {
    readonly doc: {
      readonly length: number;
      toString(): string;
    };
  };
  dispatch(spec: {
    readonly changes?: YjsTextChange | readonly YjsTextChange[];
  }): void;
}

/** The small CodeMirror update shape required to route local user transactions. */
export interface YjsCodeMirrorViewUpdate {
  readonly docChanged: boolean;
  readonly changes?: {
    iterChanges(listener: (
      fromA: number,
      toA: number,
      fromB: number,
      toB: number,
      inserted: { toString(): string },
    ) => void): void;
  };
}

export type BindYjsCodeMirrorOptions = Omit<YjsTextBindingOptions, "onTextChanges">;

/**
 * Connects one CodeMirror 6 plain-text document to a native Y.Text. Remote
 * Yjs transactions dispatch only their changed ranges, so a one-character
 * remote update never becomes a full-document editor replacement.
 */
export class YjsCodeMirrorBinding {
  readonly #binding: YjsTextBinding;
  #writing = false;

  constructor(
    document: Y.Doc,
    text: Y.Text,
    private readonly view: YjsCodeMirrorTextPort,
    options: BindYjsCodeMirrorOptions,
    awareness?: Awareness,
  ) {
    this.#binding = new YjsTextBinding(document, text, {
      ...options,
      onTextChanges: (changes) => this.#applyRemoteChanges(changes),
    }, awareness);
    const current = view.state.doc.toString();
    if (current !== text.toString()) {
      this.#writeFullInitialProjection(text.toString());
    }
  }

  get text(): Y.Text {
    return this.#binding.text;
  }

  get document(): Y.Doc {
    return this.#binding.document;
  }

  get awareness(): Awareness | undefined {
    return this.#binding.awareness;
  }

  applyRemoteUpdate(update: Uint8Array): void {
    this.#binding.applyRemoteUpdate(update);
  }

  applyRemoteAwarenessUpdate(update: Uint8Array): void {
    this.#binding.applyRemoteAwarenessUpdate(update);
  }

  encodeStateVector(): Uint8Array {
    return this.#binding.encodeStateVector();
  }

  encodeStateAsUpdate(remoteStateVector?: Uint8Array): Uint8Array {
    return this.#binding.encodeStateAsUpdate(remoteStateVector);
  }

  createRelativePosition(
    index: number,
    association: YjsRelativePositionAssociation = 0,
  ): YjsTextRelativePosition {
    return this.#binding.createRelativePosition(index, association);
  }

  resolveRelativePosition(position: YjsTextRelativePosition): number {
    return this.#binding.resolveRelativePosition(position);
  }

  createUndoManager(options: YjsTextUndoManagerOptions = {}): YjsTextUndoManager {
    return this.#binding.createUndoManager(options);
  }

  createSyncProtocol(options: YjsSyncProtocolOptions): YjsSyncProtocol {
    return this.#binding.createSyncProtocol(options);
  }

  encodeAwarenessUpdate(clientIDs: readonly number[]): Uint8Array {
    return this.#binding.encodeAwarenessUpdate(clientIDs);
  }

  setLocalCursor(selection: YjsTextSelection): void {
    this.#binding.setLocalCursor(selection);
  }

  clearLocalCursor(): void {
    this.#binding.clearLocalCursor();
  }

  remoteCursors(includeLocal = false): readonly YjsRemoteCursor[] {
    return this.#binding.remoteCursors(includeLocal);
  }

  /** Route every CodeMirror update listener event to this method. */
  applyViewUpdate(update: YjsCodeMirrorViewUpdate): void {
    if (this.#writing || !update.docChanged) {
      return;
    }
    const change = singleCodeMirrorChange(update, this.view.state.doc.length, this.text.length);
    if (change !== undefined) {
      this.#binding.applyLocalReplacement(change);
      return;
    }
    this.#replaceFromEditorFallback();
  }

  destroy(): boolean {
    return this.#binding.destroy();
  }

  #applyRemoteChanges(changes: readonly YjsTextChange[]): void {
    if (changes.length === 0) {
      return;
    }
    this.#writing = true;
    try {
      const first = changes[0];
      if (changes.length === 1 && first !== undefined) {
        this.view.dispatch({ changes: first });
      } else {
        this.view.dispatch({ changes });
      }
    } finally {
      this.#writing = false;
    }
  }

  #writeFullInitialProjection(value: string): void {
    this.#writing = true;
    try {
      this.view.dispatch({ changes: { from: 0, to: this.view.state.doc.length, insert: value } });
    } finally {
      this.#writing = false;
    }
  }

  #replaceFromEditorFallback(): void {
    const next = this.view.state.doc.toString();
    const previous = this.text.toString();
    if (next === previous) {
      return;
    }
    let prefix = 0;
    while (prefix < previous.length && prefix < next.length && previous.charCodeAt(prefix) === next.charCodeAt(prefix)) {
      prefix += 1;
    }
    let previousEnd = previous.length;
    let nextEnd = next.length;
    while (previousEnd > prefix && nextEnd > prefix && previous.charCodeAt(previousEnd - 1) === next.charCodeAt(nextEnd - 1)) {
      previousEnd -= 1;
      nextEnd -= 1;
    }
    this.#binding.applyLocalReplacement({ from: prefix, to: previousEnd, insert: next.slice(prefix, nextEnd) });
  }
}

export function bindYjsCodeMirrorPlainText(
  document: Y.Doc,
  text: Y.Text,
  view: YjsCodeMirrorTextPort,
  options: BindYjsCodeMirrorOptions,
  awareness?: Awareness,
): YjsCodeMirrorBinding {
  return new YjsCodeMirrorBinding(document, text, view, options, awareness);
}

function changesFromYjsDelta(delta: readonly YjsDeltaOperation[]): YjsTextChange[] | undefined {
  const changes: YjsTextChange[] = [];
  let offset = 0;
  let pending: { from: number; to: number; insert: string } | undefined;
  const flush = () => {
    if (pending !== undefined) {
      changes.push(pending);
      pending = undefined;
    }
  };
  for (const operation of delta) {
    if (!isPlainYjsOperation(operation)) {
      return undefined;
    }
    if (typeof operation.retain === "number") {
      flush();
      offset += operation.retain;
      continue;
    }
    if (typeof operation.delete === "number") {
      if (pending === undefined) {
        pending = { from: offset, to: offset, insert: "" };
      }
      pending.to += operation.delete;
      offset += operation.delete;
      continue;
    }
    if (typeof operation.insert === "string") {
      if (pending === undefined) {
        pending = { from: offset, to: offset, insert: "" };
      }
      pending.insert += operation.insert;
      continue;
    }
    return undefined;
  }
  flush();
  return changes;
}

function isPlainYText(text: Y.Text): boolean {
  return (text.toDelta() as readonly YjsDeltaOperation[]).every(isPlainYjsOperation);
}

function isPlainYjsDelta(delta: readonly YjsDeltaOperation[]): boolean {
  return delta.every(isPlainYjsOperation);
}

function isPlainYjsOperation(operation: YjsDeltaOperation): boolean {
  if (operation.attributes !== undefined && Object.keys(operation.attributes).length !== 0) {
    return false;
  }
  const actions = Number(typeof operation.retain === "number")
    + Number(typeof operation.delete === "number")
    + Number(typeof operation.insert === "string");
  return actions === 1
    && (typeof operation.retain !== "number" || Number.isSafeInteger(operation.retain) && operation.retain > 0)
    && (typeof operation.delete !== "number" || Number.isSafeInteger(operation.delete) && operation.delete > 0);
}

function singleCodeMirrorChange(
  update: YjsCodeMirrorViewUpdate,
  nextLength: number,
  previousLength: number,
): YjsTextChange | undefined {
  if (update.changes === undefined) {
    return undefined;
  }
  let result: YjsTextChange | undefined;
  let multiple = false;
  update.changes.iterChanges((fromA, toA, fromB, toB, inserted) => {
    if (result !== undefined || multiple || !validOffset(fromA, previousLength) || !validOffset(toA, previousLength) ||
      toA < fromA || !validOffset(fromB, nextLength) || !validOffset(toB, nextLength) || toB < fromB) {
      multiple = true;
      return;
    }
    const value = inserted.toString();
    if (value.length !== toB - fromB) {
      multiple = true;
      return;
    }
    result = { from: fromA, to: toA, insert: value };
  });
  if (multiple || result === undefined || nextLength !== previousLength - (result.to - result.from) + result.insert.length) {
    return undefined;
  }
  return result;
}

function encodeRelativePosition(position: Y.RelativePosition, maximumBytes: number): string {
  const encoded = Y.encodeRelativePosition(position);
  assertBoundedBytes(encoded, maximumBytes);
  return toBase64(encoded);
}

function decodeCursor(
  value: unknown,
  document: Y.Doc,
  text: Y.Text,
  maximumBytes: number,
): YjsTextSelection | undefined {
  if (!isEncodedCursor(value)) {
    return undefined;
  }
  try {
    const anchor = Y.createAbsolutePositionFromRelativePosition(Y.decodeRelativePosition(fromBase64(value.anchor, maximumBytes)), document);
    const head = Y.createAbsolutePositionFromRelativePosition(Y.decodeRelativePosition(fromBase64(value.head, maximumBytes)), document);
    if (anchor === null || head === null || anchor.type !== text || head.type !== text ||
      !validSelection({ anchor: anchor.index, head: head.index }, text.length)) {
      return undefined;
    }
    return { anchor: anchor.index, head: head.index };
  } catch {
    return undefined;
  }
}

function isEncodedCursor(value: unknown): value is EncodedYjsCursor {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    && (value as { version?: unknown }).version === 1
    && typeof (value as { anchor?: unknown }).anchor === "string"
    && typeof (value as { head?: unknown }).head === "string";
}

function isTextRelativePosition(value: unknown): value is YjsTextRelativePosition {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    && (value as { version?: unknown }).version === 1
    && typeof (value as { encoded?: unknown }).encoded === "string";
}

function toBase64(value: Uint8Array): string {
  let binary = "";
  for (let offset = 0; offset < value.length; offset += 8192) {
    binary += String.fromCharCode(...value.subarray(offset, Math.min(offset + 8192, value.length)));
  }
  return btoa(binary);
}

function fromBase64(value: string, maximumBytes: number): Uint8Array {
  if (value.length > base64Length(maximumBytes) || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value)) {
    throw new YjsBindingError("resource_limit");
  }
  const binary = atob(value);
  if (binary.length > maximumBytes) {
    throw new YjsBindingError("resource_limit");
  }
  const decoded = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) {
    decoded[index] = binary.charCodeAt(index);
  }
  return decoded;
}

function base64Length(bytes: number): number {
  return Math.ceil(bytes / 3) * 4;
}

function encodeYjsSyncMessage(type: number, payload: Uint8Array, maximumBytes: number): Uint8Array {
  const encoder = encoding.createEncoder();
  encoding.writeVarUint(encoder, type);
  encoding.writeVarUint8Array(encoder, payload);
  const message = encoding.toUint8Array(encoder);
  assertBoundedBytes(message, maximumBytes);
  return message;
}

function validateOptions(options: YjsTextBindingOptions): void {
  if (!validPositive(options.maxUpdateBytes) || !validPositive(options.maxAwarenessBytes) ||
    !validPositive(options.maxTextUTF16) || !validPositive(options.maxCursorBytes)) {
    throw new YjsBindingError("invalid_options");
  }
}

function validateUndoOptions(options: YjsTextUndoManagerOptions): void {
  if (options.captureTimeout !== undefined &&
    (!Number.isSafeInteger(options.captureTimeout) || options.captureTimeout < 0) ||
    options.maxStackItems !== undefined && !validPositive(options.maxStackItems)) {
    throw new YjsBindingError("invalid_options");
  }
}

function validateDeepObserverOptions(options: YjsDeepObserverOptions): void {
  if (!validPositive(options.maxEventsPerTransaction) || !validPositive(options.maxPathDepth) ||
    typeof options.onChanges !== "function") {
    throw new YjsBindingError("invalid_options");
  }
}

function validateSyncProtocolOptions(options: YjsSyncProtocolOptions): void {
  if (!validPositive(options.maxMessageBytes)) {
    throw new YjsBindingError("invalid_options");
  }
}

function validCursorField(value: string): boolean {
  return value.length > 0 && value.length <= 64 && /^[a-zA-Z0-9._-]+$/.test(value);
}

function assertBoundedBytes(value: Uint8Array, maximumBytes: number): void {
  if (value.byteLength > maximumBytes) {
    throw new YjsBindingError("resource_limit");
  }
}

function validSelection(value: YjsTextSelection, length: number): boolean {
  return validOffset(value.anchor, length) && validOffset(value.head, length);
}

function validTextChange(value: YjsTextChange, length: number): boolean {
  return validSelection({ anchor: value.from, head: value.to }, length) && value.to >= value.from && typeof value.insert === "string";
}

function validOffset(value: number, length: number): boolean {
  return Number.isSafeInteger(value) && value >= 0 && value <= length;
}

function validRelativePositionAssociation(value: number): value is YjsRelativePositionAssociation {
  return value === -1 || value === 0;
}

function validPositive(value: number): boolean {
  return Number.isSafeInteger(value) && value > 0;
}

function validClientID(value: number): boolean {
  return Number.isSafeInteger(value) && value >= 0;
}
