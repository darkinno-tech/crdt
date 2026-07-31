/**
 * Bounded Counter, OR-Set, LWW register, and OR-Tree views over
 * `native-ts-v1` maps.
 *
 * This module deliberately does not invent a second transport format. A
 * `NativeCollectionsDocument` emits ordinary canonical `native-ts-v1`
 * updates, but reserves a private root namespace and adds the
 * `native-ts-collections-v1` semantic contract. Peers must negotiate that
 * contract, the root declarations, and compatible limits before exchanging
 * updates. It is not compatible with Go framed Counter/Set/LWW/Tree types.
 */

import {
  decodeNativeUpdate,
  encodeNativeUpdate,
  NativeCRDTError,
  NativeDocument,
  type NativeDocumentLimits,
  type NativeDocumentOptions,
  type NativeID,
  type NativeMap,
  type NativeSnapshot,
  type NativeUpdate,
  type NativeUpdateEvent,
  type NativeUpdateListener,
  type NativeValue,
} from "./native.js";

const TEXT_ENCODER = new TextEncoder();
const COLLECTION_NAMESPACE = "\u0000darkinno/collections/v1/";
const COLLECTION_MARKER = "native-ts-collections-v1";

export const NATIVE_COLLECTIONS_SEMANTICS = "native-ts-collections-v1";
export const NATIVE_COLLECTIONS_SNAPSHOT_VERSION = 1;

export type NativeCollectionType = "counter" | "orset" | "lww" | "tree";

export interface NativeCollectionLimits {
  readonly maxCounterComponents: number;
  readonly maxCounterDigits: number;
  readonly maxSetEntries: number;
  readonly maxSetTombstones: number;
  readonly maxTreeNodes: number;
  readonly maxTreeTombstones: number;
  readonly maxTreePendingNodes: number;
  readonly maxTreeDepth: number;
}

export interface NativeCollectionsDocumentOptions {
  readonly document?: NativeDocumentOptions;
  readonly collections?: Partial<NativeCollectionLimits>;
}

/** Conservative limits for a browser tab. Product manifests may lower them. */
export const DEFAULT_NATIVE_COLLECTION_LIMITS: Readonly<NativeCollectionLimits> = Object.freeze({
  maxCounterComponents: 10_000,
  maxCounterDigits: 128,
  maxSetEntries: 10_000,
  maxSetTombstones: 10_000,
  maxTreeNodes: 10_000,
  maxTreeTombstones: 10_000,
  maxTreePendingNodes: 10_000,
  maxTreeDepth: 128,
});

export interface NativeCollectionRoot {
  readonly name: string;
  readonly type: NativeCollectionType;
}

/** Persist this unit atomically with an authenticated outbox/frontier. */
export interface NativeCollectionsSnapshot {
  readonly version: typeof NATIVE_COLLECTIONS_SNAPSHOT_VERSION;
  readonly collections: readonly NativeCollectionRoot[];
  readonly native: NativeSnapshot;
}

export interface NativeTreeNode<T extends NativeValue = NativeValue> {
  readonly id: NativeID;
  readonly value: T;
  readonly children: readonly NativeTreeNode<T>[];
}

interface CounterComponent {
  readonly $crdt: typeof COLLECTION_MARKER;
  readonly type: "counter-component";
  readonly actor: string;
  readonly id: NativeID;
  readonly positive: string;
  readonly negative: string;
}

interface SetAdd {
  readonly $crdt: typeof COLLECTION_MARKER;
  readonly type: "orset-add";
  readonly id: NativeID;
  readonly value: NativeValue;
}

interface SetTombstone {
  readonly $crdt: typeof COLLECTION_MARKER;
  readonly type: "orset-tombstone";
  readonly id: NativeID;
  readonly removedBy: NativeID;
  readonly value: NativeValue;
}

interface LWWValue {
  readonly $crdt: typeof COLLECTION_MARKER;
  readonly type: "lww-register";
  readonly id: NativeID;
  readonly deleted: boolean;
  readonly value?: NativeValue;
}

interface TreeAdd {
  readonly $crdt: typeof COLLECTION_MARKER;
  readonly type: "ortree-node";
  readonly id: NativeID;
  readonly parent: NativeID | null;
  readonly value: NativeValue;
}

interface TreeTombstone {
  readonly $crdt: typeof COLLECTION_MARKER;
  readonly type: "ortree-tombstone";
  readonly id: NativeID;
  readonly removedBy: NativeID;
}

interface CollectionBinding {
  readonly name: string;
  readonly type: NativeCollectionType;
  readonly targets: readonly string[];
  validate(operations: readonly NativeMapSet[]): void;
  commit(operations: readonly NativeMapSet[]): void;
}

interface NativeMapSet {
  readonly kind: "map-set";
  readonly target: string;
  readonly key: string;
  readonly id: NativeID;
  readonly value: NativeValue;
}

/** A PN-Counter with arbitrary-precision read values and per-actor maxima. */
export class NativeCounter implements CollectionBinding {
  readonly targets: readonly string[];
  readonly #components: NativeMap<NativeValue>;

  constructor(
    private readonly document: NativeDocument,
    private readonly flushEvents: () => void,
    readonly name: string,
    private readonly limits: Readonly<NativeCollectionLimits>,
    target: string,
  ) {
    this.targets = [target];
    this.#components = document.getMap(target);
  }

  readonly type = "counter" as const;

  increment(amount: bigint = 1n): void {
    this.#change(amount, 0n);
  }

  decrement(amount: bigint = 1n): void {
    this.#change(0n, amount);
  }

  value(): bigint {
    let positive = 0n;
    let negative = 0n;
    for (const [, value] of this.#components.entries()) {
      const component = readCounterComponent(value, this.document.limits, this.limits);
      positive += BigInt(component.positive);
      negative += BigInt(component.negative);
    }
    return positive - negative;
  }

  componentCount(): number {
    return this.#components.size;
  }

  observe(listener: () => void): () => void {
    return this.#components.observe(() => listener());
  }

  validate(operations: readonly NativeMapSet[]): void {
    const current = new Map<string, CounterComponent>();
    for (const [, value] of this.#components.entries()) {
      const component = readCounterComponent(value, this.document.limits, this.limits);
      current.set(component.actor, component);
    }
    const incomingActors = new Set<string>();
    const byActor = new Map<string, CounterComponent[]>();
    for (const operation of operations) {
      requireTarget(operation, this.targets[0]!);
      const component = readCounterComponent(operation.value, this.document.limits, this.limits);
      if (!sameID(operation.id, component.id) || operation.key !== actorKey(component.actor)) {
        throw stateConflict();
      }
      incomingActors.add(component.actor);
      const entries = byActor.get(component.actor) ?? [];
      entries.push(component);
      byActor.set(component.actor, entries);
    }
    let newComponents = 0;
    for (const actor of incomingActors) {
      if (!current.has(actor)) {
        newComponents += 1;
      }
    }
    if (this.#components.size + newComponents > this.limits.maxCounterComponents) {
      throw resourceLimit();
    }
    for (const [actor, components] of byActor) {
      components.sort((left, right) => compareID(left.id, right.id));
      let previous = current.get(actor);
      for (const component of components) {
        if (previous !== undefined && compareID(component.id, previous.id) > 0) {
          if (BigInt(component.positive) < BigInt(previous.positive) || BigInt(component.negative) < BigInt(previous.negative)) {
            throw stateConflict();
          }
          previous = component;
        }
      }
    }
  }

  commit(_operations: readonly NativeMapSet[]): void {}

  #change(positiveDelta: bigint, negativeDelta: bigint): void {
    if (positiveDelta <= 0n && negativeDelta <= 0n) {
      throw invalidUpdate();
    }
    const actor = this.document.replicaID;
    const existing = this.#components.get(actorKey(actor));
    const previous = existing === undefined ? undefined : readCounterComponent(existing, this.document.limits, this.limits);
    const id = this.document._peekNextID();
    const positive = (previous === undefined ? 0n : BigInt(previous.positive)) + positiveDelta;
    const negative = (previous === undefined ? 0n : BigInt(previous.negative)) + negativeDelta;
    const component: CounterComponent = {
      $crdt: COLLECTION_MARKER,
      type: "counter-component",
      actor,
      id,
      positive: boundedDecimal(positive, this.limits),
      negative: boundedDecimal(negative, this.limits),
    };
    this.validate([{
      kind: "map-set",
      target: this.targets[0]!,
      key: actorKey(actor),
      id,
      value: component as unknown as NativeValue,
    }]);
    this.#components.set(actorKey(actor), component as unknown as NativeValue);
    this.flushEvents();
  }
}

/** An add-wins observed-remove set. Removes retain per-add tombstones. */
export class NativeORSet<T extends NativeValue = NativeValue> implements CollectionBinding {
  readonly targets: readonly string[];
  readonly #adds: NativeMap<NativeValue>;
  readonly #tombstones: NativeMap<NativeValue>;

  constructor(
    private readonly document: NativeDocument,
    private readonly flushEvents: () => void,
    readonly name: string,
    private readonly limits: Readonly<NativeCollectionLimits>,
    addsTarget: string,
    tombstonesTarget: string,
  ) {
    this.targets = [addsTarget, tombstonesTarget];
    this.#adds = document.getMap(addsTarget);
    this.#tombstones = document.getMap(tombstonesTarget);
  }

  readonly type = "orset" as const;

  add(value: T): void {
    const id = this.document._peekNextID();
    const entry: SetAdd = { $crdt: COLLECTION_MARKER, type: "orset-add", id, value };
    this.validate([{
      kind: "map-set",
      target: this.targets[0]!,
      key: idKey(id),
      id,
      value: entry as unknown as NativeValue,
    }]);
    this.#adds.set(idKey(id), entry as unknown as NativeValue);
    this.flushEvents();
  }

  remove(value: T): boolean {
    const wanted = canonicalValue(value);
    const matches: SetAdd[] = [];
    for (const [key, raw] of this.#adds.entries()) {
      if (this.#tombstones.has(key)) {
        continue;
      }
      const entry = readSetAdd(raw, this.document.limits);
      if (canonicalValue(entry.value) === wanted) {
        matches.push(entry);
      }
    }
    if (matches.length === 0) {
      return false;
    }
    this.document.transact(() => {
      for (const entry of matches) {
        const removedBy = this.document._peekNextID();
        const tombstone: SetTombstone = {
          $crdt: COLLECTION_MARKER,
          type: "orset-tombstone",
          id: entry.id,
          removedBy,
          value: entry.value,
        };
        this.validate([{
          kind: "map-set",
          target: this.targets[1]!,
          key: idKey(entry.id),
          id: removedBy,
          value: tombstone as unknown as NativeValue,
        }]);
        this.#tombstones.set(idKey(entry.id), tombstone as unknown as NativeValue);
      }
    });
    this.flushEvents();
    return true;
  }

  has(value: T): boolean {
    const wanted = canonicalValue(value);
    return this.#liveAdds().some((entry) => canonicalValue(entry.value) === wanted);
  }

  values(): T[] {
    const unique = new Map<string, T>();
    for (const entry of this.#liveAdds()) {
      unique.set(canonicalValue(entry.value), entry.value as T);
    }
    return [...unique.entries()].sort(([left], [right]) => compareText(left, right)).map(([, value]) => value);
  }

  get size(): number {
    return this.values().length;
  }

  tombstoneCount(): number {
    return this.#tombstones.size;
  }

  observe(listener: () => void): () => void {
    const stopAdds = this.#adds.observe(() => listener());
    const stopTombstones = this.#tombstones.observe(() => listener());
    return () => {
      stopAdds();
      stopTombstones();
    };
  }

  validate(operations: readonly NativeMapSet[]): void {
    const additions = new Map<string, SetAdd>();
    const tombstones: NativeMapSet[] = [];
    for (const operation of operations) {
      if (operation.target === this.targets[0]) {
        const entry = readSetAdd(operation.value, this.document.limits);
        if (!sameID(operation.id, entry.id) || operation.key !== idKey(entry.id)) {
          throw stateConflict();
        }
        const previous = additions.get(operation.key);
        if (previous !== undefined && canonicalValue(previous) !== canonicalValue(entry)) {
          throw stateConflict();
        }
        additions.set(operation.key, entry);
      } else if (operation.target === this.targets[1]) {
        tombstones.push(operation);
      } else {
        throw invalidUpdate();
      }
    }
    let newAdds = 0;
    for (const key of additions.keys()) {
      if (!this.#adds.has(key)) {
        newAdds += 1;
      }
    }
    if (this.#adds.size + newAdds > this.limits.maxSetEntries) {
      throw resourceLimit();
    }
    let newTombstones = 0;
    for (const operation of tombstones) {
      const tombstone = readSetTombstone(operation.value, this.document.limits);
      if (!sameID(operation.id, tombstone.removedBy) || operation.key !== idKey(tombstone.id)) {
        throw stateConflict();
      }
      const add = additions.get(operation.key) ?? this.#readAdd(operation.key);
      if (add !== undefined && canonicalValue(add.value) !== canonicalValue(tombstone.value)) {
        throw stateConflict();
      }
      if (!this.#tombstones.has(operation.key)) {
        newTombstones += 1;
      }
    }
    if (this.#tombstones.size + newTombstones > this.limits.maxSetTombstones) {
      throw resourceLimit();
    }
  }

  commit(_operations: readonly NativeMapSet[]): void {}

  #readAdd(key: string): SetAdd | undefined {
    const value = this.#adds.get(key);
    return value === undefined ? undefined : readSetAdd(value, this.document.limits);
  }

  #liveAdds(): SetAdd[] {
    const values: SetAdd[] = [];
    for (const [key, value] of this.#adds.entries()) {
      if (!this.#tombstones.has(key)) {
        values.push(readSetAdd(value, this.document.limits));
      }
    }
    return values;
  }
}

/** A retained-tombstone last-write-wins register. */
export class NativeLWWRegister<T extends NativeValue = NativeValue> implements CollectionBinding {
  readonly targets: readonly string[];
  readonly #entries: NativeMap<NativeValue>;

  constructor(
    private readonly document: NativeDocument,
    private readonly flushEvents: () => void,
    readonly name: string,
    target: string,
  ) {
    this.targets = [target];
    this.#entries = document.getMap(target);
  }

  readonly type = "lww" as const;

  set(value: T): void {
    const id = this.document._peekNextID();
    const entry: LWWValue = { $crdt: COLLECTION_MARKER, type: "lww-register", id, deleted: false, value };
    this.validate([{ kind: "map-set", target: this.targets[0]!, key: "value", id, value: entry as unknown as NativeValue }]);
    this.#entries.set("value", entry as unknown as NativeValue);
    this.flushEvents();
  }

  clear(): boolean {
    const existed = this.get() !== undefined;
    const id = this.document._peekNextID();
    const entry: LWWValue = { $crdt: COLLECTION_MARKER, type: "lww-register", id, deleted: true };
    this.validate([{ kind: "map-set", target: this.targets[0]!, key: "value", id, value: entry as unknown as NativeValue }]);
    this.#entries.set("value", entry as unknown as NativeValue);
    this.flushEvents();
    return existed;
  }

  get(): T | undefined {
    const value = this.#entries.get("value");
    if (value === undefined) {
      return undefined;
    }
    const entry = readLWWValue(value, this.document.limits);
    return entry.deleted ? undefined : entry.value as T;
  }

  observe(listener: () => void): () => void {
    return this.#entries.observe(() => listener());
  }

  validate(operations: readonly NativeMapSet[]): void {
    for (const operation of operations) {
      requireTarget(operation, this.targets[0]!);
      if (operation.key !== "value") {
        throw invalidUpdate();
      }
      const entry = readLWWValue(operation.value, this.document.limits);
      if (!sameID(operation.id, entry.id)) {
        throw stateConflict();
      }
    }
  }

  commit(_operations: readonly NativeMapSet[]): void {}
}

/** Immutable-parent observed-remove tree. Move is delete plus a new add. */
export class NativeORTree<T extends NativeValue = NativeValue> implements CollectionBinding {
  readonly targets: readonly string[];
  readonly #nodes: NativeMap<NativeValue>;
  readonly #tombstones: NativeMap<NativeValue>;
  /** Nodes and deletions are append-only under immutable IDs, so this avoids sorting/cloning every map on each receive. */
  readonly #nodeCache = new Map<string, TreeAdd>();
  readonly #deletedCache = new Set<string>();

  constructor(
    private readonly document: NativeDocument,
    private readonly flushEvents: () => void,
    readonly name: string,
    private readonly limits: Readonly<NativeCollectionLimits>,
    nodesTarget: string,
    tombstonesTarget: string,
  ) {
    this.targets = [nodesTarget, tombstonesTarget];
    this.#nodes = document.getMap(nodesTarget);
    this.#tombstones = document.getMap(tombstonesTarget);
  }

  readonly type = "tree" as const;

  add(parent: NativeID | null, value: T): NativeID {
    if (parent !== null && !this.#isLive(parent)) {
      throw stateConflict();
    }
    const id = this.document._peekNextID();
    const node: TreeAdd = { $crdt: COLLECTION_MARKER, type: "ortree-node", id, parent, value };
    this.validate([{ kind: "map-set", target: this.targets[0]!, key: idKey(id), id, value: node as unknown as NativeValue }]);
    this.#nodes.set(idKey(id), node as unknown as NativeValue);
    this.#cacheNode(idKey(id));
    this.flushEvents();
    return copyID(id);
  }

  remove(id: NativeID): boolean {
    const node = this.#readNode(idKey(id));
    if (node === undefined || this.#tombstones.has(idKey(id))) {
      return false;
    }
    const removedBy = this.document._peekNextID();
    const tombstone: TreeTombstone = { $crdt: COLLECTION_MARKER, type: "ortree-tombstone", id: node.id, removedBy };
    this.validate([{ kind: "map-set", target: this.targets[1]!, key: idKey(node.id), id: removedBy, value: tombstone as unknown as NativeValue }]);
    this.#tombstones.set(idKey(node.id), tombstone as unknown as NativeValue);
    this.#deletedCache.add(idKey(node.id));
    this.flushEvents();
    return true;
  }

  roots(): NativeTreeNode<T>[] {
    const nodes = this.#allNodes();
    const children = new Map<string, TreeAdd[]>();
    for (const node of nodes.values()) {
      const key = node.parent === null ? "root" : idKey(node.parent);
      const values = children.get(key) ?? [];
      values.push(node);
      children.set(key, values);
    }
    for (const values of children.values()) {
      values.sort((left, right) => compareID(right.id, left.id));
    }
    const materialize = (node: TreeAdd): NativeTreeNode<T> | undefined => {
      if (this.#deletedCache.has(idKey(node.id))) {
        return undefined;
      }
      const nested: NativeTreeNode<T>[] = [];
      for (const child of children.get(idKey(node.id)) ?? []) {
        const visible = materialize(child);
        if (visible !== undefined) {
          nested.push(visible);
        }
      }
      return { id: copyID(node.id), value: node.value as T, children: nested };
    };
    const roots: NativeTreeNode<T>[] = [];
    for (const node of children.get("root") ?? []) {
      const visible = materialize(node);
      if (visible !== undefined) {
        roots.push(visible);
      }
    }
    return roots;
  }

  nodeCount(): number {
    return this.#nodes.size;
  }

  tombstoneCount(): number {
    return this.#tombstones.size;
  }

  pendingCount(): number {
    let pending = 0;
    for (const node of this.#nodeCache.values()) {
      if (node.parent !== null && !this.#nodeCache.has(idKey(node.parent))) {
        pending += 1;
      }
    }
    return pending;
  }

  observe(listener: () => void): () => void {
    const stopNodes = this.#nodes.observe(() => listener());
    const stopTombstones = this.#tombstones.observe(() => listener());
    return () => {
      stopNodes();
      stopTombstones();
    };
  }

  validate(operations: readonly NativeMapSet[]): void {
    const incoming = new Map<string, TreeAdd>();
    const tombstones: NativeMapSet[] = [];
    for (const operation of operations) {
      if (operation.target === this.targets[0]) {
        const node = readTreeAdd(operation.value, this.document.limits);
        if (!sameID(operation.id, node.id) || operation.key !== idKey(node.id) || (node.parent !== null && sameID(node.id, node.parent))) {
          throw stateConflict();
        }
        const previous = incoming.get(operation.key);
        if (previous !== undefined && canonicalValue(previous) !== canonicalValue(node)) {
          throw stateConflict();
        }
        incoming.set(operation.key, node);
      } else if (operation.target === this.targets[1]) {
        tombstones.push(operation);
      } else {
        throw invalidUpdate();
      }
    }
    let newNodes = 0;
    for (const key of incoming.keys()) {
      if (!this.#nodes.has(key)) {
        newNodes += 1;
      }
    }
    if (this.#nodes.size + newNodes > this.limits.maxTreeNodes) {
      throw resourceLimit();
    }
    let newTombstones = 0;
    for (const operation of tombstones) {
      const tombstone = readTreeTombstone(operation.value, this.document.limits);
      if (!sameID(operation.id, tombstone.removedBy) || operation.key !== idKey(tombstone.id)) {
        throw stateConflict();
      }
      if (!this.#tombstones.has(operation.key)) {
        newTombstones += 1;
      }
    }
    if (this.#tombstones.size + newTombstones > this.limits.maxTreeTombstones) {
      throw resourceLimit();
    }
    const newNodeKeys = new Set<string>();
    for (const [key, node] of incoming) {
      const existing = this.#nodeCache.get(key);
      if (existing !== undefined && canonicalValue(existing) !== canonicalValue(node)) {
        throw stateConflict();
      }
      if (existing === undefined) {
        newNodeKeys.add(key);
      }
    }
    // Existing nodes are already acyclic and immutable. An incoming add can
    // only create a cycle through its own parent chain, so validate that
    // bounded chain instead of re-walking the complete tree per packet.
    for (const node of incoming.values()) {
      let cursor: TreeAdd | undefined = node;
      const seen = new Set<string>();
      let depth = 0;
      while (cursor !== undefined) {
        const key = idKey(cursor.id);
        if (seen.has(key)) {
          throw stateConflict();
        }
        seen.add(key);
        depth += 1;
        if (depth > this.limits.maxTreeDepth) {
          throw resourceLimit();
        }
        if (cursor.parent === null) {
          break;
        }
        const parentKey = idKey(cursor.parent);
        cursor = incoming.get(parentKey) ?? this.#nodeCache.get(parentKey);
      }
    }
    // Pending accounting is intentionally one linear pass. It accepts a
    // parent that resolves pre-existing children in the same packet and never
    // grows an unbounded wait set, while avoiding the former O(nodes x depth)
    // validation pass for every ordinary tree update.
    let pending = 0;
    for (const node of this.#nodeCache.values()) {
      if (node.parent !== null && !this.#nodeCache.has(idKey(node.parent)) && !incoming.has(idKey(node.parent))) {
        pending += 1;
      }
    }
    for (const [key, node] of incoming) {
      if (newNodeKeys.has(key) && node.parent !== null && !this.#nodeCache.has(idKey(node.parent)) && !incoming.has(idKey(node.parent))) {
        pending += 1;
      }
    }
    if (pending > this.limits.maxTreePendingNodes) {
      throw resourceLimit();
    }
  }

  #isLive(id: NativeID): boolean {
    return this.#nodeCache.has(idKey(id)) && !this.#deletedCache.has(idKey(id));
  }

  #readNode(key: string): TreeAdd | undefined {
    const value = this.#nodes.get(key);
    return value === undefined ? undefined : readTreeAdd(value, this.document.limits);
  }

  #allNodes(): Map<string, TreeAdd> {
    return new Map(this.#nodeCache);
  }

  commit(operations: readonly NativeMapSet[]): void {
    for (const operation of operations) {
      if (operation.target === this.targets[0]) {
        this.#cacheNode(operation.key);
      } else if (operation.target === this.targets[1]) {
        this.#deletedCache.add(operation.key);
      }
    }
  }

  #cacheNode(key: string): void {
    const node = this.#readNode(key);
    if (node !== undefined) {
      this.#nodeCache.set(key, node);
    }
  }
}

/**
 * Owns collection declarations and an internal native document. Do not apply
 * collection updates to a raw `NativeDocument`: doing so bypasses semantic
 * validation such as monotone counter components and immutable tree parents.
 */
export class NativeCollectionsDocument {
  readonly limits: Readonly<NativeCollectionLimits>;
  readonly documentLimits: Readonly<NativeDocumentLimits>;
  readonly #document: NativeDocument;
  readonly #collections = new Map<string, CollectionBinding>();
  readonly #targets = new Map<string, CollectionBinding>();
  readonly #updateListeners = new Set<NativeUpdateListener>();
  readonly #pendingEvents: NativeUpdateEvent[] = [];

  constructor(
    readonly replicaID: string,
    options: NativeCollectionsDocumentOptions = {},
  ) {
    this.#document = new NativeDocument(replicaID, options.document);
    this.documentLimits = this.#document.limits;
    this.limits = resolveCollectionLimits(options.collections, this.documentLimits);
    this.#document.onUpdate((event) => this.#pendingEvents.push(event));
  }

  getCounter(name: string): NativeCounter {
    return this.#get(name, "counter") as NativeCounter;
  }

  getORSet<T extends NativeValue = NativeValue>(name: string): NativeORSet<T> {
    return this.#get(name, "orset") as NativeORSet<T>;
  }

  getLWWRegister<T extends NativeValue = NativeValue>(name: string): NativeLWWRegister<T> {
    return this.#get(name, "lww") as NativeLWWRegister<T>;
  }

  getORTree<T extends NativeValue = NativeValue>(name: string): NativeORTree<T> {
    return this.#get(name, "tree") as NativeORTree<T>;
  }

  transact<T>(callback: () => T, origin?: unknown): T {
    try {
      return this.#document.transact(callback, origin);
    } finally {
      this.#flushEvents();
    }
  }

  onUpdate(listener: NativeUpdateListener): () => void {
    this.#updateListeners.add(listener);
    return () => this.#updateListeners.delete(listener);
  }

  /** Validates the complete collection semantic envelope before base-map mutation. */
  applyUpdate(update: NativeUpdate, origin?: unknown): boolean {
    const normalized = decodeNativeUpdate(encodeNativeUpdate(update, this.documentLimits), this.documentLimits);
    const grouped = this.#validate(normalized);
    const changed = this.#document.applyUpdate(normalized, origin);
    if (changed) {
      for (const [collection, operations] of grouped) {
        collection.commit(operations);
      }
    }
    this.#flushEvents();
    return changed;
  }

  applyEncodedUpdate(encoded: Uint8Array, origin?: unknown): boolean {
    const normalized = decodeNativeUpdate(encoded, this.documentLimits);
    const grouped = this.#validate(normalized);
    const changed = this.#document.applyUpdate(normalized, origin);
    if (changed) {
      for (const [collection, operations] of grouped) {
        collection.commit(operations);
      }
    }
    this.#flushEvents();
    return changed;
  }

  snapshot(): NativeCollectionsSnapshot {
    const collections = [...this.#collections.values()]
      .sort((left, right) => compareText(left.name, right.name))
      .map((collection) => ({ name: collection.name, type: collection.type }));
    return { version: NATIVE_COLLECTIONS_SNAPSHOT_VERSION, collections, native: this.#document.snapshot() };
  }

  static restore(
    replicaID: string,
    snapshot: NativeCollectionsSnapshot,
    options: NativeCollectionsDocumentOptions = {},
  ): NativeCollectionsDocument {
    if (!isRecord(snapshot) || snapshot.version !== NATIVE_COLLECTIONS_SNAPSHOT_VERSION || !Array.isArray(snapshot.collections) || !isNativeSnapshot(snapshot.native)) {
      throw invalidUpdate();
    }
    const document = new NativeCollectionsDocument(replicaID, options);
    const names = new Set<string>();
    for (const root of snapshot.collections) {
      if (!isRecord(root) || typeof root.name !== "string" || !isCollectionType(root.type) || names.has(root.name)) {
        throw invalidUpdate();
      }
      names.add(root.name);
      document.#get(root.name, root.type);
    }
    const expectedTargets = new Set(document.#targets.keys());
    if (snapshot.native.roots.length !== expectedTargets.size) {
      throw invalidUpdate();
    }
    for (const root of snapshot.native.roots) {
      if (!isRecord(root) || root.type !== "map" || typeof root.name !== "string" || !expectedTargets.delete(root.name)) {
        throw invalidUpdate();
      }
    }
    if (expectedTargets.size !== 0) {
      throw invalidUpdate();
    }
    for (const update of snapshot.native.updates) {
      document.applyUpdate(update, "restore");
    }
    document.#document._setCounterAtLeast(snapshot.native.counter);
    return document;
  }

  close(): boolean {
    const closed = this.#document.close();
    this.#pendingEvents.length = 0;
    this.#updateListeners.clear();
    return closed;
  }

  #get(name: string, type: NativeCollectionType): CollectionBinding {
    assertCollectionName(name, this.#document);
    const existing = this.#collections.get(name);
    if (existing !== undefined) {
      if (existing.type !== type) {
        throw typeConflict();
      }
      return existing;
    }
    let collection: CollectionBinding;
    switch (type) {
      case "counter":
        collection = new NativeCounter(this.#document, () => this.#flushEvents(), name, this.limits, internalTarget(name, "counter"));
        break;
      case "orset":
        collection = new NativeORSet(this.#document, () => this.#flushEvents(), name, this.limits, internalTarget(name, "orset-adds"), internalTarget(name, "orset-tombstones"));
        break;
      case "lww":
        collection = new NativeLWWRegister(this.#document, () => this.#flushEvents(), name, internalTarget(name, "lww"));
        break;
      case "tree":
        collection = new NativeORTree(this.#document, () => this.#flushEvents(), name, this.limits, internalTarget(name, "tree-nodes"), internalTarget(name, "tree-tombstones"));
        break;
    }
    for (const target of collection.targets) {
      const previous = this.#targets.get(target);
      if (previous !== undefined) {
        throw stateConflict();
      }
      this.#targets.set(target, collection);
    }
    this.#collections.set(name, collection);
    return collection;
  }

  #validate(update: NativeUpdate): Map<CollectionBinding, NativeMapSet[]> {
    const grouped = new Map<CollectionBinding, NativeMapSet[]>();
    for (const raw of update.operations) {
      if (raw.kind !== "map-set") {
        throw invalidUpdate();
      }
      const operation = raw as NativeMapSet;
      const collection = this.#targets.get(operation.target);
      if (collection === undefined) {
        throw invalidUpdate();
      }
      const operations = grouped.get(collection) ?? [];
      operations.push(operation);
      grouped.set(collection, operations);
    }
    for (const [collection, operations] of grouped) {
      collection.validate(operations);
    }
    return grouped;
  }

  #flushEvents(): void {
    if (this.#pendingEvents.length === 0) {
      return;
    }
    const events = this.#pendingEvents.splice(0);
    for (const event of events) {
      for (const listener of [...this.#updateListeners]) {
        listener(event);
      }
    }
  }
}

function resolveCollectionLimits(
  requested: NativeCollectionsDocumentOptions["collections"],
  documentLimits: Readonly<NativeDocumentLimits>,
): Readonly<NativeCollectionLimits> {
  const resolved: NativeCollectionLimits = { ...DEFAULT_NATIVE_COLLECTION_LIMITS, ...requested };
  for (const value of Object.values(resolved)) {
    if (!Number.isSafeInteger(value) || value <= 0) {
      throw resourceLimit();
    }
  }
  if (
    resolved.maxCounterComponents > documentLimits.maxMapEntries ||
    resolved.maxSetEntries > documentLimits.maxMapEntries ||
    resolved.maxSetTombstones > documentLimits.maxMapEntries ||
    resolved.maxTreeNodes > documentLimits.maxMapEntries ||
    resolved.maxTreeTombstones > documentLimits.maxMapEntries
  ) {
    throw resourceLimit();
  }
  return Object.freeze(resolved);
}

function readCounterComponent(value: NativeValue, limits: Readonly<NativeDocumentLimits>, collectionLimits: Readonly<NativeCollectionLimits>): CounterComponent {
  const record = requireRecord(value);
  assertKeys(record, ["$crdt", "actor", "id", "negative", "positive", "type"]);
  if (record.$crdt !== COLLECTION_MARKER || record.type !== "counter-component" || typeof record.actor !== "string") {
    throw invalidUpdate();
  }
  const id = readID(record.id, limits);
  if (id.actor !== record.actor || !isDecimal(record.positive, collectionLimits.maxCounterDigits) || !isDecimal(record.negative, collectionLimits.maxCounterDigits)) {
    throw invalidUpdate();
  }
  return { $crdt: COLLECTION_MARKER, type: "counter-component", actor: record.actor, id, positive: record.positive, negative: record.negative };
}

function readSetAdd(value: NativeValue, limits: Readonly<NativeDocumentLimits>): SetAdd {
  const record = requireRecord(value);
  assertKeys(record, ["$crdt", "id", "type", "value"]);
  if (record.$crdt !== COLLECTION_MARKER || record.type !== "orset-add" || !("value" in record)) {
    throw invalidUpdate();
  }
  return { $crdt: COLLECTION_MARKER, type: "orset-add", id: readID(record.id, limits), value: record.value as NativeValue };
}

function readSetTombstone(value: NativeValue, limits: Readonly<NativeDocumentLimits>): SetTombstone {
  const record = requireRecord(value);
  assertKeys(record, ["$crdt", "id", "removedBy", "type", "value"]);
  if (record.$crdt !== COLLECTION_MARKER || record.type !== "orset-tombstone" || !("value" in record)) {
    throw invalidUpdate();
  }
  return {
    $crdt: COLLECTION_MARKER,
    type: "orset-tombstone",
    id: readID(record.id, limits),
    removedBy: readID(record.removedBy, limits),
    value: record.value as NativeValue,
  };
}

function readLWWValue(value: NativeValue, limits: Readonly<NativeDocumentLimits>): LWWValue {
  const record = requireRecord(value);
  if (record.$crdt !== COLLECTION_MARKER || record.type !== "lww-register" || typeof record.deleted !== "boolean") {
    throw invalidUpdate();
  }
  if (record.deleted) {
    assertKeys(record, ["$crdt", "deleted", "id", "type"]);
    return { $crdt: COLLECTION_MARKER, type: "lww-register", id: readID(record.id, limits), deleted: true };
  }
  assertKeys(record, ["$crdt", "deleted", "id", "type", "value"]);
  if (!("value" in record)) {
    throw invalidUpdate();
  }
  return { $crdt: COLLECTION_MARKER, type: "lww-register", id: readID(record.id, limits), deleted: false, value: record.value as NativeValue };
}

function readTreeAdd(value: NativeValue, limits: Readonly<NativeDocumentLimits>): TreeAdd {
  const record = requireRecord(value);
  assertKeys(record, ["$crdt", "id", "parent", "type", "value"]);
  if (record.$crdt !== COLLECTION_MARKER || record.type !== "ortree-node" || !("value" in record)) {
    throw invalidUpdate();
  }
  return {
    $crdt: COLLECTION_MARKER,
    type: "ortree-node",
    id: readID(record.id, limits),
    parent: record.parent === null ? null : readID(record.parent, limits),
    value: record.value as NativeValue,
  };
}

function readTreeTombstone(value: NativeValue, limits: Readonly<NativeDocumentLimits>): TreeTombstone {
  const record = requireRecord(value);
  assertKeys(record, ["$crdt", "id", "removedBy", "type"]);
  if (record.$crdt !== COLLECTION_MARKER || record.type !== "ortree-tombstone") {
    throw invalidUpdate();
  }
  return { $crdt: COLLECTION_MARKER, type: "ortree-tombstone", id: readID(record.id, limits), removedBy: readID(record.removedBy, limits) };
}

function readID(value: unknown, limits: Readonly<NativeDocumentLimits>): NativeID {
  const record = requireRecord(value);
  assertKeys(record, ["actor", "counter"]);
  const actor = record.actor;
  const counter = record.counter;
  if (typeof actor !== "string" || typeof counter !== "number" || !Number.isSafeInteger(counter) || counter <= 0 || utf8Length(actor) > limits.maxReplicaIDBytes) {
    throw invalidUpdate();
  }
  return { actor, counter };
}

function assertCollectionName(name: string, document: NativeDocument): void {
  document._assertRootName(name);
  document._assertRootName(internalTarget(name, "counter"));
}

function internalTarget(name: string, suffix: string): string {
  const encoded = [...TEXT_ENCODER.encode(name)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
  return `${COLLECTION_NAMESPACE}${encoded}/${suffix}`;
}

function actorKey(actor: string): string {
  return `actor:${[...TEXT_ENCODER.encode(actor)].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function idKey(id: NativeID): string {
  return `${actorKey(id.actor)}:${id.counter}`;
}

function boundedDecimal(value: bigint, limits: Readonly<NativeCollectionLimits>): string {
  const encoded = value.toString(10);
  if (encoded.length > limits.maxCounterDigits) {
    throw resourceLimit();
  }
  return encoded;
}

function isDecimal(value: unknown, maximumDigits: number): value is string {
  return typeof value === "string" && value.length > 0 && value.length <= maximumDigits && /^(0|[1-9][0-9]*)$/.test(value);
}

function canonicalValue(value: unknown): string {
  if (value === null || typeof value === "boolean" || typeof value === "string") {
    return JSON.stringify(value);
  }
  if (typeof value === "number") {
    if (!Number.isFinite(value) || Object.is(value, -0)) {
      throw invalidUpdate();
    }
    return JSON.stringify(value);
  }
  if (Array.isArray(value)) {
    return `[${value.map((item) => canonicalValue(item)).join(",")}]`;
  }
  if (!isRecord(value)) {
    throw invalidUpdate();
  }
  return `{${Object.keys(value).sort(compareText).map((key) => `${JSON.stringify(key)}:${canonicalValue(value[key])}`).join(",")}}`;
}

function requireTarget(operation: NativeMapSet, target: string): void {
  if (operation.target !== target) {
    throw invalidUpdate();
  }
}

function isNativeSnapshot(value: unknown): value is NativeSnapshot {
  return isRecord(value) && Array.isArray(value.roots) && Array.isArray(value.updates) && typeof value.counter === "number" && Number.isSafeInteger(value.counter) && value.counter >= 0;
}

function isCollectionType(value: unknown): value is NativeCollectionType {
  return value === "counter" || value === "orset" || value === "lww" || value === "tree";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function requireRecord(value: unknown): Record<string, unknown> {
  if (!isRecord(value)) {
    throw invalidUpdate();
  }
  return value;
}

function assertKeys(value: Record<string, unknown>, allowed: readonly string[]): void {
  const keys = Object.keys(value).sort(compareText);
  if (keys.length !== allowed.length || keys.some((key, index) => key !== allowed[index])) {
    throw invalidUpdate();
  }
}

function compareID(left: NativeID, right: NativeID): number {
  if (left.counter !== right.counter) {
    return left.counter < right.counter ? -1 : 1;
  }
  return compareText(left.actor, right.actor);
}

function sameID(left: NativeID, right: NativeID): boolean {
  return left.counter === right.counter && left.actor === right.actor;
}

function copyID(value: NativeID): NativeID {
  return { actor: value.actor, counter: value.counter };
}

function compareText(left: string, right: string): number {
  const leftBytes = TEXT_ENCODER.encode(left);
  const rightBytes = TEXT_ENCODER.encode(right);
  const length = Math.min(leftBytes.length, rightBytes.length);
  for (let index = 0; index < length; index += 1) {
    const difference = leftBytes[index]! - rightBytes[index]!;
    if (difference !== 0) {
      return difference;
    }
  }
  return leftBytes.length - rightBytes.length;
}

function utf8Length(value: string): number {
  return TEXT_ENCODER.encode(value).byteLength;
}

function invalidUpdate(): NativeCRDTError {
  return new NativeCRDTError("invalid_update");
}

function stateConflict(): NativeCRDTError {
  return new NativeCRDTError("state_conflict");
}

function typeConflict(): NativeCRDTError {
  return new NativeCRDTError("type_conflict");
}

function resourceLimit(): NativeCRDTError {
  return new NativeCRDTError("resource_limit");
}
