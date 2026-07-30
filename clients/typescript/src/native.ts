/**
 * A dependency-free, browser-oriented CRDT document with Map and Array shared
 * types. This is intentionally a separate `native-ts-v1` update contract: it
 * is not a Go CRDT frame and must not be sent to a Go frame decoder.
 *
 * Map values use LWW resolution. Array values use an RGA-style immutable node
 * graph: inserts name their left neighbour, deletes retain tombstones, and
 * concurrent siblings are ordered by their immutable IDs. Updates are
 * transport-agnostic, commutative, and idempotent once admitted by this
 * document's bounded validator.
 */

const TEXT_ENCODER = new TextEncoder();
const TEXT_DECODER = new TextDecoder("utf-8", { fatal: true });
const ROOT_PARENT = "root";

export const NATIVE_UPDATE_VERSION = 1;

/** JSON values are copied on entry and exit, so callers cannot mutate CRDT state by reference. */
export type NativeValue =
  | null
  | boolean
  | number
  | string
  | readonly NativeValue[]
  | { readonly [key: string]: NativeValue };

export interface NativeID {
  readonly actor: string;
  readonly counter: number;
}

export interface NativeArrayEntry {
  readonly id: NativeID;
  /** `null` is the synthetic beginning of the array. */
  readonly after: NativeID | null;
  readonly value: NativeValue;
}

export interface NativeMapSetOperation {
  readonly kind: "map-set";
  readonly target: string;
  readonly key: string;
  readonly id: NativeID;
  readonly value: NativeValue;
}

export interface NativeMapDeleteOperation {
  readonly kind: "map-delete";
  readonly target: string;
  readonly key: string;
  readonly id: NativeID;
}

export interface NativeArrayInsertOperation {
  readonly kind: "array-insert";
  readonly target: string;
  readonly entries: readonly NativeArrayEntry[];
}

export interface NativeArrayDeleteOperation {
  readonly kind: "array-delete";
  readonly target: string;
  readonly ids: readonly NativeID[];
}

export type NativeOperation =
  | NativeMapSetOperation
  | NativeMapDeleteOperation
  | NativeArrayInsertOperation
  | NativeArrayDeleteOperation;

/** A canonical, JSON-encoded update may be forwarded without interpreting its operations. */
export interface NativeUpdate {
  readonly version: typeof NATIVE_UPDATE_VERSION;
  /** Envelope provenance only. Authenticate transport peers separately. */
  readonly actor: string;
  readonly operations: readonly NativeOperation[];
}

export interface NativeDocumentLimits {
  readonly maxUpdateBytes: number;
  readonly maxOperationsPerUpdate: number;
  readonly maxReplicaIDBytes: number;
  readonly maxRootNameBytes: number;
  readonly maxRootTypes: number;
  readonly maxMapEntries: number;
  readonly maxMapKeyBytes: number;
  readonly maxArrayItems: number;
  readonly maxArrayTombstones: number;
  readonly maxPendingItems: number;
  readonly maxValueBytes: number;
  readonly maxValueDepth: number;
  readonly maxValueItems: number;
}

export type NativeDocumentOptions = Partial<NativeDocumentLimits>;

/** Conservative defaults for mobile web clients. Lower compatible deployment limits are allowed. */
export const DEFAULT_NATIVE_LIMITS: Readonly<NativeDocumentLimits> = Object.freeze({
  maxUpdateBytes: 1 << 20,
  maxOperationsPerUpdate: 10_000,
  maxReplicaIDBytes: 256,
  maxRootNameBytes: 256,
  maxRootTypes: 128,
  maxMapEntries: 10_000,
  maxMapKeyBytes: 1_024,
  maxArrayItems: 100_000,
  maxArrayTombstones: 100_000,
  maxPendingItems: 10_000,
  maxValueBytes: 64 << 10,
  maxValueDepth: 32,
  maxValueItems: 10_000,
});

export type NativeCRDTErrorCode =
  | "invalid_update"
  | "resource_limit"
  | "state_conflict"
  | "type_conflict"
  | "transaction_active"
  | "document_closed"
  | "incomplete_state";

export class NativeCRDTError extends Error {
  readonly code: NativeCRDTErrorCode;

  constructor(code: NativeCRDTErrorCode) {
    super(code);
    this.name = "NativeCRDTError";
    this.code = code;
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

export interface NativeUpdateEvent {
  readonly update: NativeUpdate;
  readonly origin: unknown;
  /** True only for an update created by this document's local mutation API. */
  readonly local: boolean;
}

export interface NativeTypeEvent extends NativeUpdateEvent {
  readonly target: NativeMap | NativeArray;
}

export type NativeUpdateListener = (event: NativeUpdateEvent) => void;
export type NativeTypeListener = (event: NativeTypeEvent) => void;

export interface NativeRoot {
  readonly name: string;
  readonly type: RootType;
}

/** Save root declarations, complete state updates, and `counter` in one atomic write. */
export interface NativeSnapshot {
  readonly roots: readonly NativeRoot[];
  readonly updates: readonly NativeUpdate[];
  readonly counter: number;
}

/**
 * The mutable portion of a persistence record. Browser stores can append
 * canonical updates without repeatedly encoding the complete document state.
 */
export interface NativePersistenceMetadata {
  readonly roots: readonly NativeRoot[];
  readonly counter: number;
}

interface MapEntry {
  readonly id: NativeID;
  readonly present: boolean;
  readonly value?: NativeValue;
}

interface ArrayNode extends NativeArrayEntry {}

type RootType = "map" | "array";
type Root = NativeMap | NativeArray;

interface PreparedUpdate {
  readonly update: NativeUpdate;
  readonly roots: ReadonlyMap<string, Root>;
  readonly newRoots: ReadonlyMap<string, Root>;
}

/**
 * A LWW map whose keys are strings and whose values are copied JSON values.
 * It is Y.Map-like, but intentionally does not claim Yjs API or wire compatibility.
 */
export class NativeMap<T extends NativeValue = NativeValue> {
  readonly #entries = new Map<string, MapEntry>();
  /** Current-tag ownership prevents one immutable ID from naming two keys. */
  readonly #tagOwners = new Map<string, string>();
  readonly #listeners = new Set<NativeTypeListener>();
  #visibleSize = 0;

  constructor(
    private readonly document: NativeDocument,
    readonly name: string,
  ) {}

  get size(): number {
    this.document._assertOpen();
    return this.#visibleSize;
  }

  has(key: string): boolean {
    this.document._assertOpen();
    this.document._assertMapKey(key);
    return this.#entries.get(key)?.present === true;
  }

  get(key: string): T | undefined {
    this.document._assertOpen();
    this.document._assertMapKey(key);
    const entry = this.#entries.get(key);
    return entry?.present === true ? (cloneValue(entry.value!) as T) : undefined;
  }

  set(key: string, value: T): this {
    this.document._assertOpen();
    this.document._assertMapKey(key);
    const copied = this.document._copyValue(value);
    const operation: NativeMapSetOperation = {
      kind: "map-set",
      target: this.name,
      key,
      id: this.document._nextID(),
      value: copied,
    };
    this.document._applyLocal(operation);
    return this;
  }

  /**
   * Emits a tombstone even when the key is currently absent, so a reordered
   * older set cannot resurrect it. The return value reports prior visibility.
   */
  delete(key: string): boolean {
    this.document._assertOpen();
    this.document._assertMapKey(key);
    const existed = this.#entries.get(key)?.present === true;
    const operation: NativeMapDeleteOperation = {
      kind: "map-delete",
      target: this.name,
      key,
      id: this.document._nextID(),
    };
    this.document._applyLocal(operation);
    return existed;
  }

  entries(): IterableIterator<[string, T]> {
    this.document._assertOpen();
    const values = [...this.#entries.entries()]
      .filter(([, entry]) => entry.present)
      .sort(([left], [right]) => compareText(left, right))
      .map(([key, entry]) => [key, cloneValue(entry.value!) as T] as [string, T]);
    return values[Symbol.iterator]();
  }

  toJSON(): Record<string, T> {
    this.document._assertOpen();
    const value: Record<string, T> = {};
    for (const [key, entry] of this.#entries) {
      if (entry.present) {
        defineOwnValue(value, key, cloneValue(entry.value!) as T);
      }
    }
    return value;
  }

  observe(listener: NativeTypeListener): () => void {
    this.document._assertOpen();
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  }

  /** @internal */
  _preflight(operations: readonly (NativeMapSetOperation | NativeMapDeleteOperation)[]): void {
    const newKeys = new Set<string>();
    const definitions = new Map<string, string>();
    for (const operation of operations) {
      const current = this.#entries.get(operation.key);
      if (current === undefined) {
        newKeys.add(operation.key);
      }
      const definitionKey = idKey(operation.id);
      const definition = canonicalJSON(operation);
      const previous = definitions.get(definitionKey);
      if (previous !== undefined && previous !== definition) {
        throw stateConflict();
      }
      definitions.set(definitionKey, definition);
      const owner = this.#tagOwners.get(definitionKey);
      if (owner !== undefined && owner !== operation.key) {
        throw stateConflict();
      }
      if (current !== undefined && equalID(current.id, operation.id) && !equalMapOperation(current, operation)) {
        throw stateConflict();
      }
    }
    if (this.#entries.size + newKeys.size > this.document.limits.maxMapEntries) {
      throw resourceLimit();
    }
  }

  /** @internal */
  _apply(operation: NativeMapSetOperation | NativeMapDeleteOperation): boolean {
    const current = this.#entries.get(operation.key);
    if (current !== undefined) {
      if (equalID(current.id, operation.id)) {
        return false;
      }
      if (compareID(operation.id, current.id) < 0) {
        return false;
      }
    }
    if (operation.kind === "map-set") {
      if (current !== undefined && this.#tagOwners.get(idKey(current.id)) === operation.key) {
        this.#tagOwners.delete(idKey(current.id));
      }
      this.#entries.set(operation.key, { id: copyID(operation.id), present: true, value: cloneValue(operation.value) });
      if (current?.present !== true) {
        this.#visibleSize += 1;
      }
    } else {
      if (current !== undefined && this.#tagOwners.get(idKey(current.id)) === operation.key) {
        this.#tagOwners.delete(idKey(current.id));
      }
      this.#entries.set(operation.key, { id: copyID(operation.id), present: false });
      if (current?.present === true) {
        this.#visibleSize -= 1;
      }
    }
    this.#tagOwners.set(idKey(operation.id), operation.key);
    return true;
  }

  /** @internal */
  _stateOperations(): NativeOperation[] {
    return [...this.#entries.entries()]
      .sort(([left], [right]) => compareText(left, right))
      .map(([key, entry]): NativeOperation =>
        entry.present
          ? { kind: "map-set", target: this.name, key, id: copyID(entry.id), value: cloneValue(entry.value!) }
          : { kind: "map-delete", target: this.name, key, id: copyID(entry.id) },
      );
  }

  /** @internal */
  _notify(event: NativeTypeEvent): void {
    for (const listener of [...this.#listeners]) {
      listener(event);
    }
  }
}

/**
 * An RGA-style sequence. It has no move operation: a move is delete plus
 * insert, preserving immutable IDs and deterministic convergence.
 */
export class NativeArray<T extends NativeValue = NativeValue> {
  readonly #nodes = new Map<string, ArrayNode>();
  readonly #pending = new Map<string, ArrayNode>();
  readonly #waitingByParent = new Map<string, ArrayNode[]>();
  readonly #deleted = new Set<string>();
  readonly #children = new Map<string, ArrayNode[]>();
  readonly #listeners = new Set<NativeTypeListener>();
  #childrenDirty = false;
  /** Nodes are immutable after admission; retain the private visible projection between writes. */
  #visibleCache: ArrayNode[] | undefined;

  constructor(
    private readonly document: NativeDocument,
    readonly name: string,
  ) {}

  get length(): number {
    return this.#visibleNodes().length;
  }

  get pendingCount(): number {
    this.document._assertOpen();
    return this.#pending.size;
  }

  get(index: number): T | undefined {
    this.document._assertOpen();
    const visible = this.#visibleNodes();
    assertArrayIndex(index, visible.length, false);
    const node = visible[index];
    return node === undefined ? undefined : (cloneValue(node.value) as T);
  }

  insert(index: number, values: readonly T[]): void {
    this.document._assertOpen();
    const visible = this.#visibleNodes();
    assertArrayIndex(index, visible.length, true);
    if (values.length === 0) {
      return;
    }
    if (this.#nodes.size + this.#pending.size + values.length > this.document.limits.maxArrayItems) {
      throw resourceLimit();
    }
    let after = index === 0 ? null : copyID(visible[index - 1]!.id);
    const entries: NativeArrayEntry[] = [];
    for (const value of values) {
      const entry: NativeArrayEntry = { id: this.document._nextID(), after, value: this.document._copyValue(value) };
      entries.push(entry);
      after = copyID(entry.id);
    }
    this.document._applyLocal({ kind: "array-insert", target: this.name, entries });
  }

  /**
   * Inserts preallocated immutable entries. It is reserved for document-owned
   * extensions that need one entry ID to also name a nested shared type.
   * Callers must obtain every ID from the same NativeDocument.
   *
   * @internal
   */
  _insertWithIDs(index: number, values: readonly { readonly id: NativeID; readonly value: NativeValue }[]): NativeArrayInsertOperation {
    this.document._assertOpen();
    const visible = this.#visibleNodes();
    assertArrayIndex(index, visible.length, true);
    if (values.length === 0 || values.length > this.document.limits.maxArrayItems) {
      throw invalidUpdate();
    }
    if (this.#nodes.size + this.#pending.size + values.length > this.document.limits.maxArrayItems) {
      throw resourceLimit();
    }
    let after = index === 0 ? null : copyID(visible[index - 1]!.id);
    const entries: NativeArrayEntry[] = [];
    const ids = new Set<string>();
    for (const value of values) {
      const id = this.document._copyID(value.id);
      if (ids.has(idKey(id))) {
        throw stateConflict();
      }
      ids.add(idKey(id));
      entries.push({ id, after, value: this.document._copyValue(value.value) });
      after = copyID(id);
    }
    return this.document._applyLocal({ kind: "array-insert", target: this.name, entries }) as NativeArrayInsertOperation;
  }

  push(values: readonly T[]): void {
    this.insert(this.length, values);
  }

  /** Tombstones a visible range. A delete is idempotent and may be delivered before its insert. */
  delete(index: number, count = 1): NativeArrayDeleteOperation {
    this.document._assertOpen();
    const visible = this.#visibleNodes();
    assertArrayIndex(index, visible.length, false);
    if (!Number.isSafeInteger(count) || count < 0 || index + count > visible.length) {
      throw invalidUpdate();
    }
    if (count === 0) {
      return { kind: "array-delete", target: this.name, ids: [] };
    }
    const ids = visible
      .slice(index, index + count)
      .map((node) => copyID(node.id));
    return this.document._applyLocal({ kind: "array-delete", target: this.name, ids }) as NativeArrayDeleteOperation;
  }

  toArray(): T[] {
    this.document._assertOpen();
    return this.#visibleNodes().map((node) => cloneValue(node.value) as T);
  }

  observe(listener: NativeTypeListener): () => void {
    this.document._assertOpen();
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  }

  /** @internal */
  _preflight(
    inserts: readonly NativeArrayInsertOperation[],
    deletes: readonly NativeArrayDeleteOperation[],
  ): void {
    const incoming = new Map<string, ArrayNode>();
    for (const operation of inserts) {
      for (const entry of operation.entries) {
        const key = idKey(entry.id);
        const current = this.#nodes.get(key) ?? this.#pending.get(key);
        if (current !== undefined) {
          if (!equalNode(current, entry)) {
            throw stateConflict();
          }
          continue;
        }
        if (incoming.has(key)) {
          throw stateConflict();
        }
        incoming.set(key, copyNode(entry));
      }
    }
    if (this.#nodes.size + this.#pending.size + incoming.size > this.document.limits.maxArrayItems) {
      throw resourceLimit();
    }

    assertAcyclic(incoming, this.#nodes, this.#pending);
    let unresolved = 0;
    const resolving = new Set<string>();
    const resolved = new Set<string>();
    for (const [key, entry] of incoming) {
      if (!isResolvable(entry, incoming, this.#nodes, resolving, resolved)) {
        unresolved += 1;
      }
    }
    // Conservative by design: an update that also resolves old pending nodes
    // may still be rejected at the boundary instead of briefly exceeding it.
    if (this.#pending.size + unresolved > this.document.limits.maxPendingItems) {
      throw resourceLimit();
    }

    const newTombstones = new Set<string>();
    for (const operation of deletes) {
      for (const id of operation.ids) {
        const key = idKey(id);
        if (!this.#deleted.has(key)) {
          newTombstones.add(key);
        }
      }
    }
    if (this.#deleted.size + newTombstones.size > this.document.limits.maxArrayTombstones) {
      throw resourceLimit();
    }
  }

  /** @internal */
  _apply(operation: NativeArrayInsertOperation | NativeArrayDeleteOperation): boolean {
    let changed = false;
    if (operation.kind === "array-insert") {
      for (const entry of operation.entries) {
        const key = idKey(entry.id);
        if (this.#nodes.has(key) || this.#pending.has(key)) {
          continue;
        }
        this.#receive(copyNode(entry));
        changed = true;
      }
      return changed;
    }
    for (const id of operation.ids) {
      const key = idKey(id);
      if (!this.#deleted.has(key)) {
        this.#deleted.add(key);
        changed = true;
      }
    }
    if (changed) {
      this.#visibleCache = undefined;
    }
    return changed;
  }

  /** @internal */
  _stateOperations(): NativeOperation[] {
    if (this.#pending.size !== 0) {
      throw incompleteState();
    }
    const operations: NativeOperation[] = [];
    const entries = topologicalNodes(this.#nodes);
    for (const chunk of chunkArrayEntries(entries, this.document.limits, this.document.replicaID, this.name)) {
      operations.push({ kind: "array-insert", target: this.name, entries: chunk });
    }
    const ids = [...this.#deleted]
      .map(parseIDKey)
      .sort(compareID);
    if (ids.length !== 0) {
      operations.push({ kind: "array-delete", target: this.name, ids });
    }
    return operations;
  }

  /** @internal */
  _notify(event: NativeTypeEvent): void {
    for (const listener of [...this.#listeners]) {
      listener(event);
    }
  }

  #receive(node: ArrayNode): void {
    const parentKey = node.after === null ? ROOT_PARENT : idKey(node.after);
    if (node.after !== null && !this.#nodes.has(parentKey)) {
      this.#pending.set(idKey(node.id), node);
      const waiting = this.#waitingByParent.get(parentKey) ?? [];
      waiting.push(node);
      this.#waitingByParent.set(parentKey, waiting);
      return;
    }
    this.#integrateAndResolve(node);
  }

  #integrateAndResolve(node: ArrayNode): void {
    this.#integrate(node);
    const waiting = this.#waitingByParent.get(idKey(node.id));
    if (waiting === undefined) {
      return;
    }
    this.#waitingByParent.delete(idKey(node.id));
    for (const child of waiting) {
      this.#pending.delete(idKey(child.id));
      this.#integrateAndResolve(child);
    }
  }

  #integrate(node: ArrayNode): void {
    const key = idKey(node.id);
    this.#nodes.set(key, node);
    const parentKey = node.after === null ? ROOT_PARENT : idKey(node.after);
    const children = this.#children.get(parentKey) ?? [];
    children.push(node);
    this.#children.set(parentKey, children);
    this.#childrenDirty = true;
    this.#visibleCache = undefined;
  }

  #visibleNodes(): ArrayNode[] {
    this.document._assertOpen();
    if (this.#visibleCache !== undefined) {
      return this.#visibleCache;
    }
    if (this.#childrenDirty) {
      for (const children of this.#children.values()) {
        children.sort((left, right) => compareID(right.id, left.id));
      }
      this.#childrenDirty = false;
    }
    const visible: ArrayNode[] = [];
    const stack: ArrayNode[] = [];
    pushChildren(stack, this.#children.get(ROOT_PARENT));
    while (stack.length !== 0) {
      const node = stack.pop()!;
      if (!this.#deleted.has(idKey(node.id))) {
        visible.push(node);
      }
      pushChildren(stack, this.#children.get(idKey(node.id)));
    }
    this.#visibleCache = visible;
    return visible;
  }
}

/**
 * A document owns named root types, a local actor counter, transaction
 * boundaries, and update listeners. It has no network, persistence, identity,
 * authorization, encryption, or replay policy; hosts own those boundaries.
 */
export class NativeDocument {
  readonly limits: Readonly<NativeDocumentLimits>;
  readonly #roots = new Map<string, Root>();
  readonly #updateListeners = new Set<NativeUpdateListener>();
  #counter = 0;
  #transactionDepth = 0;
  #transactionOrigin: unknown;
  #pendingOperations: NativeOperation[] = [];
  /** Canonical UTF-8 bytes of pending operation JSON, excluding commas. */
  #pendingOperationBytes = 0;
  #changedRoots = new Set<Root>();
  #closed = false;

  constructor(
    readonly replicaID: string,
    options: NativeDocumentOptions = {},
  ) {
    this.limits = resolveLimits(options);
    assertReplicaID(replicaID, this.limits);
  }

  getMap<T extends NativeValue = NativeValue>(name: string): NativeMap<T> {
    this._assertOpen();
    this._assertRootName(name);
    const existing = this.#roots.get(name);
    if (existing !== undefined) {
      if (!(existing instanceof NativeMap)) {
        throw typeConflict();
      }
      return existing as NativeMap<T>;
    }
    this.#assertRootCapacity();
    const map = new NativeMap<T>(this, name);
    this.#roots.set(name, map);
    return map;
  }

  getArray<T extends NativeValue = NativeValue>(name: string): NativeArray<T> {
    this._assertOpen();
    this._assertRootName(name);
    const existing = this.#roots.get(name);
    if (existing !== undefined) {
      if (!(existing instanceof NativeArray)) {
        throw typeConflict();
      }
      return existing as NativeArray<T>;
    }
    this.#assertRootCapacity();
    const array = new NativeArray<T>(this, name);
    this.#roots.set(name, array);
    return array;
  }

  /** Groups local mutations into one update and one observer turn. */
  transact<T>(callback: () => T, origin?: unknown): T {
    this._assertOpen();
    if (this.#transactionDepth === 0) {
      this.#transactionOrigin = origin;
    }
    this.#transactionDepth += 1;
    try {
      return callback();
    } finally {
      this.#transactionDepth -= 1;
      if (this.#transactionDepth === 0) {
        this.#flushLocal();
      }
    }
  }

  onUpdate(listener: NativeUpdateListener): () => void {
    this._assertOpen();
    this.#updateListeners.add(listener);
    return () => this.#updateListeners.delete(listener);
  }

  /** Validates fully before applying; malformed/conflicting updates leave state unchanged. */
  applyUpdate(update: NativeUpdate, origin?: unknown): boolean {
    this._assertOpen();
    if (this.#transactionDepth !== 0) {
      throw new NativeCRDTError("transaction_active");
    }
    const prepared = this.#prepare(update);
    const changed = this.#applyPrepared(prepared);
    if (changed) {
      this.#emit(prepared.update, origin, false);
    }
    return changed;
  }

  applyEncodedUpdate(encoded: Uint8Array, origin?: unknown): boolean {
    this._assertOpen();
    if (this.#transactionDepth !== 0) {
      throw new NativeCRDTError("transaction_active");
    }
    // The byte decoder already returned an owned, normalized update. Avoid a
    // second full value clone on the hot browser receive path.
    const prepared = this.#prepareNormalized(decodeNativeUpdate(encoded, this.limits));
    const changed = this.#applyPrepared(prepared);
    if (changed) {
      this.#emit(prepared.update, origin, false);
    }
    return changed;
  }

  /**
   * Returns bounded state updates. Pending arrays are deliberately not
   * serializable: receive their missing parents first, then snapshot.
   */
  encodeStateAsUpdates(): NativeUpdate[] {
    this._assertOpen();
    const operations = [...this.#roots.values()]
      .sort((left, right) => compareText(left.name, right.name))
      .flatMap((root) => root._stateOperations());
    return packOperations(operations, this.replicaID, this.limits);
  }

  snapshot(): NativeSnapshot {
    this._assertOpen();
    return { ...this.persistenceMetadata(), updates: this.encodeStateAsUpdates() };
  }

  /**
   * Returns copied root declarations and the local counter without encoding
   * complete state. Pair this metadata with an atomically appended canonical
   * update log; it is not a standalone recovery record.
   */
  persistenceMetadata(): NativePersistenceMetadata {
    this._assertOpen();
    const roots: NativeRoot[] = [...this.#roots.values()]
      .sort((left, right) => compareText(left.name, right.name))
      .map((root) => ({ name: root.name, type: root instanceof NativeMap ? "map" : "array" }));
    return { roots, counter: this.#counter };
  }

  static restore(replicaID: string, snapshot: NativeSnapshot, options: NativeDocumentOptions = {}): NativeDocument {
    const document = new NativeDocument(replicaID, options);
    if (
      !isRecord(snapshot) ||
      !Array.isArray(snapshot.roots) ||
      !Array.isArray(snapshot.updates) ||
      !isPositiveOrZeroSafeInteger(snapshot.counter)
    ) {
      throw invalidUpdate();
    }
    const rootNames = new Set<string>();
    for (const root of snapshot.roots) {
      const normalized = normalizeRoot(root, document.limits);
      if (rootNames.has(normalized.name)) {
        throw invalidUpdate();
      }
      rootNames.add(normalized.name);
      if (normalized.type === "map") {
        document.getMap(normalized.name);
      } else {
        document.getArray(normalized.name);
      }
    }
    for (const update of snapshot.updates) {
      document.applyUpdate(update, "restore");
    }
    document.#counter = Math.max(document.#counter, snapshot.counter);
    return document;
  }

  close(): boolean {
    if (this.#closed) {
      return false;
    }
    this.#closed = true;
    this.#roots.clear();
    this.#updateListeners.clear();
    this.#pendingOperations = [];
    this.#pendingOperationBytes = 0;
    this.#changedRoots.clear();
    return true;
  }

  /** @internal */
  _assertOpen(): void {
    if (this.#closed) {
      throw new NativeCRDTError("document_closed");
    }
  }

  /** @internal */
  _assertMapKey(key: string): void {
    assertBoundedText(key, this.limits.maxMapKeyBytes, false);
  }

  /** @internal */
  _copyValue(value: NativeValue): NativeValue {
    return copyAndValidateValue(value, this.limits);
  }

  /** @internal */
  _copyID(value: NativeID): NativeID {
    return normalizeID(value, this.limits);
  }

  /** @internal */
  _setCounterAtLeast(counter: number): void {
    if (!isPositiveOrZeroSafeInteger(counter)) {
      throw invalidUpdate();
    }
    this.#counter = Math.max(this.#counter, counter);
  }

  /** @internal */
  _nextID(): NativeID {
    if (this.#counter >= Number.MAX_SAFE_INTEGER) {
      throw resourceLimit();
    }
    this.#counter += 1;
    return { actor: this.replicaID, counter: this.#counter };
  }

  /** @internal */
  _applyLocal(operation: NativeOperation): NativeOperation {
    this._assertOpen();
    // Normalize and preflight one operation first. Re-encoding every prior
    // operation for each member of a large transaction is quadratic, while
    // the exact canonical envelope size is additive after normalization.
    const normalized = normalizeUpdate({
      version: NATIVE_UPDATE_VERSION,
      actor: this.replicaID,
      operations: [operation],
    }, this.limits);
    const normalizedOperation = normalized.operations[0]!;
    if (this.#pendingOperations.length >= this.limits.maxOperationsPerUpdate) {
      throw resourceLimit();
    }
    const operationBytes = encodedLength(canonicalJSON(normalizedOperation));
    const candidateBytes = nativeUpdateFixedBytes(this.replicaID) + this.#pendingOperationBytes + operationBytes + (this.#pendingOperations.length === 0 ? 0 : 1);
    // Check the future outbound envelope before local state changes.
    if (candidateBytes > this.limits.maxUpdateBytes) {
      throw resourceLimit();
    }
    const prepared = this.#prepareNormalized({
      version: NATIVE_UPDATE_VERSION,
      actor: this.replicaID,
      operations: [normalizedOperation],
    });
    this.#applyPrepared(prepared);
    this.#pendingOperations.push(normalizedOperation);
    this.#pendingOperationBytes += operationBytes;
    if (this.#transactionDepth === 0) {
      this.#flushLocal();
    }
    return normalizedOperation;
  }

  #prepare(update: NativeUpdate): PreparedUpdate {
    return this.#prepareNormalized(normalizeUpdate(update, this.limits));
  }

  #prepareNormalized(normalized: NativeUpdate): PreparedUpdate {
    const roots = new Map<string, Root>();
    const newRoots = new Map<string, Root>();
    const mapOperations = new Map<NativeMap, Array<NativeMapSetOperation | NativeMapDeleteOperation>>();
    const arrayInserts = new Map<NativeArray, NativeArrayInsertOperation[]>();
    const arrayDeletes = new Map<NativeArray, NativeArrayDeleteOperation[]>();

    for (const operation of normalized.operations) {
      const expected = rootTypeFor(operation);
      let root = roots.get(operation.target) ?? this.#roots.get(operation.target);
      if (root === undefined) {
        if (this.#roots.size + newRoots.size >= this.limits.maxRootTypes) {
          throw resourceLimit();
        }
        root = expected === "map" ? new NativeMap(this, operation.target) : new NativeArray(this, operation.target);
        newRoots.set(operation.target, root);
      } else if ((expected === "map" && !(root instanceof NativeMap)) || (expected === "array" && !(root instanceof NativeArray))) {
        throw typeConflict();
      }
      roots.set(operation.target, root);
      if (root instanceof NativeMap) {
        const operations = mapOperations.get(root) ?? [];
        operations.push(operation as NativeMapSetOperation | NativeMapDeleteOperation);
        mapOperations.set(root, operations);
      } else if (operation.kind === "array-insert") {
        const operations = arrayInserts.get(root) ?? [];
        operations.push(operation);
        arrayInserts.set(root, operations);
      } else {
        const operations = arrayDeletes.get(root) ?? [];
        operations.push(operation as NativeArrayDeleteOperation);
        arrayDeletes.set(root, operations);
      }
    }
    for (const [root, operations] of mapOperations) {
      root._preflight(operations);
    }
    for (const root of new Set([...arrayInserts.keys(), ...arrayDeletes.keys()])) {
      root._preflight(arrayInserts.get(root) ?? [], arrayDeletes.get(root) ?? []);
    }
    return { update: normalized, roots, newRoots };
  }

  #applyPrepared(prepared: PreparedUpdate): boolean {
    for (const [name, root] of prepared.newRoots) {
      this.#roots.set(name, root);
    }
    let changed = false;
    let ownCounter = this.#counter;
    for (const operation of prepared.update.operations) {
      const root = prepared.roots.get(operation.target)!;
      const rootChanged = root instanceof NativeMap ? root._apply(operation as NativeMapSetOperation | NativeMapDeleteOperation) : root._apply(operation as NativeArrayInsertOperation | NativeArrayDeleteOperation);
      if (rootChanged) {
        changed = true;
        this.#changedRoots.add(root);
      }
      for (const id of operationIDs(operation)) {
        if (id.actor === this.replicaID) {
          ownCounter = Math.max(ownCounter, id.counter);
        }
      }
    }
    this.#counter = ownCounter;
    return changed;
  }

  #flushLocal(): void {
    if (this.#pendingOperations.length === 0) {
      return;
    }
    const update = normalizeUpdate(
      { version: NATIVE_UPDATE_VERSION, actor: this.replicaID, operations: this.#pendingOperations },
      this.limits,
    );
    this.#pendingOperations = [];
    this.#pendingOperationBytes = 0;
    const origin = this.#transactionOrigin;
    this.#transactionOrigin = undefined;
    this.#emit(update, origin, true);
  }

  #emit(update: NativeUpdate, origin: unknown, local: boolean): void {
    const updateEvent: NativeUpdateEvent = { update, origin, local };
    for (const listener of [...this.#updateListeners]) {
      listener(updateEvent);
    }
    const changedRoots = [...this.#changedRoots];
    this.#changedRoots.clear();
    for (const root of changedRoots) {
      root._notify({ ...updateEvent, target: root });
    }
  }

  _assertRootName(name: string): void {
    assertBoundedText(name, this.limits.maxRootNameBytes, false);
  }

  #assertRootCapacity(): void {
    if (this.#roots.size >= this.limits.maxRootTypes) {
      throw resourceLimit();
    }
  }
}

/** Encodes one canonical native-ts-v1 update. It is not a Go CRDT frame. */
export function encodeNativeUpdate(
  update: NativeUpdate,
  limits: NativeDocumentOptions = {},
): Uint8Array {
  const resolved = resolveLimits(limits);
  const normalized = normalizeUpdate(update, resolved);
  return TEXT_ENCODER.encode(canonicalJSON(normalized));
}

/** Decodes only canonical UTF-8 JSON accepted by the native-ts-v1 validator. */
export function decodeNativeUpdate(
  encoded: Uint8Array,
  limits: NativeDocumentOptions = {},
): NativeUpdate {
  const resolved = resolveLimits(limits);
  if (!(encoded instanceof Uint8Array) || encoded.length === 0 || encoded.length > resolved.maxUpdateBytes) {
    throw resourceLimit();
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(TEXT_DECODER.decode(encoded));
  } catch {
    throw invalidUpdate();
  }
  const normalized = normalizeUpdate(parsed, resolved);
  if (!matchesUTF8(canonicalJSON(normalized), encoded)) {
    throw invalidUpdate();
  }
  return normalized;
}

function normalizeUpdate(value: unknown, limits: Readonly<NativeDocumentLimits>): NativeUpdate {
  const record = requireRecord(value);
  assertExactKeys(record, ["actor", "operations", "version"]);
  if (record.version !== NATIVE_UPDATE_VERSION || !Array.isArray(record.operations) || record.operations.length === 0) {
    throw invalidUpdate();
  }
  assertReplicaID(record.actor, limits);
  if (record.operations.length > limits.maxOperationsPerUpdate) {
    throw resourceLimit();
  }
  const operations = record.operations.map((operation) => normalizeOperation(operation, limits));
  const update: NativeUpdate = { version: NATIVE_UPDATE_VERSION, actor: record.actor, operations };
  if (utf8ByteLength(canonicalJSON(update)) > limits.maxUpdateBytes) {
    throw resourceLimit();
  }
  return update;
}

function normalizeOperation(value: unknown, limits: Readonly<NativeDocumentLimits>): NativeOperation {
  const record = requireRecord(value);
  if (typeof record.kind !== "string" || typeof record.target !== "string") {
    throw invalidUpdate();
  }
  assertBoundedText(record.target, limits.maxRootNameBytes, false);
  switch (record.kind) {
    case "map-set": {
      assertExactKeys(record, ["id", "key", "kind", "target", "value"]);
      assertBoundedText(record.key, limits.maxMapKeyBytes, false);
      return {
        kind: "map-set",
        target: record.target,
        key: record.key,
        id: normalizeID(record.id, limits),
        value: copyAndValidateValue(record.value, limits),
      };
    }
    case "map-delete": {
      assertExactKeys(record, ["id", "key", "kind", "target"]);
      assertBoundedText(record.key, limits.maxMapKeyBytes, false);
      return { kind: "map-delete", target: record.target, key: record.key, id: normalizeID(record.id, limits) };
    }
    case "array-insert": {
      assertExactKeys(record, ["entries", "kind", "target"]);
      if (!Array.isArray(record.entries)) {
        throw invalidUpdate();
      }
      if (record.entries.length === 0) {
        throw invalidUpdate();
      }
      if (record.entries.length > limits.maxArrayItems) {
        throw resourceLimit();
      }
      return {
        kind: "array-insert",
        target: record.target,
        entries: record.entries.map((entry) => normalizeArrayEntry(entry, limits)),
      };
    }
    case "array-delete": {
      assertExactKeys(record, ["ids", "kind", "target"]);
      if (!Array.isArray(record.ids)) {
        throw invalidUpdate();
      }
      if (record.ids.length === 0) {
        throw invalidUpdate();
      }
      if (record.ids.length > limits.maxArrayTombstones) {
        throw resourceLimit();
      }
      const ids = record.ids.map((id) => normalizeID(id, limits));
      if (new Set(ids.map(idKey)).size !== ids.length) {
        throw invalidUpdate();
      }
      return { kind: "array-delete", target: record.target, ids };
    }
    default:
      throw invalidUpdate();
  }
}

function normalizeArrayEntry(value: unknown, limits: Readonly<NativeDocumentLimits>): NativeArrayEntry {
  const record = requireRecord(value);
  assertExactKeys(record, ["after", "id", "value"]);
  return {
    id: normalizeID(record.id, limits),
    after: record.after === null ? null : normalizeID(record.after, limits),
    value: copyAndValidateValue(record.value, limits),
  };
}

function normalizeRoot(value: unknown, limits: Readonly<NativeDocumentLimits>): NativeRoot {
  const record = requireRecord(value);
  assertExactKeys(record, ["name", "type"]);
  assertBoundedText(record.name, limits.maxRootNameBytes, false);
  if (record.type !== "map" && record.type !== "array") {
    throw invalidUpdate();
  }
  return { name: record.name, type: record.type };
}

function resolveLimits(options: NativeDocumentOptions): Readonly<NativeDocumentLimits> {
  if (!isRecord(options)) {
    throw invalidUpdate();
  }
  const limits = { ...DEFAULT_NATIVE_LIMITS, ...options };
  for (const value of Object.values(limits)) {
    if (!Number.isSafeInteger(value) || value <= 0) {
      throw resourceLimit();
    }
  }
  if (limits.maxValueBytes > limits.maxUpdateBytes) {
    throw resourceLimit();
  }
  return Object.freeze(limits);
}

function assertReplicaID(value: unknown, limits: Readonly<NativeDocumentLimits>): asserts value is string {
  assertBoundedText(value, limits.maxReplicaIDBytes, true);
}

function assertBoundedText(value: unknown, maxBytes: number, rejectWhitespace: boolean): asserts value is string {
  if (
    typeof value !== "string" ||
    value.length === 0 ||
    hasUnpairedSurrogate(value) ||
    (rejectWhitespace && value.trim().length === 0)
  ) {
    throw invalidUpdate();
  }
  if (utf8ByteLength(value) > maxBytes) {
    throw resourceLimit();
  }
}

function hasUnpairedSurrogate(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (code >= 0xd800 && code <= 0xdbff) {
      const following = value.charCodeAt(index + 1);
      if (following < 0xdc00 || following > 0xdfff) {
        return true;
      }
      index += 1;
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      return true;
    }
  }
  return false;
}

function normalizeID(value: unknown, limits: Readonly<NativeDocumentLimits>): NativeID {
  const record = requireRecord(value);
  assertExactKeys(record, ["actor", "counter"]);
  assertReplicaID(record.actor, limits);
  if (!isPositiveSafeInteger(record.counter)) {
    throw invalidUpdate();
  }
  return { actor: record.actor, counter: record.counter };
}

function copyAndValidateValue(value: unknown, limits: Readonly<NativeDocumentLimits>): NativeValue {
  const copied = copyValue(value, limits, 0, { count: 0 }, new Set<object>());
  if (utf8ByteLength(canonicalJSON(copied)) > limits.maxValueBytes) {
    throw resourceLimit();
  }
  return copied;
}

function copyValue(
  value: unknown,
  limits: Readonly<NativeDocumentLimits>,
  depth: number,
  count: { count: number },
  seen: Set<object>,
): NativeValue {
  if (depth > limits.maxValueDepth) {
    throw resourceLimit();
  }
  count.count += 1;
  if (count.count > limits.maxValueItems) {
    throw resourceLimit();
  }
  if (value === null || typeof value === "boolean" || typeof value === "string") {
    return value;
  }
  if (typeof value === "number") {
    if (!Number.isFinite(value)) {
      throw invalidUpdate();
    }
    return value;
  }
  if (Array.isArray(value)) {
    if (seen.has(value)) {
      throw invalidUpdate();
    }
    seen.add(value);
    const copied = value.map((item) => copyValue(item, limits, depth + 1, count, seen));
    seen.delete(value);
    return copied;
  }
  if (isRecord(value)) {
    if (seen.has(value)) {
      throw invalidUpdate();
    }
    seen.add(value);
    const copied: Record<string, NativeValue> = {};
    for (const key of Object.keys(value).sort(compareText)) {
      assertBoundedText(key, limits.maxMapKeyBytes, false);
      defineOwnValue(copied, key, copyValue(value[key], limits, depth + 1, count, seen));
    }
    seen.delete(value);
    return copied;
  }
  throw invalidUpdate();
}

function cloneValue(value: NativeValue): NativeValue {
  if (value === null || typeof value !== "object") {
    return value;
  }
  if (Array.isArray(value)) {
    return value.map(cloneValue);
  }
  const copied: Record<string, NativeValue> = {};
  for (const [key, child] of Object.entries(value)) {
    defineOwnValue(copied, key, cloneValue(child));
  }
  return copied;
}

function assertAcyclic(
  incoming: ReadonlyMap<string, ArrayNode>,
  existing: ReadonlyMap<string, ArrayNode>,
  pending: ReadonlyMap<string, ArrayNode>,
): void {
  const visiting = new Set<string>();
  const visited = new Set<string>();
  const visit = (key: string): void => {
    if (visited.has(key)) {
      return;
    }
    if (visiting.has(key)) {
      throw stateConflict();
    }
    const node = incoming.get(key) ?? existing.get(key) ?? pending.get(key);
    if (node === undefined) {
      return;
    }
    visiting.add(key);
    if (node.after !== null) {
      visit(idKey(node.after));
    }
    visiting.delete(key);
    visited.add(key);
  };
  for (const key of incoming.keys()) {
    visit(key);
  }
}

function isResolvable(
  entry: ArrayNode,
  incoming: ReadonlyMap<string, ArrayNode>,
  existing: ReadonlyMap<string, ArrayNode>,
  resolving: Set<string>,
  resolved: Set<string>,
): boolean {
  const key = idKey(entry.id);
  if (resolved.has(key)) {
    return true;
  }
  if (entry.after === null || existing.has(idKey(entry.after))) {
    resolved.add(key);
    return true;
  }
  if (resolving.has(key)) {
    return false;
  }
  const parent = incoming.get(idKey(entry.after));
  if (parent === undefined) {
    return false;
  }
  resolving.add(key);
  const result = isResolvable(parent, incoming, existing, resolving, resolved);
  resolving.delete(key);
  if (result) {
    resolved.add(key);
  }
  return result;
}

function topologicalNodes(nodes: ReadonlyMap<string, ArrayNode>): ArrayNode[] {
  const children = new Map<string, ArrayNode[]>();
  for (const node of nodes.values()) {
    const parent = node.after === null ? ROOT_PARENT : idKey(node.after);
    const values = children.get(parent) ?? [];
    values.push(node);
    children.set(parent, values);
  }
  for (const values of children.values()) {
    values.sort((left, right) => compareID(left.id, right.id));
  }
  const output: ArrayNode[] = [];
  const stack: ArrayNode[] = [];
  pushChildren(stack, children.get(ROOT_PARENT));
  while (stack.length !== 0) {
    const node = stack.pop()!;
    output.push(copyNode(node));
    pushChildren(stack, children.get(idKey(node.id)));
  }
  return output;
}

function pushChildren(stack: ArrayNode[], children: readonly ArrayNode[] | undefined): void {
  if (children === undefined) {
    return;
  }
  for (let index = children.length - 1; index >= 0; index -= 1) {
    stack.push(children[index]!);
  }
}

function chunkArrayEntries(
  entries: readonly ArrayNode[],
  limits: Readonly<NativeDocumentLimits>,
  actor: string,
  target: string,
): NativeArrayEntry[][] {
  const chunks: NativeArrayEntry[][] = [];
  let current: NativeArrayEntry[] = [];
  const prefix = `{"actor":${canonicalJSON(actor)},"operations":[{"entries":[`;
  const suffix = `],"kind":"array-insert","target":${canonicalJSON(target)}}],"version":1}`;
  const fixedBytes = encodedLength(prefix) + encodedLength(suffix);
  let currentBytes = fixedBytes;
  for (const entry of entries) {
    const entryBytes = encodedLength(canonicalJSON(entry));
    const candidateBytes = currentBytes + (current.length === 0 ? 0 : 1) + entryBytes;
    if (candidateBytes > limits.maxUpdateBytes) {
      if (current.length === 0) {
        throw resourceLimit();
      }
      chunks.push(current);
      current = [copyNode(entry)];
      currentBytes = fixedBytes + entryBytes;
    } else {
      current.push(copyNode(entry));
      currentBytes = candidateBytes;
    }
  }
  if (current.length !== 0) {
    chunks.push(current);
  }
  return chunks;
}

function packOperations(
  operations: readonly NativeOperation[],
  actor: string,
  limits: Readonly<NativeDocumentLimits>,
): NativeUpdate[] {
  const updates: NativeUpdate[] = [];
  let current: NativeOperation[] = [];
  const prefix = `{"actor":${canonicalJSON(actor)},"operations":[`;
  const suffix = "],\"version\":1}";
  const fixedBytes = encodedLength(prefix) + encodedLength(suffix);
  let currentBytes = fixedBytes;
  for (const operation of operations) {
    const operationBytes = encodedLength(canonicalJSON(operation));
    const candidateBytes = currentBytes + (current.length === 0 ? 0 : 1) + operationBytes;
    if (
      current.length + 1 > limits.maxOperationsPerUpdate ||
      candidateBytes > limits.maxUpdateBytes
    ) {
      if (current.length === 0) {
        throw resourceLimit();
      }
      updates.push(normalizeUpdate({ version: NATIVE_UPDATE_VERSION, actor, operations: current }, limits));
      current = [operation];
      currentBytes = fixedBytes + operationBytes;
    } else {
      current.push(operation);
      currentBytes = candidateBytes;
    }
  }
  if (current.length !== 0) {
    updates.push(normalizeUpdate({ version: NATIVE_UPDATE_VERSION, actor, operations: current }, limits));
  }
  return updates;
}

function rootTypeFor(operation: NativeOperation): RootType {
  return operation.kind.startsWith("map-") ? "map" : "array";
}

function operationIDs(operation: NativeOperation): readonly NativeID[] {
  switch (operation.kind) {
    case "map-set":
    case "map-delete":
      return [operation.id];
    case "array-insert":
      return operation.entries.map((entry) => entry.id);
    case "array-delete":
      return operation.ids;
  }
}

function equalMapOperation(entry: MapEntry, operation: NativeMapSetOperation | NativeMapDeleteOperation): boolean {
  if (entry.present !== (operation.kind === "map-set")) {
    return false;
  }
  return operation.kind === "map-delete" || canonicalJSON(entry.value) === canonicalJSON(operation.value);
}

function equalNode(left: ArrayNode, right: NativeArrayEntry): boolean {
  return equalID(left.id, right.id) && equalNullableID(left.after, right.after) && canonicalJSON(left.value) === canonicalJSON(right.value);
}

function copyNode(value: NativeArrayEntry): ArrayNode {
  return { id: copyID(value.id), after: value.after === null ? null : copyID(value.after), value: cloneValue(value.value) };
}

function copyID(value: NativeID): NativeID {
  return { actor: value.actor, counter: value.counter };
}

function equalNullableID(left: NativeID | null, right: NativeID | null): boolean {
  return left === null ? right === null : right !== null && equalID(left, right);
}

function equalID(left: NativeID, right: NativeID): boolean {
  return left.counter === right.counter && left.actor === right.actor;
}

function compareID(left: NativeID, right: NativeID): number {
  return left.counter === right.counter ? compareText(left.actor, right.actor) : left.counter - right.counter;
}

/** UTF-8 bytewise order is stable across browser locale settings. */
function compareText(left: string, right: string): number {
  let leftIndex = 0;
  let rightIndex = 0;
  while (leftIndex < left.length && rightIndex < right.length) {
    const leftCodePoint = utf8CodePointAt(left, leftIndex);
    const rightCodePoint = utf8CodePointAt(right, rightIndex);
    if (leftCodePoint !== rightCodePoint) {
      return leftCodePoint - rightCodePoint;
    }
    leftIndex += utf8CodePointWidth(left, leftIndex);
    rightIndex += utf8CodePointWidth(right, rightIndex);
  }
  return leftIndex === left.length ? (rightIndex === right.length ? 0 : -1) : 1;
}

function idKey(id: NativeID): string {
  return `${id.counter}:${id.actor}`;
}

function parseIDKey(value: string): NativeID {
  const separator = value.indexOf(":");
  const counter = Number(value.slice(0, separator));
  const actor = value.slice(separator + 1);
  if (!isPositiveSafeInteger(counter) || actor.length === 0) {
    throw stateConflict();
  }
  return { actor, counter };
}

function assertArrayIndex(index: number, length: number, allowEnd: boolean): void {
  if (!Number.isSafeInteger(index) || index < 0 || index > length || (!allowEnd && index === length)) {
    throw invalidUpdate();
  }
}

function canonicalJSON(value: unknown): string {
  if (value === null) {
    return "null";
  }
  switch (typeof value) {
    case "string":
      return JSON.stringify(value);
    case "boolean":
      return value ? "true" : "false";
    case "number":
      if (!Number.isFinite(value)) {
        throw invalidUpdate();
      }
      return JSON.stringify(value);
    case "object":
      if (Array.isArray(value)) {
        return `[${value.map(canonicalJSON).join(",")}]`;
      }
      if (isRecord(value)) {
        return `{${Object.keys(value)
          .sort(compareText)
          .map((key) => `${JSON.stringify(key)}:${canonicalJSON(value[key])}`)
          .join(",")}}`;
      }
      throw invalidUpdate();
    default:
      throw invalidUpdate();
  }
}

function requireRecord(value: unknown): Record<string, unknown> {
  if (!isRecord(value)) {
    throw invalidUpdate();
  }
  return value;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    return false;
  }
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function assertExactKeys(value: Record<string, unknown>, keys: readonly string[]): void {
  const actual = Object.keys(value).sort(compareText);
  if (actual.length !== keys.length || actual.some((key, index) => key !== keys[index])) {
    throw invalidUpdate();
  }
}

function encodedLength(value: string): number {
  return utf8ByteLength(value);
}

function nativeUpdateFixedBytes(actor: string): number {
  return encodedLength(`{"actor":${canonicalJSON(actor)},"operations":[`) + encodedLength("],\"version\":1}");
}

/** Matches TextEncoder's replacement behaviour without allocating a byte array. */
function utf8ByteLength(value: string): number {
  let length = 0;
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (code <= 0x7f) {
      length += 1;
    } else if (code <= 0x7ff) {
      length += 2;
    } else if (code >= 0xd800 && code <= 0xdbff && isLowSurrogate(value.charCodeAt(index + 1))) {
      length += 4;
      index += 1;
    } else {
      // An unpaired surrogate becomes the three-byte U+FFFD replacement.
      length += 3;
    }
  }
  return length;
}

/** Compares canonical text to wire bytes without allocating another encoded update. */
function matchesUTF8(value: string, encoded: Uint8Array): boolean {
  let byteIndex = 0;
  for (let index = 0; index < value.length; index += 1) {
    const codePoint = utf8CodePointAt(value, index);
    index += utf8CodePointWidth(value, index) - 1;
    if (codePoint <= 0x7f) {
      if (byteIndex >= encoded.length || encoded[byteIndex] !== codePoint) {
        return false;
      }
      byteIndex += 1;
    } else if (codePoint <= 0x7ff) {
      if (
        byteIndex + 2 > encoded.length ||
        encoded[byteIndex] !== (0xc0 | (codePoint >> 6)) ||
        encoded[byteIndex + 1] !== (0x80 | (codePoint & 0x3f))
      ) {
        return false;
      }
      byteIndex += 2;
    } else if (codePoint <= 0xffff) {
      if (
        byteIndex + 3 > encoded.length ||
        encoded[byteIndex] !== (0xe0 | (codePoint >> 12)) ||
        encoded[byteIndex + 1] !== (0x80 | ((codePoint >> 6) & 0x3f)) ||
        encoded[byteIndex + 2] !== (0x80 | (codePoint & 0x3f))
      ) {
        return false;
      }
      byteIndex += 3;
    } else {
      if (
        byteIndex + 4 > encoded.length ||
        encoded[byteIndex] !== (0xf0 | (codePoint >> 18)) ||
        encoded[byteIndex + 1] !== (0x80 | ((codePoint >> 12) & 0x3f)) ||
        encoded[byteIndex + 2] !== (0x80 | ((codePoint >> 6) & 0x3f)) ||
        encoded[byteIndex + 3] !== (0x80 | (codePoint & 0x3f))
      ) {
        return false;
      }
      byteIndex += 4;
    }
  }
  return byteIndex === encoded.length;
}

/** Returns the scalar TextEncoder would emit at an UTF-16 offset. */
function utf8CodePointAt(value: string, index: number): number {
  const code = value.charCodeAt(index);
  if (code >= 0xd800 && code <= 0xdbff && isLowSurrogate(value.charCodeAt(index + 1))) {
    return ((code - 0xd800) << 10) + value.charCodeAt(index + 1) - 0xdc00 + 0x10000;
  }
  return code >= 0xd800 && code <= 0xdfff ? 0xfffd : code;
}

function utf8CodePointWidth(value: string, index: number): number {
  const code = value.charCodeAt(index);
  return code >= 0xd800 && code <= 0xdbff && isLowSurrogate(value.charCodeAt(index + 1)) ? 2 : 1;
}

function isLowSurrogate(value: number): boolean {
  return value >= 0xdc00 && value <= 0xdfff;
}

function defineOwnValue(target: Record<string, unknown>, key: string, value: unknown): void {
  Object.defineProperty(target, key, { configurable: true, enumerable: true, value, writable: true });
}

function isPositiveSafeInteger(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0;
}

function isPositiveOrZeroSafeInteger(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}

function invalidUpdate(): NativeCRDTError {
  return new NativeCRDTError("invalid_update");
}

function resourceLimit(): NativeCRDTError {
  return new NativeCRDTError("resource_limit");
}

function stateConflict(): NativeCRDTError {
  return new NativeCRDTError("state_conflict");
}

function incompleteState(): NativeCRDTError {
  return new NativeCRDTError("incomplete_state");
}

function typeConflict(): NativeCRDTError {
  return new NativeCRDTError("type_conflict");
}
