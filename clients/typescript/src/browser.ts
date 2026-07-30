import {
  encodeNativeUpdate,
  NativeCRDTError,
  NativeDocument,
} from "./native.js";
import type {
  NativeArray,
  NativeDocumentOptions,
  NativeMap,
  NativePersistenceMetadata,
  NativeRoot,
  NativeSnapshot,
  NativeTypeListener,
  NativeUpdate,
  NativeUpdateEvent,
  NativeUpdateListener,
  NativeValue,
} from "./native.js";

const BROWSER_PERSISTENCE_FORMAT = 1;
const DEFAULT_DATABASE_NAME = "darkinno-crdt-native";
const UPDATE_STORE = "updates";
const DOCUMENT_STORE = "documents";
const MAX_STORAGE_KEY_BYTES = 1 << 10;

/** Limits for the append-only browser recovery log. */
export interface BrowserPersistenceLimits {
  /** Compact a fully resolved document after this many retained updates. */
  readonly compactAfterUpdates: number;
  /** Compact a fully resolved document after this many retained update bytes. */
  readonly compactAfterBytes: number;
  /** Reject further persistence once the retained log reaches this update count. */
  readonly maxUpdates: number;
  /** Reject further persistence once the retained log reaches this byte budget. */
  readonly maxBytes: number;
}

/** Conservative browser defaults; deployment policy may choose lower limits. */
export const DEFAULT_BROWSER_PERSISTENCE_LIMITS: Readonly<BrowserPersistenceLimits> = Object.freeze({
  compactAfterUpdates: 128,
  compactAfterBytes: 1 << 20,
  maxUpdates: 10_000,
  maxBytes: 32 << 20,
});

export type NativeBrowserErrorCode =
  | "document_closed"
  | "invalid_argument"
  | "persistence_failed"
  | "persistence_limit"
  | "persistence_unavailable"
  | "transport_failed";

/** A browser-runtime boundary error; CRDT validation errors retain their native type. */
export class NativeBrowserError extends Error {
  readonly code: NativeBrowserErrorCode;

  constructor(code: NativeBrowserErrorCode, cause?: unknown) {
    super(code, cause === undefined ? undefined : { cause });
    this.name = "NativeBrowserError";
    this.code = code;
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/** One persisted canonical update. `pending` is a caller-defined transport receipt boundary. */
export interface BrowserPersistedUpdate {
  readonly sequence: number;
  readonly encoded: Uint8Array;
  readonly local: boolean;
  readonly pending: boolean;
}

/**
 * The durable record for one browser document. Stores must update metadata and
 * an appended update in one transaction; otherwise a restarted actor can
 * reuse its counter or recover a partial local mutation.
 */
export interface BrowserStoredDocument {
  readonly metadata: NativePersistenceMetadata;
  readonly baseUpdates: readonly NativeUpdate[];
  readonly updates: readonly BrowserPersistedUpdate[];
  readonly nextSequence: number;
  readonly logBytes: number;
}

export interface NativeBrowserPersistence {
  load(key: string): Promise<BrowserStoredDocument | undefined>;
  writeMetadata(key: string, metadata: NativePersistenceMetadata): Promise<void>;
  append(
    key: string,
    metadata: NativePersistenceMetadata,
    encoded: Uint8Array,
    local: boolean,
    limits: Readonly<BrowserPersistenceLimits>,
  ): Promise<{ readonly sequence: number; readonly updateCount: number; readonly logBytes: number }>;
  acknowledge(key: string, sequence: number): Promise<void>;
  compact(key: string, snapshot: NativeSnapshot): Promise<void>;
  close?(): void | Promise<void>;
}

/**
 * A transport must route one authenticated, document-bound native-ts-v1
 * update. Resolve `send` only at the application's chosen receipt boundary;
 * a WebSocket write alone is not a durable remote acknowledgement.
 */
export interface NativeBrowserTransport {
  send(encoded: Uint8Array): void | Promise<void>;
  subscribe(receiver: (encoded: Uint8Array) => void): () => void;
}

export interface NativeBrowserUpdateEvent extends NativeUpdateEvent {
  /** A defensive, canonical transport representation of `update`. */
  readonly encoded: Uint8Array;
}

export interface BrowserNativeDocumentOptions {
  /** Persistence and transport identity; bind it to one authenticated group. */
  readonly documentID: string;
  /** Each concurrently active browser context needs its own immutable actor ID. */
  readonly replicaID: string;
  readonly documentOptions?: NativeDocumentOptions;
  readonly persistence?: NativeBrowserPersistence;
  /** Used only when `persistence` is omitted. */
  readonly databaseName?: string;
  readonly persistenceLimits?: Partial<BrowserPersistenceLimits>;
  /** Optional already-authenticated transport for automatic outbox draining. */
  readonly transport?: NativeBrowserTransport;
}

/**
 * Opens a complete browser-native document: restore → local merge → atomic
 * IndexedDB append/outbox → application-authenticated transport. It never
 * assumes a server URL, handshake, credentials, or acknowledgement format.
 */
export async function openNativeBrowserDocument(options: BrowserNativeDocumentOptions): Promise<NativeBrowserDocument> {
  assertStorageKey(options.documentID);
  const limits = resolvePersistenceLimits(options.persistenceLimits);
  const ownsPersistence = options.persistence === undefined;
  const persistence = options.persistence ?? (await IndexedDBNativePersistence.open(options.databaseName));
  const stored = await persistence.load(options.documentID);
  const document = restoreDocument(options.replicaID, stored, options.documentOptions);
  const client = NativeBrowserDocument.fromRestored(
    options.documentID,
    document,
    persistence,
    limits,
    ownsPersistence,
    stored,
  );
  if (options.transport !== undefined) {
    await client.connect(options.transport);
  }
  return client;
}

/**
 * Browser-facing native-ts-v1 facade. It intentionally exposes CRDT-shaped
 * methods, while keeping the mutable NativeDocument inaccessible so all local
 * and received updates enter the persistence/outbox boundary.
 */
export class NativeBrowserDocument {
  readonly #updateListeners = new Set<(event: NativeBrowserUpdateEvent) => void>();
  readonly #errorListeners = new Set<(error: unknown) => void>();
  readonly #outbox = new Map<number, BrowserPersistedUpdate>();
  readonly #unsubscribeDocument: () => void;
  #unsubscribeTransport: (() => void) | undefined;
  #transport: NativeBrowserTransport | undefined;
  #persistenceQueue: Promise<void> = Promise.resolve();
  #drainPromise: Promise<void> | undefined;
  #reportedPersistenceError: unknown;
  #closed = false;

  private constructor(
    readonly documentID: string,
    private readonly document: NativeDocument,
    private readonly persistence: NativeBrowserPersistence,
    private readonly persistenceLimits: Readonly<BrowserPersistenceLimits>,
    private readonly ownsPersistence: boolean,
  ) {
    this.#unsubscribeDocument = this.document.onUpdate((event) => this.recordUpdate(event));
  }

  static fromRestored(
    documentID: string,
    document: NativeDocument,
    persistence: NativeBrowserPersistence,
    limits: Readonly<BrowserPersistenceLimits>,
    ownsPersistence: boolean,
    stored: BrowserStoredDocument | undefined,
  ): NativeBrowserDocument {
    const client = new NativeBrowserDocument(documentID, document, persistence, limits, ownsPersistence);
    if (stored !== undefined) {
      for (const update of stored.updates) {
        if (update.local && update.pending) {
          client.#outbox.set(update.sequence, copyPersistedUpdate(update));
        }
      }
    }
    return client;
  }

  /** A count of local updates retained until the configured transport resolves. */
  get pendingOutbox(): number {
    return this.#outbox.size;
  }

  getMap<T extends NativeValue = NativeValue>(name: string): NativeMap<T> {
    this.assertOpen();
    const map = this.document.getMap<T>(name);
    this.queueMetadata();
    return map;
  }

  getArray<T extends NativeValue = NativeValue>(name: string): NativeArray<T> {
    this.assertOpen();
    const array = this.document.getArray<T>(name);
    this.queueMetadata();
    return array;
  }

  transact<T>(callback: () => T, origin?: unknown): T {
    this.assertOpen();
    return this.document.transact(callback, origin);
  }

  applyUpdate(update: NativeUpdate, origin?: unknown): boolean {
    this.assertOpen();
    return this.document.applyUpdate(update, origin);
  }

  applyEncodedUpdate(encoded: Uint8Array, origin?: unknown): boolean {
    this.assertOpen();
    return this.document.applyEncodedUpdate(encoded, origin);
  }

  onUpdate(listener: (event: NativeBrowserUpdateEvent) => void): () => void {
    this.assertOpen();
    this.#updateListeners.add(listener);
    return () => this.#updateListeners.delete(listener);
  }

  onError(listener: (error: unknown) => void): () => void {
    this.assertOpen();
    this.#errorListeners.add(listener);
    return () => this.#errorListeners.delete(listener);
  }

  /**
   * Attaches a transport and retries the restored outbox in sequence. The
   * transport itself owns authentication, authorization, replay policy, and
   * what constitutes a durable receipt.
   */
  async connect(transport: NativeBrowserTransport): Promise<void> {
    this.assertOpen();
    assertTransport(transport);
    this.disconnect();
    this.#transport = transport;
    this.#unsubscribeTransport = transport.subscribe((encoded) => {
      try {
        this.applyEncodedUpdate(encoded, "transport");
      } catch (error) {
        this.report(error);
      }
    });
    await this.flush();
  }

  disconnect(): void {
    this.#unsubscribeTransport?.();
    this.#unsubscribeTransport = undefined;
    this.#transport = undefined;
  }

  /**
   * Waits for IndexedDB commits and, when connected, drains the outbox. A
   * resolved call proves this browser accepted the recovery record; browsers
   * still cannot guarantee a transaction completes during process shutdown.
   */
  async flush(): Promise<void> {
    this.assertOpen();
    await this.#persistenceQueue;
    if (this.#reportedPersistenceError !== undefined) {
      throw this.#reportedPersistenceError;
    }
    await this.drainOutbox();
  }

  /** Flushes recovery state, releases listeners, and closes owned IndexedDB handles. */
  async close(): Promise<boolean> {
    if (this.#closed) {
      return false;
    }
    await this.flush();
    this.#closed = true;
    this.disconnect();
    this.#unsubscribeDocument();
    this.document.close();
    this.#updateListeners.clear();
    this.#errorListeners.clear();
    if (this.ownsPersistence) {
      await this.persistence.close?.();
    }
    return true;
  }

  private recordUpdate(event: NativeUpdateEvent): void {
    const encoded = encodeNativeUpdate(event.update, this.document.limits);
    const emitted: NativeBrowserUpdateEvent = { ...event, encoded: encoded.slice() };
    for (const listener of [...this.#updateListeners]) {
      listener(emitted);
    }
    void this.enqueue(async () => {
      const result = await this.persistence.append(
        this.documentID,
        this.document.persistenceMetadata(),
        encoded,
        event.local === true,
        this.persistenceLimits,
      );
      if (event.local === true) {
        this.#outbox.set(result.sequence, {
          sequence: result.sequence,
          encoded: encoded.slice(),
          local: true,
          pending: true,
        });
      }
      await this.maybeCompact(result.updateCount, result.logBytes);
    }).then(
      () => {
        if (event.local === true) {
          void this.drainOutbox().catch((error) => this.report(error));
        }
      },
      (error) => this.report(error),
    );
  }

  private queueMetadata(): void {
    void this.enqueue(() => this.persistence.writeMetadata(this.documentID, this.document.persistenceMetadata())).catch((error) =>
      this.report(error),
    );
  }

  private enqueue(operation: () => Promise<void>): Promise<void> {
    const next = this.#persistenceQueue.then(operation);
    this.#persistenceQueue = next.catch((error) => {
      this.#reportedPersistenceError ??= error;
    });
    return next;
  }

  private async maybeCompact(updateCount: number, logBytes: number): Promise<void> {
    if (
      this.#outbox.size !== 0 ||
      (updateCount < this.persistenceLimits.compactAfterUpdates && logBytes < this.persistenceLimits.compactAfterBytes)
    ) {
      return;
    }
    try {
      await this.persistence.compact(this.documentID, this.document.snapshot());
    } catch (error) {
      // An unresolved array cannot safely encode complete state. Its append
      // log remains recoverable, and a later resolved update can compact it.
      if (!(error instanceof NativeCRDTError) || error.code !== "incomplete_state") {
        throw error;
      }
    }
  }

  private async drainOutbox(): Promise<void> {
    if (this.#transport === undefined || this.#outbox.size === 0) {
      return;
    }
    if (this.#drainPromise !== undefined) {
      return this.#drainPromise;
    }
    const draining = this.runDrain();
    this.#drainPromise = draining.finally(() => {
      this.#drainPromise = undefined;
    });
    return this.#drainPromise;
  }

  private async runDrain(): Promise<void> {
    const transport = this.#transport;
    if (transport === undefined) {
      return;
    }
    for (const update of [...this.#outbox.values()].sort((left, right) => left.sequence - right.sequence)) {
      try {
        await transport.send(update.encoded.slice());
      } catch (error) {
        throw new NativeBrowserError("transport_failed", error);
      }
      await this.enqueue(async () => {
        await this.persistence.acknowledge(this.documentID, update.sequence);
        this.#outbox.delete(update.sequence);
      });
    }
    const stored = await this.persistence.load(this.documentID);
    if (stored !== undefined) {
      await this.enqueue(() => this.maybeCompact(stored.updates.length, stored.logBytes));
    }
  }

  private report(error: unknown): void {
    for (const listener of [...this.#errorListeners]) {
      try {
        listener(error);
      } catch {
        // User error observers must not prevent a bounded recovery write.
      }
    }
  }

  private assertOpen(): void {
    if (this.#closed) {
      throw new NativeBrowserError("document_closed");
    }
  }
}

/** A deterministic in-memory persistence implementation for tests and SSR adapters. */
export class MemoryNativeBrowserPersistence implements NativeBrowserPersistence {
  readonly #documents = new Map<string, BrowserStoredDocument>();

  async load(key: string): Promise<BrowserStoredDocument | undefined> {
    const record = this.#documents.get(key);
    return record === undefined ? undefined : copyStoredDocument(record);
  }

  async writeMetadata(key: string, metadata: NativePersistenceMetadata): Promise<void> {
    const record = this.#documents.get(key) ?? emptyStoredDocument(metadata);
    this.#documents.set(key, { ...record, metadata: copyMetadata(metadata) });
  }

  async append(
    key: string,
    metadata: NativePersistenceMetadata,
    encoded: Uint8Array,
    local: boolean,
    limits: Readonly<BrowserPersistenceLimits>,
  ): Promise<{ readonly sequence: number; readonly updateCount: number; readonly logBytes: number }> {
    const record = this.#documents.get(key) ?? emptyStoredDocument(metadata);
    const nextBytes = record.logBytes + encoded.byteLength;
    if (record.updates.length >= limits.maxUpdates || nextBytes > limits.maxBytes) {
      throw new NativeBrowserError("persistence_limit");
    }
    const sequence = record.nextSequence + 1;
    const update: BrowserPersistedUpdate = { sequence, encoded: encoded.slice(), local, pending: local };
    const next: BrowserStoredDocument = {
      metadata: copyMetadata(metadata),
      baseUpdates: record.baseUpdates.map(copyNativeUpdate),
      updates: [...record.updates.map(copyPersistedUpdate), update],
      nextSequence: sequence,
      logBytes: nextBytes,
    };
    this.#documents.set(key, next);
    return { sequence, updateCount: next.updates.length, logBytes: next.logBytes };
  }

  async acknowledge(key: string, sequence: number): Promise<void> {
    const record = this.#documents.get(key);
    if (record === undefined) {
      return;
    }
    this.#documents.set(key, {
      ...record,
      updates: record.updates.map((update) =>
        update.sequence === sequence ? { ...copyPersistedUpdate(update), pending: false } : copyPersistedUpdate(update),
      ),
    });
  }

  async compact(key: string, snapshot: NativeSnapshot): Promise<void> {
    const record = this.#documents.get(key);
    if (record?.updates.some((update) => update.local && update.pending)) {
      throw new NativeBrowserError("persistence_failed");
    }
    this.#documents.set(key, {
      metadata: { roots: snapshot.roots.map(copyRoot), counter: snapshot.counter },
      baseUpdates: snapshot.updates.map(copyNativeUpdate),
      updates: [],
      nextSequence: 0,
      logBytes: 0,
    });
  }
}

/** IndexedDB-backed append log; metadata and a newly appended update share one transaction. */
export class IndexedDBNativePersistence implements NativeBrowserPersistence {
  readonly #database: IDBDatabase;

  private constructor(database: IDBDatabase) {
    this.#database = database;
  }

  static async open(databaseName = DEFAULT_DATABASE_NAME): Promise<IndexedDBNativePersistence> {
    if (typeof indexedDB === "undefined") {
      throw new NativeBrowserError("persistence_unavailable");
    }
    if (typeof databaseName !== "string" || databaseName.length === 0) {
      throw new NativeBrowserError("invalid_argument");
    }
    try {
      const request = indexedDB.open(databaseName, BROWSER_PERSISTENCE_FORMAT);
      request.onupgradeneeded = () => {
        const database = request.result;
        if (!database.objectStoreNames.contains(DOCUMENT_STORE)) {
          database.createObjectStore(DOCUMENT_STORE, { keyPath: "key" });
        }
        if (!database.objectStoreNames.contains(UPDATE_STORE)) {
          database.createObjectStore(UPDATE_STORE, { keyPath: ["key", "sequence"] });
        }
      };
      return new IndexedDBNativePersistence(await requestValue(request));
    } catch (error) {
      throw new NativeBrowserError("persistence_failed", error);
    }
  }

  async load(key: string): Promise<BrowserStoredDocument | undefined> {
    try {
      const transaction = this.#database.transaction([DOCUMENT_STORE, UPDATE_STORE], "readonly");
      const documentRequest = transaction.objectStore(DOCUMENT_STORE).get(key);
      const updatesRequest = readUpdates(transaction.objectStore(UPDATE_STORE), key);
      const [record, updates] = await Promise.all([requestValue(documentRequest), updatesRequest]);
      await transactionComplete(transaction);
      if (record === undefined) {
        return undefined;
      }
      return fromIndexedRecord(record, updates);
    } catch (error) {
      throw asPersistenceError(error);
    }
  }

  async writeMetadata(key: string, metadata: NativePersistenceMetadata): Promise<void> {
    try {
      const transaction = this.#database.transaction(DOCUMENT_STORE, "readwrite");
      const store = transaction.objectStore(DOCUMENT_STORE);
      const existing = await requestValue(store.get(key));
      const record = existing === undefined ? indexedEmptyRecord(key, metadata) : requireIndexedRecord(existing);
      store.put({ ...record, metadata: copyMetadata(metadata) });
      await transactionComplete(transaction);
    } catch (error) {
      throw asPersistenceError(error);
    }
  }

  async append(
    key: string,
    metadata: NativePersistenceMetadata,
    encoded: Uint8Array,
    local: boolean,
    limits: Readonly<BrowserPersistenceLimits>,
  ): Promise<{ readonly sequence: number; readonly updateCount: number; readonly logBytes: number }> {
    try {
      const transaction = this.#database.transaction([DOCUMENT_STORE, UPDATE_STORE], "readwrite");
      const documentStore = transaction.objectStore(DOCUMENT_STORE);
      const existing = await requestValue(documentStore.get(key));
      const record = existing === undefined ? indexedEmptyRecord(key, metadata) : requireIndexedRecord(existing);
      const nextBytes = record.logBytes + encoded.byteLength;
      if (record.updateCount >= limits.maxUpdates || nextBytes > limits.maxBytes || record.nextSequence >= Number.MAX_SAFE_INTEGER) {
        transaction.abort();
        throw new NativeBrowserError("persistence_limit");
      }
      const sequence = record.nextSequence + 1;
      transaction.objectStore(UPDATE_STORE).add({ key, sequence, encoded: encoded.slice(), local, pending: local });
      documentStore.put({
        ...record,
        metadata: copyMetadata(metadata),
        nextSequence: sequence,
        updateCount: record.updateCount + 1,
        logBytes: nextBytes,
      });
      await transactionComplete(transaction);
      return { sequence, updateCount: record.updateCount + 1, logBytes: nextBytes };
    } catch (error) {
      throw asPersistenceError(error);
    }
  }

  async acknowledge(key: string, sequence: number): Promise<void> {
    try {
      const transaction = this.#database.transaction(UPDATE_STORE, "readwrite");
      const store = transaction.objectStore(UPDATE_STORE);
      const record = await requestValue(store.get([key, sequence]));
      if (record !== undefined) {
        const update = requireIndexedUpdate(record);
        store.put({ ...update, pending: false });
      }
      await transactionComplete(transaction);
    } catch (error) {
      throw asPersistenceError(error);
    }
  }

  async compact(key: string, snapshot: NativeSnapshot): Promise<void> {
    try {
      const transaction = this.#database.transaction([DOCUMENT_STORE, UPDATE_STORE], "readwrite");
      const updateStore = transaction.objectStore(UPDATE_STORE);
      const updates = await readUpdates(updateStore, key);
      if (updates.some((update) => update.local && update.pending)) {
        transaction.abort();
        throw new NativeBrowserError("persistence_failed");
      }
      for (const update of updates) {
        updateStore.delete([key, update.sequence]);
      }
      transaction.objectStore(DOCUMENT_STORE).put({
        key,
        format: BROWSER_PERSISTENCE_FORMAT,
        metadata: { roots: snapshot.roots.map(copyRoot), counter: snapshot.counter },
        baseUpdates: snapshot.updates.map(copyNativeUpdate),
        nextSequence: 0,
        updateCount: 0,
        logBytes: 0,
      } satisfies IndexedDocumentRecord);
      await transactionComplete(transaction);
    } catch (error) {
      throw asPersistenceError(error);
    }
  }

  close(): void {
    this.#database.close();
  }
}

/** A same-origin, volatile multi-tab transport; it is not authentication or durable delivery. */
export class BroadcastChannelNativeTransport implements NativeBrowserTransport {
  readonly #channel: BroadcastChannel;
  readonly #source = createBrowserReplicaID("tab");

  constructor(channelName: string) {
    if (typeof BroadcastChannel === "undefined") {
      throw new NativeBrowserError("persistence_unavailable");
    }
    assertStorageKey(channelName);
    this.#channel = new BroadcastChannel(channelName);
  }

  send(encoded: Uint8Array): void {
    this.#channel.postMessage({ version: 1, source: this.#source, encoded: encoded.slice() });
  }

  subscribe(receiver: (encoded: Uint8Array) => void): () => void {
    const handler = (event: MessageEvent<unknown>): void => {
      const value = event.data;
      if (!isRecord(value) || value.version !== 1 || value.source === this.#source || !(value.encoded instanceof Uint8Array)) {
        return;
      }
      receiver(value.encoded.slice());
    };
    this.#channel.addEventListener("message", handler);
    return () => this.#channel.removeEventListener("message", handler);
  }

  close(): void {
    this.#channel.close();
  }
}

/** Produces a per-context, URL-safe actor ID without persisting a credential. */
export function createBrowserReplicaID(prefix = "browser"): string {
  if (typeof prefix !== "string" || prefix.length === 0 || !/^[A-Za-z0-9_-]+$/.test(prefix)) {
    throw new NativeBrowserError("invalid_argument");
  }
  if (globalThis.crypto?.getRandomValues === undefined) {
    throw new NativeBrowserError("persistence_unavailable");
  }
  const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
  let suffix = "";
  for (const value of bytes) {
    suffix += value.toString(16).padStart(2, "0");
  }
  return `${prefix}-${suffix}`;
}

interface IndexedDocumentRecord {
  readonly key: string;
  readonly format: number;
  readonly metadata: NativePersistenceMetadata;
  readonly baseUpdates: readonly NativeUpdate[];
  readonly nextSequence: number;
  readonly updateCount: number;
  readonly logBytes: number;
}

interface IndexedUpdateRecord {
  readonly key: string;
  readonly sequence: number;
  readonly encoded: Uint8Array;
  readonly local: boolean;
  readonly pending: boolean;
}

function restoreDocument(
  replicaID: string,
  stored: BrowserStoredDocument | undefined,
  options: NativeDocumentOptions | undefined,
): NativeDocument {
  if (stored === undefined) {
    return new NativeDocument(replicaID, options);
  }
  const document = NativeDocument.restore(
    replicaID,
    { roots: stored.metadata.roots, updates: stored.baseUpdates, counter: stored.metadata.counter },
    options,
  );
  for (const update of stored.updates) {
    document.applyEncodedUpdate(update.encoded, "persistence-replay");
  }
  return document;
}

function resolvePersistenceLimits(options: Partial<BrowserPersistenceLimits> | undefined): Readonly<BrowserPersistenceLimits> {
  const limits = { ...DEFAULT_BROWSER_PERSISTENCE_LIMITS, ...options };
  for (const value of Object.values(limits)) {
    if (!Number.isSafeInteger(value) || value <= 0) {
      throw new NativeBrowserError("invalid_argument");
    }
  }
  if (
    limits.compactAfterUpdates > limits.maxUpdates ||
    limits.compactAfterBytes > limits.maxBytes
  ) {
    throw new NativeBrowserError("invalid_argument");
  }
  return Object.freeze(limits);
}

function emptyStoredDocument(metadata: NativePersistenceMetadata): BrowserStoredDocument {
  return { metadata: copyMetadata(metadata), baseUpdates: [], updates: [], nextSequence: 0, logBytes: 0 };
}

function indexedEmptyRecord(key: string, metadata: NativePersistenceMetadata): IndexedDocumentRecord {
  return {
    key,
    format: BROWSER_PERSISTENCE_FORMAT,
    metadata: copyMetadata(metadata),
    baseUpdates: [],
    nextSequence: 0,
    updateCount: 0,
    logBytes: 0,
  };
}

function copyStoredDocument(document: BrowserStoredDocument): BrowserStoredDocument {
  return {
    metadata: copyMetadata(document.metadata),
    baseUpdates: document.baseUpdates.map(copyNativeUpdate),
    updates: document.updates.map(copyPersistedUpdate),
    nextSequence: document.nextSequence,
    logBytes: document.logBytes,
  };
}

function copyPersistedUpdate(update: BrowserPersistedUpdate): BrowserPersistedUpdate {
  return { ...update, encoded: update.encoded.slice() };
}

function copyMetadata(metadata: NativePersistenceMetadata): NativePersistenceMetadata {
  return { roots: metadata.roots.map(copyRoot), counter: metadata.counter };
}

function copyRoot(root: NativeRoot): NativeRoot {
  return { name: root.name, type: root.type };
}

function copyNativeUpdate(update: NativeUpdate): NativeUpdate {
  return JSON.parse(JSON.stringify(update)) as NativeUpdate;
}

function assertStorageKey(value: unknown): asserts value is string {
  if (typeof value !== "string" || value.trim().length === 0 || new TextEncoder().encode(value).byteLength > MAX_STORAGE_KEY_BYTES) {
    throw new NativeBrowserError("invalid_argument");
  }
}

function assertTransport(value: unknown): asserts value is NativeBrowserTransport {
  if (!isRecord(value) || typeof value.send !== "function" || typeof value.subscribe !== "function") {
    throw new NativeBrowserError("invalid_argument");
  }
}

function requestValue<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

function transactionComplete(transaction: IDBTransaction): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    transaction.oncomplete = () => resolve();
    transaction.onabort = () => reject(transaction.error ?? new Error("IndexedDB transaction aborted"));
    transaction.onerror = () => reject(transaction.error ?? new Error("IndexedDB transaction failed"));
  });
}

function readUpdates(store: IDBObjectStore, key: string): Promise<IndexedUpdateRecord[]> {
  return new Promise<IndexedUpdateRecord[]>((resolve, reject) => {
    const values: IndexedUpdateRecord[] = [];
    const request = store.openCursor(IDBKeyRange.bound([key, 0], [key, Number.MAX_SAFE_INTEGER]));
    request.onerror = () => reject(request.error);
    request.onsuccess = () => {
      const cursor = request.result;
      if (cursor === null) {
        resolve(values);
        return;
      }
      try {
        values.push(requireIndexedUpdate(cursor.value));
        cursor.continue();
      } catch (error) {
        reject(error);
      }
    };
  });
}

function fromIndexedRecord(value: unknown, updates: readonly IndexedUpdateRecord[]): BrowserStoredDocument {
  const record = requireIndexedRecord(value);
  if (record.updateCount !== updates.length || record.logBytes !== updates.reduce((total, update) => total + update.encoded.byteLength, 0)) {
    throw new NativeBrowserError("persistence_failed");
  }
  return {
    metadata: copyMetadata(record.metadata),
    baseUpdates: record.baseUpdates.map(copyNativeUpdate),
    updates: updates.map((update) => ({ ...update, encoded: update.encoded.slice() })),
    nextSequence: record.nextSequence,
    logBytes: record.logBytes,
  };
}

function requireIndexedRecord(value: unknown): IndexedDocumentRecord {
  if (!isRecord(value) || value.format !== BROWSER_PERSISTENCE_FORMAT || typeof value.key !== "string" || !isRecord(value.metadata)) {
    throw new NativeBrowserError("persistence_failed");
  }
  const metadata = value.metadata;
  const counter = metadata.counter;
  const nextSequence = value.nextSequence;
  const updateCount = value.updateCount;
  const logBytes = value.logBytes;
  if (
    !Array.isArray(metadata.roots) ||
    typeof counter !== "number" ||
    !Number.isSafeInteger(counter) ||
    counter < 0 ||
    !Array.isArray(value.baseUpdates)
  ) {
    throw new NativeBrowserError("persistence_failed");
  }
  if (
    typeof nextSequence !== "number" ||
    !Number.isSafeInteger(nextSequence) ||
    nextSequence < 0 ||
    typeof updateCount !== "number" ||
    !Number.isSafeInteger(updateCount) ||
    updateCount < 0 ||
    typeof logBytes !== "number" ||
    !Number.isSafeInteger(logBytes) ||
    logBytes < 0
  ) {
    throw new NativeBrowserError("persistence_failed");
  }
  return value as unknown as IndexedDocumentRecord;
}

function requireIndexedUpdate(value: unknown): IndexedUpdateRecord {
  const sequence = isRecord(value) ? value.sequence : undefined;
  if (
    !isRecord(value) ||
    typeof value.key !== "string" ||
    typeof sequence !== "number" ||
    !Number.isSafeInteger(sequence) ||
    sequence <= 0 ||
    !(value.encoded instanceof Uint8Array) ||
    typeof value.local !== "boolean" ||
    typeof value.pending !== "boolean"
  ) {
    throw new NativeBrowserError("persistence_failed");
  }
  return value as unknown as IndexedUpdateRecord;
}

function asPersistenceError(error: unknown): NativeBrowserError {
  return error instanceof NativeBrowserError ? error : new NativeBrowserError("persistence_failed", error);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}
