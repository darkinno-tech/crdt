/**
 * native-ts-nested-v1 adds single-owner nested shared maps and arrays on top
 * of native-ts-v1 updates. The outer frame is still a native update, but a
 * nested group must be negotiated as this separate semantic contract: a plain
 * NativeDocument deliberately treats the reference values as opaque JSON.
 *
 * A nested container is named by the immutable operation ID that integrates
 * it. It can therefore have one parent only, cannot be moved or aliased, and
 * cannot form a cycle without reusing an ID. Child updates may arrive before
 * their parent reference; they wait in a bounded document queue and a snapshot
 * refuses to serialize that incomplete state.
 */

import {
  DEFAULT_NATIVE_LIMITS,
  decodeNativeUpdate,
  encodeNativeUpdate,
  NATIVE_UPDATE_VERSION,
  NativeArray,
  NativeCRDTError,
  NativeDocument,
  NativeMap,
} from "./native.js";
import type {
  NativeArrayDeleteOperation,
  NativeArrayEntry,
  NativeArrayInsertOperation,
  NativeDocumentLimits,
  NativeDocumentOptions,
  NativeID,
  NativeMapDeleteOperation,
  NativeMapSetOperation,
  NativeOperation,
  NativeRoot,
  NativeSnapshot,
  NativeUpdate,
  NativeUpdateEvent,
  NativeUpdateListener,
  NativeValue,
} from "./native.js";

export const NATIVE_NESTED_SEMANTICS = "native-ts-nested-v1";

const TARGET_PREFIX = "\u0001darkinno:nested:";
const TARGET_SEPARATOR = "\u0000";

export type NativeNestedType = "map" | "array";
export type NativeNestedScalar = NativeValue;
export type NativeNestedValue = NativeNestedScalar | NativeNestedMap | NativeNestedArray;

interface NestedReference {
  readonly $crdt: typeof NATIVE_NESTED_SEMANTICS;
  readonly id: NativeID;
  readonly type: NativeNestedType;
}

export interface NativeNestedDocumentOptions extends NativeDocumentOptions {
  /** Maximum retained child-container identities, including detached history. */
  readonly maxNestedTypes?: number;
  /** Maximum operations held while waiting for a parent container reference. */
  readonly maxPendingNestedOperations?: number;
}

interface NestedLimits {
  readonly maxNestedTypes: number;
  readonly maxPendingNestedOperations: number;
}

export interface NativeNestedContainerSnapshot {
  readonly id: NativeID;
  readonly type: NativeNestedType;
}

/** Persist this complete object atomically before reusing the replica ID. */
export interface NativeNestedSnapshot {
  readonly version: 1;
  readonly native: NativeSnapshot;
  readonly containers: readonly NativeNestedContainerSnapshot[];
}

export interface NativeNestedTypeEvent extends NativeUpdateEvent {
  readonly target: NativeNestedMap | NativeNestedArray;
}

export type NativeNestedTypeListener = (event: NativeNestedTypeEvent) => void;

type RawRoot = NativeMap<NativeValue> | NativeArray<NativeValue>;
type NestedRoot = NativeNestedMap | NativeNestedArray;

interface MapState {
  readonly id: NativeID;
  readonly reference?: NestedReference;
}

interface ArrayState {
  readonly reference?: NestedReference;
}

interface Metadata {
  readonly containers: ReadonlyMap<string, NativeNestedType>;
  readonly mapStates: ReadonlyMap<string, MapState>;
  readonly arrayStates: ReadonlyMap<string, ArrayState>;
  readonly deletedArrayEntries: ReadonlySet<string>;
  readonly activeReferences: ReadonlyMap<string, string>;
}

interface PendingUpdate {
  readonly key: string;
  readonly update: NativeUpdate;
}

/**
 * A nested LWW map. Plain JSON values remain atomic; use createMap or
 * createArray when descendants must merge independently.
 */
export class NativeNestedMap {
  readonly #listeners = new Set<NativeNestedTypeListener>();

  constructor(
    private readonly document: NativeNestedDocument,
    readonly name: string,
  ) {}

  get size(): number {
    return this.#raw().size;
  }

  has(key: string): boolean {
    return this.#raw().has(key);
  }

  get(key: string): NativeNestedValue | undefined {
    const value = this.#raw().get(key);
    return value === undefined ? undefined : this.document._fromStoredValue(value);
  }

  set(key: string, value: NativeNestedScalar): this {
    this.document._setMapScalar(this.name, key, value);
    return this;
  }

  createMap(key: string): NativeNestedMap {
    return this.document._setMapChild(this.name, key, "map") as NativeNestedMap;
  }

  createArray(key: string): NativeNestedArray {
    return this.document._setMapChild(this.name, key, "array") as NativeNestedArray;
  }

  delete(key: string): boolean {
    return this.document._deleteMapKey(this.name, key);
  }

  entries(): IterableIterator<[string, NativeNestedValue]> {
    const values = [...this.#raw().entries()].map(([key, value]) => [key, this.document._fromStoredValue(value)] as [string, NativeNestedValue]);
    return values[Symbol.iterator]();
  }

  toJSON(): Record<string, NativeValue | Record<string, NativeValue> | NativeValue[]> {
    const value: Record<string, NativeValue | Record<string, NativeValue> | NativeValue[]> = {};
    for (const [key, nested] of this.entries()) {
      Object.defineProperty(value, key, {
        configurable: true,
        enumerable: true,
        value: nested instanceof NativeNestedMap || nested instanceof NativeNestedArray ? nested.toJSON() : nested,
        writable: true,
      });
    }
    return value;
  }

  observe(listener: NativeNestedTypeListener): () => void {
    this.document._assertOpen();
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  }

  /** @internal */
  _notify(event: NativeNestedTypeEvent): void {
    for (const listener of [...this.#listeners]) {
      listener(event);
    }
  }

  #raw(): NativeMap<NativeValue> {
    return this.document._rawMap(this.name);
  }
}

/** An RGA sequence whose elements may be atomic JSON or single-owner children. */
export class NativeNestedArray {
  readonly #listeners = new Set<NativeNestedTypeListener>();

  constructor(
    private readonly document: NativeNestedDocument,
    readonly name: string,
  ) {}

  get length(): number {
    return this.#raw().length;
  }

  get pendingCount(): number {
    return this.#raw().pendingCount;
  }

  get(index: number): NativeNestedValue | undefined {
    const value = this.#raw().get(index);
    return value === undefined ? undefined : this.document._fromStoredValue(value);
  }

  insert(index: number, values: readonly NativeNestedScalar[]): void {
    this.document._insertArrayScalars(this.name, index, values);
  }

  push(values: readonly NativeNestedScalar[]): void {
    this.insert(this.length, values);
  }

  insertMap(index: number): NativeNestedMap {
    return this.document._insertArrayChild(this.name, index, "map") as NativeNestedMap;
  }

  insertArray(index: number): NativeNestedArray {
    return this.document._insertArrayChild(this.name, index, "array") as NativeNestedArray;
  }

  pushMap(): NativeNestedMap {
    return this.insertMap(this.length);
  }

  pushArray(): NativeNestedArray {
    return this.insertArray(this.length);
  }

  delete(index: number, count = 1): void {
    this.document._deleteArrayItems(this.name, index, count);
  }

  toArray(): NativeNestedValue[] {
    return this.#raw().toArray().map((value) => this.document._fromStoredValue(value));
  }

  toJSON(): Array<NativeValue | Record<string, NativeValue> | NativeValue[]> {
    return this.toArray().map((value) => value instanceof NativeNestedMap || value instanceof NativeNestedArray ? value.toJSON() : value);
  }

  observe(listener: NativeNestedTypeListener): () => void {
    this.document._assertOpen();
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  }

  /** @internal */
  _notify(event: NativeNestedTypeEvent): void {
    for (const listener of [...this.#listeners]) {
      listener(event);
    }
  }

  #raw(): NativeArray<NativeValue> {
    return this.document._rawArray(this.name);
  }
}

/**
 * NativeNestedDocument owns named roots, recursively integrated containers,
 * transaction boundaries, and bounded pending child traffic. It is transport
 * neutral: hosts still authenticate peers, bind this exact semantics version,
 * cap bodies before allocation, and persist snapshots atomically.
 */
export class NativeNestedDocument {
  readonly limits: Readonly<NativeDocumentLimits>;
  readonly nestedLimits: Readonly<NestedLimits>;
  readonly #native: NativeDocument;
  readonly #roots = new Map<string, NativeNestedType>();
  readonly #wrappers = new Map<string, NestedRoot>();
  #containers = new Map<string, NativeNestedType>();
  #mapStates = new Map<string, MapState>();
  #arrayStates = new Map<string, ArrayState>();
  #deletedArrayEntries = new Set<string>();
  #activeReferences = new Map<string, string>();
  readonly #updateListeners = new Set<NativeUpdateListener>();
  readonly #pending = new Map<string, PendingUpdate>();
  #pendingOperations = 0;
  #transactionDepth = 0;
  #transactionOrigin: unknown;
  #localOperations: NativeOperation[] = [];

  constructor(replicaID: string, options: NativeNestedDocumentOptions = {}) {
    const nestedLimits = resolveNestedLimits(options);
    const rawOptions: NativeDocumentOptions = {
      ...options,
      maxRootTypes: rawRootLimit(options, nestedLimits),
    };
    delete (rawOptions as { maxNestedTypes?: number }).maxNestedTypes;
    delete (rawOptions as { maxPendingNestedOperations?: number }).maxPendingNestedOperations;
    this.#native = new NativeDocument(replicaID, rawOptions);
    this.limits = this.#native.limits;
    this.nestedLimits = nestedLimits;
    targetFor({ actor: replicaID, counter: Number.MAX_SAFE_INTEGER }, this.limits);
  }

  get replicaID(): string {
    return this.#native.replicaID;
  }

  getMap(name: string): NativeNestedMap {
    this._assertOpen();
    assertPublicRootName(name);
    const known = this.#roots.get(name);
    if (known !== undefined && known !== "map") {
      throw new NativeCRDTError("type_conflict");
    }
    this.#native.getMap(name);
    this.#roots.set(name, "map");
    return this.#wrapper(name, "map") as NativeNestedMap;
  }

  getArray(name: string): NativeNestedArray {
    this._assertOpen();
    assertPublicRootName(name);
    const known = this.#roots.get(name);
    if (known !== undefined && known !== "array") {
      throw new NativeCRDTError("type_conflict");
    }
    this.#native.getArray(name);
    this.#roots.set(name, "array");
    return this.#wrapper(name, "array") as NativeNestedArray;
  }

  transact<T>(callback: () => T, origin?: unknown): T {
    this._assertOpen();
    if (this.#transactionDepth === 0) {
      this.#transactionOrigin = origin;
    }
    this.#transactionDepth += 1;
    try {
      return this.#native.transact(callback, origin);
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

  applyUpdate(update: NativeUpdate, origin?: unknown): boolean {
    this._assertOpen();
    const normalized = normalizedUpdate(update, this.limits);
    return this.#applyRemote(normalized, origin, true);
  }

  applyEncodedUpdate(encoded: Uint8Array, origin?: unknown): boolean {
    this._assertOpen();
    return this.#applyRemote(decodeNativeUpdate(encoded, this.limits), origin, true);
  }

  encodeStateAsUpdates(): NativeUpdate[] {
    this._assertOpen();
    if (this.#pending.size !== 0) {
      throw new NativeCRDTError("invalid_update");
    }
    return this.#native.encodeStateAsUpdates();
  }

  snapshot(): NativeNestedSnapshot {
    this._assertOpen();
    if (this.#pending.size !== 0) {
      throw new NativeCRDTError("invalid_update");
    }
    const containers = [...this.#containers.entries()]
      .map(([target, type]) => ({ id: idFromTarget(target, this.limits), type }))
      .sort((left, right) => compareID(left.id, right.id));
    return { version: 1, native: this.#native.snapshot(), containers };
  }

  static restore(replicaID: string, snapshot: NativeNestedSnapshot, options: NativeNestedDocumentOptions = {}): NativeNestedDocument {
    if (!isRecord(snapshot) || snapshot.version !== 1 || !isRecord(snapshot.native) || !Array.isArray(snapshot.containers)) {
      throw new NativeCRDTError("invalid_update");
    }
    const document = new NativeNestedDocument(replicaID, options);
    for (const item of snapshot.containers) {
      const container = normalizeContainer(item, document.limits);
      document.#registerContainer(container.id, container.type);
    }
    const native = snapshot.native as NativeSnapshot;
    if (!Array.isArray(native.roots) || !Array.isArray(native.updates) || !isPositiveOrZeroSafeInteger(native.counter)) {
      throw new NativeCRDTError("invalid_update");
    }
    for (const root of native.roots) {
      const normalized = normalizeRoot(root, document.limits);
      if (isNestedTarget(normalized.name)) {
        const expected = document.#containers.get(normalized.name);
        if (expected !== normalized.type) {
          throw new NativeCRDTError("invalid_update");
        }
        document.#ensureRaw(normalized.name, normalized.type);
      } else if (normalized.type === "map") {
        document.getMap(normalized.name);
      } else {
        document.getArray(normalized.name);
      }
    }
    for (const update of native.updates) {
      document.applyUpdate(update, "restore");
    }
    document.#native._setCounterAtLeast(native.counter);
    return document;
  }

  close(): boolean {
    this.#updateListeners.clear();
    this.#pending.clear();
    this.#pendingOperations = 0;
    this.#localOperations = [];
    this.#wrappers.clear();
    return this.#native.close();
  }

  /** @internal */
  _assertOpen(): void {
    this.#native._assertOpen();
  }

  /** @internal */
  _rawMap(target: string): NativeMap<NativeValue> {
    this._assertOpen();
    this.#assertTargetType(target, "map");
    return this.#native.getMap(target);
  }

  /** @internal */
  _rawArray(target: string): NativeArray<NativeValue> {
    this._assertOpen();
    this.#assertTargetType(target, "array");
    return this.#native.getArray(target);
  }

  /** @internal */
  _fromStoredValue(value: NativeValue): NativeNestedValue {
    const reference = referenceFromValue(value, this.limits);
    if (reference === undefined) {
      return value;
    }
    const target = targetFor(reference.id, this.limits);
    const type = this.#containers.get(target);
    if (type !== reference.type) {
      throw new NativeCRDTError("state_conflict");
    }
    return this.#wrapper(target, type);
  }

  /** @internal */
  _setMapScalar(target: string, key: string, value: NativeNestedScalar): void {
    this.#assertTargetType(target, "map");
    assertPlainValue(value, this.limits);
    const operation: NativeMapSetOperation = {
      kind: "map-set",
      target,
      key,
      id: this.#native._nextID(),
      value,
    };
    this.#applyLocal(operation);
  }

  /** @internal */
  _setMapChild(target: string, key: string, type: NativeNestedType): NativeNestedMap | NativeNestedArray {
    this.#assertTargetType(target, "map");
    const id = this.#native._peekNextID();
    const reference = makeReference(id, type, this.limits);
    this.#applyLocal({ kind: "map-set", target, key, id, value: reference as unknown as NativeValue });
    return this.#wrapper(targetFor(id, this.limits), type);
  }

  /** @internal */
  _deleteMapKey(target: string, key: string): boolean {
    const raw = this._rawMap(target);
    const existed = raw.has(key);
    this.#applyLocal({ kind: "map-delete", target, key, id: this.#native._nextID() });
    return existed;
  }

  /** @internal */
  _insertArrayScalars(target: string, index: number, values: readonly NativeNestedScalar[]): void {
    this.#assertTargetType(target, "array");
    if (values.length === 0) {
      return;
    }
    for (const value of values) {
      assertPlainValue(value, this.limits);
    }
    const entries = values.map((value) => ({ id: this.#native._nextID(), value }));
    const operation = this._rawArray(target)._insertWithIDs(index, entries);
    this.#commitLocal(operation);
  }

  /** @internal */
  _insertArrayChild(target: string, index: number, type: NativeNestedType): NativeNestedMap | NativeNestedArray {
    this.#assertTargetType(target, "array");
    const id = this.#native._peekNextID();
    const operation = this._rawArray(target)._planInsertWithIDs(index, [{ id, value: makeReference(id, type, this.limits) as unknown as NativeValue }]);
    this.#applyLocal(operation);
    return this.#wrapper(targetFor(id, this.limits), type);
  }

  /** @internal */
  _deleteArrayItems(target: string, index: number, count: number): void {
    this.#assertTargetType(target, "array");
    const operation = this._rawArray(target).delete(index, count);
    if (operation.ids.length !== 0) {
      this.#commitLocal(operation);
    }
  }

  #applyLocal(operation: NativeOperation): void {
    const update = normalizedUpdate({
      version: NATIVE_UPDATE_VERSION,
      actor: this.replicaID,
      operations: [operation],
    }, this.limits);
    const staged = this.#stageMetadata(update);
    if (staged === undefined) {
      throw new NativeCRDTError("state_conflict");
    }
    const applied = this.#native._applyLocal(operation);
    this.#commitLocal(applied, staged);
  }

  #commitLocal(operation: NativeOperation, staged?: Metadata): void {
    const update: NativeUpdate = { version: NATIVE_UPDATE_VERSION, actor: this.replicaID, operations: [operation] };
    const resolved = staged ?? this.#stageMetadata(update);
    if (resolved === undefined) {
      throw new NativeCRDTError("state_conflict");
    }
    this.#installMetadata(resolved);
    this.#localOperations.push(operation);
    if (this.#transactionDepth === 0) {
      this.#flushLocal();
    }
  }

  #flushLocal(): void {
    if (this.#localOperations.length === 0) {
      return;
    }
    const update = normalizedUpdate({
      version: NATIVE_UPDATE_VERSION,
      actor: this.replicaID,
      operations: this.#localOperations,
    }, this.limits);
    this.#localOperations = [];
    const origin = this.#transactionOrigin;
    this.#transactionOrigin = undefined;
    this.#emit(update, origin, true);
  }

  #applyRemote(update: NativeUpdate, origin: unknown, allowPending: boolean): boolean {
    if (this.#transactionDepth !== 0) {
      throw new NativeCRDTError("transaction_active");
    }
    const staged = this.#stageMetadata(update);
    if (staged === undefined) {
      if (!allowPending) {
        return false;
      }
      this.#enqueue(update);
      return false;
    }
    const changed = this.#native.applyUpdate(update, origin);
    this.#installMetadata(staged);
    this.#materializeAppearing(update);
    if (changed) {
      this.#emit(update, origin);
    }
    this.#drainPending(origin);
    return changed;
  }

  #enqueue(update: NativeUpdate): void {
    const key = updateKey(update, this.limits);
    if (this.#pending.has(key)) {
      return;
    }
    if (this.#pendingOperations + update.operations.length > this.nestedLimits.maxPendingNestedOperations) {
      throw new NativeCRDTError("resource_limit");
    }
    this.#pending.set(key, { key, update });
    this.#pendingOperations += update.operations.length;
  }

  #drainPending(origin: unknown): void {
    let progressed = true;
    while (progressed) {
      progressed = false;
      for (const pending of [...this.#pending.values()]) {
        const staged = this.#stageMetadata(pending.update);
        if (staged === undefined) {
          continue;
        }
        this.#pending.delete(pending.key);
        this.#pendingOperations -= pending.update.operations.length;
        const changed = this.#native.applyUpdate(pending.update, origin);
        this.#installMetadata(staged);
        this.#materializeAppearing(pending.update);
        if (changed) {
          this.#emit(pending.update, origin);
        }
        progressed = true;
      }
    }
  }

  #stageMetadata(update: NativeUpdate): Metadata | undefined {
    const containers = new Map(this.#containers);
    const mapStates = new Map(this.#mapStates);
    const arrayStates = new Map(this.#arrayStates);
    const deletedArrayEntries = new Set(this.#deletedArrayEntries);
    const activeReferences = new Map(this.#activeReferences);
    const pendingContainers = new Map<string, NativeNestedType>();

    for (const operation of update.operations) {
      for (const reference of referencesForOperation(operation, this.limits)) {
        const target = targetFor(reference.id, this.limits);
        const known = containers.get(target) ?? pendingContainers.get(target);
        if (known !== undefined && known !== reference.type) {
          throw new NativeCRDTError("type_conflict");
        }
        if (known === undefined) {
          if (containers.size + pendingContainers.size >= this.nestedLimits.maxNestedTypes) {
            throw new NativeCRDTError("resource_limit");
          }
          pendingContainers.set(target, reference.type);
        }
      }
    }

    for (const operation of update.operations) {
      const expected = operation.kind.startsWith("map-") ? "map" : "array";
      if (isNestedTarget(operation.target)) {
        const known = containers.get(operation.target) ?? pendingContainers.get(operation.target);
        if (known === undefined) {
          return undefined;
        }
        if (known !== expected) {
          throw new NativeCRDTError("type_conflict");
        }
      }
    }

    for (const operation of update.operations) {
      if (operation.kind === "map-set" || operation.kind === "map-delete") {
        const slot = mapSlot(operation.target, operation.key);
        const current = mapStates.get(slot);
        if (current !== undefined && compareID(operation.id, current.id) < 0) {
          continue;
        }
        if (current !== undefined && equalID(operation.id, current.id)) {
          continue;
        }
        if (current?.reference !== undefined) {
          activeReferences.delete(idKey(current.reference.id));
        }
        const reference = operation.kind === "map-set" ? operationReference(operation, this.limits) : undefined;
        if (reference !== undefined) {
          activateReference(activeReferences, reference, slot);
        }
        mapStates.set(slot, reference === undefined ? { id: copyID(operation.id) } : { id: copyID(operation.id), reference });
        continue;
      }
      if (operation.kind === "array-insert") {
        for (const entry of operation.entries) {
          const slot = arraySlot(operation.target, entry.id);
          if (arrayStates.has(slot)) {
            continue;
          }
          const reference = entryReference(entry, this.limits);
          if (reference !== undefined && !deletedArrayEntries.has(slot)) {
            activateReference(activeReferences, reference, slot);
          }
          arrayStates.set(slot, reference === undefined ? {} : { reference });
        }
        continue;
      }
      for (const id of operation.ids) {
        const slot = arraySlot(operation.target, id);
        if (deletedArrayEntries.has(slot)) {
          continue;
        }
        deletedArrayEntries.add(slot);
        const reference = arrayStates.get(slot)?.reference;
        if (reference !== undefined) {
          activeReferences.delete(idKey(reference.id));
        }
      }
    }
    for (const [target, type] of pendingContainers) {
      containers.set(target, type);
    }
    return { containers, mapStates, arrayStates, deletedArrayEntries, activeReferences };
  }

  #installMetadata(metadata: Metadata): void {
    this.#containers = new Map(metadata.containers);
    this.#mapStates = new Map(metadata.mapStates);
    this.#arrayStates = new Map(metadata.arrayStates);
    this.#deletedArrayEntries = new Set(metadata.deletedArrayEntries);
    this.#activeReferences = new Map(metadata.activeReferences);
  }

  #registerContainer(id: NativeID, type: NativeNestedType): void {
    const target = targetFor(id, this.limits);
    const known = this.#containers.get(target);
    if (known !== undefined && known !== type) {
      throw new NativeCRDTError("type_conflict");
    }
    if (known === undefined && this.#containers.size >= this.nestedLimits.maxNestedTypes) {
      throw new NativeCRDTError("resource_limit");
    }
    this.#containers.set(target, type);
  }

  #wrapper(target: string, type: NativeNestedType): NestedRoot {
    const existing = this.#wrappers.get(target);
    if (existing !== undefined) {
      if ((type === "map" && !(existing instanceof NativeNestedMap)) || (type === "array" && !(existing instanceof NativeNestedArray))) {
        throw new NativeCRDTError("type_conflict");
      }
      return existing;
    }
    this.#ensureRaw(target, type);
    const wrapper = type === "map" ? new NativeNestedMap(this, target) : new NativeNestedArray(this, target);
    this.#wrappers.set(target, wrapper);
    return wrapper;
  }

  #ensureRaw(target: string, type: NativeNestedType): RawRoot {
    return type === "map" ? this.#native.getMap(target) : this.#native.getArray(target);
  }

  #assertTargetType(target: string, expected: NativeNestedType): void {
    if (isNestedTarget(target)) {
      if (this.#containers.get(target) !== expected) {
        throw new NativeCRDTError("type_conflict");
      }
      return;
    }
    if (this.#roots.get(target) !== expected) {
      throw new NativeCRDTError("type_conflict");
    }
  }

  #materializeAppearing(update: NativeUpdate): void {
    for (const operation of update.operations) {
      if (isNestedTarget(operation.target)) {
        const type = this.#containers.get(operation.target);
        if (type !== undefined) {
          this.#ensureRaw(operation.target, type);
        }
      }
      for (const reference of referencesForOperation(operation, this.limits)) {
        this.#ensureRaw(targetFor(reference.id, this.limits), reference.type);
      }
    }
  }

  #emit(update: NativeUpdate, origin: unknown, local = false): void {
    const event: NativeUpdateEvent = { update, origin, local };
    for (const listener of [...this.#updateListeners]) {
      listener(event);
    }
    const targets = new Set(update.operations.map((operation) => operation.target));
    for (const target of targets) {
      const type = isNestedTarget(target) ? this.#containers.get(target) : this.#roots.get(target);
      if (type === undefined) {
        continue;
      }
      const wrapper = this.#wrapper(target, type);
      wrapper._notify({ ...event, target: wrapper });
    }
  }
}

function resolveNestedLimits(options: NativeNestedDocumentOptions): Readonly<NestedLimits> {
  const maxNestedTypes = options.maxNestedTypes ?? 10_000;
  const maxPendingNestedOperations = options.maxPendingNestedOperations ?? DEFAULT_NATIVE_LIMITS.maxPendingItems;
  if (!isPositiveSafeInteger(maxNestedTypes) || !isPositiveSafeInteger(maxPendingNestedOperations)) {
    throw new NativeCRDTError("resource_limit");
  }
  return Object.freeze({ maxNestedTypes, maxPendingNestedOperations });
}

function rawRootLimit(options: NativeNestedDocumentOptions, nested: Readonly<NestedLimits>): number {
  const roots = options.maxRootTypes ?? DEFAULT_NATIVE_LIMITS.maxRootTypes;
  if (!isPositiveSafeInteger(roots) || roots > Number.MAX_SAFE_INTEGER - nested.maxNestedTypes) {
    throw new NativeCRDTError("resource_limit");
  }
  return roots + nested.maxNestedTypes;
}

function normalizedUpdate(update: NativeUpdate, limits: NativeDocumentOptions): NativeUpdate {
  return decodeNativeUpdate(encodeNativeUpdate(update, limits), limits);
}

function makeReference(id: NativeID, type: NativeNestedType, limits: Readonly<NativeDocumentLimits>): NestedReference {
  const copied = copyID(id);
  targetFor(copied, limits);
  return { $crdt: NATIVE_NESTED_SEMANTICS, id: copied, type };
}

function operationReference(operation: NativeOperation, limits: Readonly<NativeDocumentLimits>): NestedReference | undefined {
  if (operation.kind === "map-set") {
    const reference = referenceFromValue(operation.value, limits);
    if (reference !== undefined && !equalID(reference.id, operation.id)) {
      throw new NativeCRDTError("state_conflict");
    }
    return reference;
  }
  return undefined;
}

function entryReference(entry: NativeArrayEntry, limits: Readonly<NativeDocumentLimits>): NestedReference | undefined {
  const reference = referenceFromValue(entry.value, limits);
  if (reference !== undefined && !equalID(reference.id, entry.id)) {
    throw new NativeCRDTError("state_conflict");
  }
  return reference;
}

function referencesForOperation(operation: NativeOperation, limits: Readonly<NativeDocumentLimits>): NestedReference[] {
  if (operation.kind === "map-set") {
    const reference = operationReference(operation, limits);
    return reference === undefined ? [] : [reference];
  }
  if (operation.kind !== "array-insert") {
    return [];
  }
  return operation.entries.flatMap((entry) => {
    const reference = entryReference(entry, limits);
    return reference === undefined ? [] : [reference];
  });
}

function referenceFromValue(value: NativeValue, limits: Readonly<NativeDocumentLimits>): NestedReference | undefined {
  if (!isRecord(value)) {
    return undefined;
  }
  const marker = value.$crdt;
  if (marker !== NATIVE_NESTED_SEMANTICS) {
    assertNoNestedMarker(value);
    return undefined;
  }
  if (!exactKeys(value, ["$crdt", "id", "type"]) || (value.type !== "map" && value.type !== "array")) {
    throw new NativeCRDTError("invalid_update");
  }
  const id = normalizeID(value.id, limits);
  targetFor(id, limits);
  return { $crdt: NATIVE_NESTED_SEMANTICS, id, type: value.type };
}

function assertPlainValue(value: NativeValue, limits: Readonly<NativeDocumentLimits>): void {
  // Reuse the canonical native validator, then reserve the marker namespace at
  // every depth so ordinary JSON can never masquerade as a shared child.
  const normalized = decodeNativeUpdate(encodeNativeUpdate({
    version: NATIVE_UPDATE_VERSION,
    actor: "v",
    operations: [{ kind: "map-set", target: "v", key: "v", id: { actor: "v", counter: 1 }, value }],
  }, limits), limits);
  assertNoNestedMarker((normalized.operations[0] as NativeMapSetOperation).value);
}

function assertNoNestedMarker(value: NativeValue): void {
  if (value === null || typeof value !== "object") {
    return;
  }
  if (Array.isArray(value)) {
    for (const item of value) {
      assertNoNestedMarker(item);
    }
    return;
  }
  if ((value as Record<string, NativeValue>).$crdt === NATIVE_NESTED_SEMANTICS) {
    throw new NativeCRDTError("invalid_update");
  }
  for (const item of Object.values(value)) {
    assertNoNestedMarker(item);
  }
}

function activateReference(active: Map<string, string>, reference: NestedReference, slot: string): void {
  const key = idKey(reference.id);
  const current = active.get(key);
  if (current !== undefined && current !== slot) {
    throw new NativeCRDTError("state_conflict");
  }
  active.set(key, slot);
}

function targetFor(id: NativeID, limits: Readonly<NativeDocumentLimits>): string {
  const target = `${TARGET_PREFIX}${id.actor}${TARGET_SEPARATOR}${id.counter}`;
  if (utf8Length(target) > limits.maxRootNameBytes) {
    throw new NativeCRDTError("resource_limit");
  }
  return target;
}

function idFromTarget(target: string, limits: Readonly<NativeDocumentLimits>): NativeID {
  if (!isNestedTarget(target)) {
    throw new NativeCRDTError("invalid_update");
  }
  const separator = target.lastIndexOf(TARGET_SEPARATOR);
  const actor = target.slice(TARGET_PREFIX.length, separator);
  const counter = Number(target.slice(separator + TARGET_SEPARATOR.length));
  const id = normalizeID({ actor, counter }, limits);
  if (targetFor(id, limits) !== target) {
    throw new NativeCRDTError("invalid_update");
  }
  return id;
}

function isNestedTarget(value: string): boolean {
  return value.startsWith(TARGET_PREFIX) && value.lastIndexOf(TARGET_SEPARATOR) > TARGET_PREFIX.length;
}

function assertPublicRootName(name: string): void {
  if (isNestedTarget(name)) {
    throw new NativeCRDTError("invalid_update");
  }
}

function mapSlot(target: string, key: string): string {
  return `${target}\u0000map\u0000${key}`;
}

function arraySlot(target: string, id: NativeID): string {
  return `${target}\u0000array\u0000${idKey(id)}`;
}

function updateKey(update: NativeUpdate, limits: Readonly<NativeDocumentLimits>): string {
  return new TextDecoder().decode(encodeNativeUpdate(update, limits));
}

function normalizeContainer(value: unknown, limits: Readonly<NativeDocumentLimits>): NativeNestedContainerSnapshot {
  if (!isRecord(value) || !exactKeys(value, ["id", "type"]) || (value.type !== "map" && value.type !== "array")) {
    throw new NativeCRDTError("invalid_update");
  }
  return { id: normalizeID(value.id, limits), type: value.type };
}

function normalizeRoot(value: unknown, limits: Readonly<NativeDocumentLimits>): NativeRoot {
  if (!isRecord(value) || !exactKeys(value, ["name", "type"]) || typeof value.name !== "string" || (value.type !== "map" && value.type !== "array")) {
    throw new NativeCRDTError("invalid_update");
  }
  if (utf8Length(value.name) === 0 || utf8Length(value.name) > limits.maxRootNameBytes) {
    throw new NativeCRDTError("invalid_update");
  }
  return { name: value.name, type: value.type };
}

function normalizeID(value: unknown, limits: Readonly<NativeDocumentLimits>): NativeID {
  if (!isRecord(value) || !exactKeys(value, ["actor", "counter"]) || typeof value.actor !== "string" || !isPositiveSafeInteger(value.counter)) {
    throw new NativeCRDTError("invalid_update");
  }
  if (value.actor.trim().length === 0 || utf8Length(value.actor) > limits.maxReplicaIDBytes) {
    throw new NativeCRDTError("resource_limit");
  }
  return { actor: value.actor, counter: value.counter };
}

function copyID(id: NativeID): NativeID {
  return { actor: id.actor, counter: id.counter };
}

function idKey(id: NativeID): string {
  return `${id.actor}\u0000${id.counter}`;
}

function equalID(left: NativeID, right: NativeID): boolean {
  return left.actor === right.actor && left.counter === right.counter;
}

function compareID(left: NativeID, right: NativeID): number {
  if (left.counter !== right.counter) {
    return left.counter < right.counter ? -1 : 1;
  }
  return compareText(left.actor, right.actor);
}

function compareText(left: string, right: string): number {
  const a = new TextEncoder().encode(left);
  const b = new TextEncoder().encode(right);
  for (let index = 0; index < Math.min(a.length, b.length); index += 1) {
    if (a[index] !== b[index]) {
      return a[index]! < b[index]! ? -1 : 1;
    }
  }
  return a.length === b.length ? 0 : a.length < b.length ? -1 : 1;
}

function exactKeys(value: Record<string, unknown>, keys: readonly string[]): boolean {
  const actual = Object.keys(value).sort();
  return actual.length === keys.length && actual.every((key, index) => key === [...keys].sort()[index]);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function isPositiveSafeInteger(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0;
}

function isPositiveOrZeroSafeInteger(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}

function utf8Length(value: string): number {
  return new TextEncoder().encode(value).byteLength;
}
