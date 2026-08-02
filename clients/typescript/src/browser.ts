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
  NativeStateVector,
  NativeTypeListener,
  NativeUpdate,
  NativeUpdateEvent,
  NativeUpdateListener,
  NativeValue,
} from "./native.js";
import { CRDTRuntimeError } from "./wasm.js";
import type { RGAAnchor, RGAWasmDocument, RGAWasmRuntime, RGASnapshot } from "./wasm.js";

const DATABASE_FORMAT = 2;
const BROWSER_PERSISTENCE_FORMAT = 1;
const RGA_BROWSER_PERSISTENCE_FORMAT = 1;
const DEFAULT_DATABASE_NAME = "darkinno-crdt-native";
const UPDATE_STORE = "updates";
const DOCUMENT_STORE = "documents";
const RGA_UPDATE_STORE = "rga-updates";
const RGA_DOCUMENT_STORE = "rga-documents";
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

/**
 * An optional low-latency path beside a durable `NativeBrowserTransport`.
 * Publishing here never acknowledges the persistent outbox: implementations
 * may be volatile, unauthenticated, unordered, or partitioned. Received data
 * still enters the normal bounded decoder before it changes document state.
 */
export interface NativeBrowserLiveTransport {
  publish(encoded: Uint8Array): void;
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
  /** Optional best-effort local live path. It never clears the durable outbox. */
  readonly liveTransport?: NativeBrowserLiveTransport;
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
  if (options.liveTransport !== undefined) {
    client.connectLive(options.liveTransport);
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
  #unsubscribeLiveTransport: (() => void) | undefined;
  #transport: NativeBrowserTransport | undefined;
  #liveTransport: NativeBrowserLiveTransport | undefined;
  #persistenceQueue: Promise<void> = Promise.resolve();
  #drainPromise: Promise<void> | undefined;
  #reportedPersistenceError: unknown;
  #mutationVersion = 0;
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

  /** Returns the persisted native-ts-v1 sparse state vector for anti-entropy. */
  getStateVector(): NativeStateVector {
    this.assertOpen();
    return this.document.getStateVector();
  }

  /**
   * Builds bounded state updates that the authenticated peer vector lacks.
   * This does not clear the durable outbox or constitute a remote receipt.
   */
  encodeStateAsUpdates(peerVector?: NativeStateVector): NativeUpdate[] {
    this.assertOpen();
    return this.document.encodeStateAsUpdates(peerVector);
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
   * Attaches a best-effort live path without changing the outbox receipt
   * boundary. Use this for same-origin multi-tab delivery only alongside the
   * application's authenticated replay/bootstrap path.
   */
  connectLive(transport: NativeBrowserLiveTransport): void {
    this.assertOpen();
    assertLiveTransport(transport);
    this.disconnectLive();
    this.#liveTransport = transport;
    this.#unsubscribeLiveTransport = transport.subscribe((encoded) => {
      try {
        this.applyEncodedUpdate(encoded, "live_transport");
      } catch (error) {
        this.report(error);
      }
    });
  }

  disconnectLive(): void {
    this.#unsubscribeLiveTransport?.();
    this.#unsubscribeLiveTransport = undefined;
    this.#liveTransport = undefined;
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
    this.disconnectLive();
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
    this.#mutationVersion += 1;
    const encoded = encodeNativeUpdate(event.update, this.document.limits);
    // Capture exactly the state visible to this update before a later
    // synchronous mutation can enter memory. Compaction must not make a frame
    // durable before that frame's own append transaction has succeeded.
    const snapshot = nativeSnapshotIfComplete(this.document);
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
      await this.maybeCompact(result.updateCount, result.logBytes, snapshot);
    }).then(
      () => {
        if (event.local === true) {
          this.publishLive(encoded);
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

  private async maybeCompact(updateCount: number, logBytes: number, snapshot: NativeSnapshot | undefined): Promise<void> {
    if (
      snapshot === undefined ||
      this.#outbox.size !== 0 ||
      (updateCount < this.persistenceLimits.compactAfterUpdates && logBytes < this.persistenceLimits.compactAfterBytes)
    ) {
      return;
    }
    await this.persistence.compact(this.documentID, snapshot);
  }

  private publishLive(encoded: Uint8Array): void {
    const transport = this.#liveTransport;
    if (transport === undefined) {
      return;
    }
    try {
      transport.publish(encoded.slice());
    } catch (error) {
      this.report(error);
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
    await this.compactAfterReceipt();
  }

  /**
   * A receipt can arrive while another synchronous editor update is waiting
   * for its append transaction. Wait for that queue to settle without error,
   * then put read-and-compact ahead of later mutations so its snapshot matches
   * the durable log prefix.
   */
  private async compactAfterReceipt(): Promise<void> {
    const version = this.#mutationVersion;
    await this.#persistenceQueue;
    if (this.#reportedPersistenceError !== undefined || version !== this.#mutationVersion) {
      return;
    }
    const snapshot = nativeSnapshotIfComplete(this.document);
    if (snapshot === undefined) {
      return;
    }
    await this.enqueue(async () => {
      const stored = await this.persistence.load(this.documentID);
      if (stored !== undefined) {
        await this.maybeCompact(stored.updates.length, stored.logBytes, snapshot);
      }
    });
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
      metadata: snapshotMetadata(snapshot),
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
      const request = indexedDB.open(databaseName, DATABASE_FORMAT);
      request.onupgradeneeded = () => {
        const database = request.result;
        if (!database.objectStoreNames.contains(DOCUMENT_STORE)) {
          database.createObjectStore(DOCUMENT_STORE, { keyPath: "key" });
        }
        if (!database.objectStoreNames.contains(UPDATE_STORE)) {
          database.createObjectStore(UPDATE_STORE, { keyPath: ["key", "sequence"] });
        }
        if (!database.objectStoreNames.contains(RGA_DOCUMENT_STORE)) {
          database.createObjectStore(RGA_DOCUMENT_STORE, { keyPath: "key" });
        }
        if (!database.objectStoreNames.contains(RGA_UPDATE_STORE)) {
          database.createObjectStore(RGA_UPDATE_STORE, { keyPath: ["key", "sequence"] });
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
        metadata: snapshotMetadata(snapshot),
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

/** One durable recovery record for a Go/Wasm RGA browser actor. */
export interface RGAWasmStoredDocument {
  /** Complete state + HLC + frontier captured before the retained append log. */
  readonly snapshot: RGASnapshot | undefined;
  readonly updates: readonly BrowserPersistedUpdate[];
  readonly nextSequence: number;
  readonly logBytes: number;
}

/**
 * Persistence boundary for Go/Wasm RGA documents. It is intentionally a
 * separate type and IndexedDB store from `native-ts-v1`: their state and
 * delta formats are not interoperable.
 */
export interface RGAWasmBrowserPersistence {
  load(key: string): Promise<RGAWasmStoredDocument | undefined>;
  append(
    key: string,
    encoded: Uint8Array,
    local: boolean,
    limits: Readonly<BrowserPersistenceLimits>,
  ): Promise<{ readonly sequence: number; readonly updateCount: number; readonly logBytes: number }>;
  acknowledge(key: string, sequence: number): Promise<void>;
  compact(key: string, snapshot: RGASnapshot): Promise<void>;
  close?(): void | Promise<void>;
}

export interface RGAWasmBrowserDocumentOptions {
  /** Authenticated document routing identifier; it is not authorization by itself. */
  readonly documentID: string;
  /** One persistent actor per concurrently active browser context. */
  readonly replicaID: string;
  /** Initialized Go/Wasm RGA runtime selected by the authenticated manifest. */
  readonly runtime: RGAWasmRuntime;
  readonly persistence?: RGAWasmBrowserPersistence;
  /** Used only when `persistence` is omitted. */
  readonly databaseName?: string;
  readonly persistenceLimits?: Partial<BrowserPersistenceLimits>;
  /** Optional authenticated transport; resolution must mean the chosen durable receipt. */
  readonly transport?: NativeBrowserTransport;
  /** Optional best-effort local live path. It never clears the durable outbox. */
  readonly liveTransport?: NativeBrowserLiveTransport;
}

export interface RGAWasmBrowserUpdateEvent {
  readonly encoded: Uint8Array;
  readonly local: boolean;
}

/**
 * Opens a local-first Go/Wasm RGA actor: restore complete snapshot → replay
 * bounded append log → persist every local/remote frame → retry the local
 * outbox. A stored actor is keyed by both document and replica ID so a second
 * tab cannot accidentally reuse its HLC sequence.
 */
export async function openRGAWasmBrowserDocument(
  options: RGAWasmBrowserDocumentOptions,
): Promise<RGAWasmBrowserDocument> {
  assertStorageKey(options.documentID);
  assertStorageKey(options.replicaID);
  if (!isRGAWasmRuntime(options.runtime)) {
    throw new NativeBrowserError("invalid_argument");
  }
  const limits = resolvePersistenceLimits(options.persistenceLimits);
  const ownsPersistence = options.persistence === undefined;
  const persistence = options.persistence ?? (await IndexedDBRGAWasmPersistence.open(options.databaseName));
  const key = rgaPersistenceKey(options.documentID, options.replicaID);
  const stored = await persistence.load(key);
  const document = restoreRGADocument(options.runtime, options.replicaID, stored);
  const client = RGAWasmBrowserDocument.fromRestored(
    options.documentID,
    options.replicaID,
    key,
    document,
    persistence,
    limits,
    ownsPersistence,
    stored,
  );
  if (options.transport !== undefined) {
    await client.connect(options.transport);
  }
  if (options.liveTransport !== undefined) {
    client.connectLive(options.liveTransport);
  }
  return client;
}

/**
 * Browser-facing facade for the canonical Go/Wasm RGA. The underlying handle
 * is deliberately private so accepted local and remote frames cannot bypass
 * the append-log/outbox boundary.
 */
export class RGAWasmBrowserDocument {
  readonly #updateListeners = new Set<(event: RGAWasmBrowserUpdateEvent) => void>();
  readonly #errorListeners = new Set<(error: unknown) => void>();
  readonly #outbox = new Map<number, BrowserPersistedUpdate>();
  #unsubscribeTransport: (() => void) | undefined;
  #unsubscribeLiveTransport: (() => void) | undefined;
  #transport: NativeBrowserTransport | undefined;
  #liveTransport: NativeBrowserLiveTransport | undefined;
  #persistenceQueue: Promise<void> = Promise.resolve();
  #drainPromise: Promise<void> | undefined;
  #reportedPersistenceError: unknown;
  #mutationVersion = 0;
  #closed = false;

  private constructor(
    readonly documentID: string,
    readonly replicaID: string,
    private readonly persistenceKey: string,
    private readonly document: RGAWasmDocument,
    private readonly persistence: RGAWasmBrowserPersistence,
    private readonly persistenceLimits: Readonly<BrowserPersistenceLimits>,
    private readonly ownsPersistence: boolean,
  ) {}

  static fromRestored(
    documentID: string,
    replicaID: string,
    persistenceKey: string,
    document: RGAWasmDocument,
    persistence: RGAWasmBrowserPersistence,
    limits: Readonly<BrowserPersistenceLimits>,
    ownsPersistence: boolean,
    stored: RGAWasmStoredDocument | undefined,
  ): RGAWasmBrowserDocument {
    const client = new RGAWasmBrowserDocument(
      documentID,
      replicaID,
      persistenceKey,
      document,
      persistence,
      limits,
      ownsPersistence,
    );
    if (stored !== undefined) {
      for (const update of stored.updates) {
        if (update.local && update.pending) {
          client.#outbox.set(update.sequence, copyPersistedUpdate(update));
        }
      }
    }
    return client;
  }

  /** Count of durable local updates waiting for an application-defined receipt. */
  get pendingOutbox(): number {
    return this.#outbox.size;
  }

  text(): string {
    this.assertUsable();
    return this.document.text();
  }

  pendingCount(): number {
    this.assertUsable();
    return this.document.pendingCount();
  }

  anchorAt(offset: number): RGAAnchor {
    this.assertUsable();
    return this.document.anchorAt(offset);
  }

  resolveAnchor(anchor: RGAAnchor): number {
    this.assertUsable();
    return this.document.resolveAnchor(anchor);
  }

  insert(offset: number, value: string): Uint8Array {
    this.assertUsable();
    const encoded = this.document.insert(offset, value);
    this.recordUpdate(encoded, true);
    return encoded.slice();
  }

  delete(offset: number, count: number): Uint8Array {
    this.assertUsable();
    const encoded = this.document.delete(offset, count);
    this.recordUpdate(encoded, true);
    return encoded.slice();
  }

  replace(offset: number, count: number, value: string): Uint8Array {
    this.assertUsable();
    const encoded = this.document.replace(offset, count, value);
    this.recordUpdate(encoded, true);
    return encoded.slice();
  }

  /** Applies one authenticated, manifest-checked remote RGA frame before retaining it locally. */
  applyDelta(encoded: Uint8Array): void {
    this.assertUsable();
    this.document.applyDelta(encoded);
    this.recordUpdate(encoded, false);
  }

  onUpdate(listener: (event: RGAWasmBrowserUpdateEvent) => void): () => void {
    this.assertUsable();
    this.#updateListeners.add(listener);
    return () => this.#updateListeners.delete(listener);
  }

  onError(listener: (error: unknown) => void): () => void {
    this.assertUsable();
    this.#errorListeners.add(listener);
    return () => this.#errorListeners.delete(listener);
  }

  async connect(transport: NativeBrowserTransport): Promise<void> {
    this.assertUsable();
    assertTransport(transport);
    this.disconnect();
    this.#transport = transport;
    this.#unsubscribeTransport = transport.subscribe((encoded) => {
      try {
        this.applyDelta(encoded);
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

  /** Attaches an auxiliary live path without treating it as a durable receipt. */
  connectLive(transport: NativeBrowserLiveTransport): void {
    this.assertUsable();
    assertLiveTransport(transport);
    this.disconnectLive();
    this.#liveTransport = transport;
    this.#unsubscribeLiveTransport = transport.subscribe((encoded) => {
      try {
        this.applyDelta(encoded);
      } catch (error) {
        this.report(error);
      }
    });
  }

  disconnectLive(): void {
    this.#unsubscribeLiveTransport?.();
    this.#unsubscribeLiveTransport = undefined;
    this.#liveTransport = undefined;
  }

  /** Waits for IndexedDB commits and then drains the durable local outbox. */
  async flush(): Promise<void> {
    this.assertOpen();
    await this.#persistenceQueue;
    if (this.#reportedPersistenceError !== undefined) {
      throw this.#reportedPersistenceError;
    }
    await this.drainOutbox();
  }

  async close(): Promise<boolean> {
    if (this.#closed) {
      return false;
    }
    await this.flush();
    this.#closed = true;
    this.disconnect();
    this.disconnectLive();
    this.document.close();
    this.#updateListeners.clear();
    this.#errorListeners.clear();
    if (this.ownsPersistence) {
      await this.persistence.close?.();
    }
    return true;
  }

  private recordUpdate(encoded: Uint8Array, local: boolean): void {
    this.#mutationVersion += 1;
    const frame = encoded.slice();
    // Capture at mutation time, before later synchronous editor events can
    // enter the document. Compaction must never checkpoint a frame whose own
    // append transaction has not completed.
    const snapshot = snapshotIfComplete(this.document);
    void this.enqueue(async () => {
      const result = await this.persistence.append(this.persistenceKey, frame, local, this.persistenceLimits);
      if (local) {
        this.#outbox.set(result.sequence, {
          sequence: result.sequence,
          encoded: frame.slice(),
          local: true,
          pending: true,
        });
      }
      this.emit({ encoded: frame, local });
      if (snapshot !== undefined) {
        await this.maybeCompact(result.updateCount, result.logBytes, snapshot);
      }
    }).then(
      () => {
        if (local) {
          this.publishLive(frame);
          void this.drainOutbox().catch((error) => this.report(error));
        }
      },
      (error) => this.report(error),
    );
  }

  private enqueue(operation: () => Promise<void>): Promise<void> {
    const next = this.#persistenceQueue.then(operation);
    this.#persistenceQueue = next.catch((error) => {
      this.#reportedPersistenceError ??= error;
    });
    return next;
  }

  private async maybeCompact(updateCount: number, logBytes: number, snapshot: RGASnapshot): Promise<void> {
    if (
      this.#outbox.size !== 0 ||
      (updateCount < this.persistenceLimits.compactAfterUpdates && logBytes < this.persistenceLimits.compactAfterBytes)
    ) {
      return;
    }
    await this.persistence.compact(this.persistenceKey, snapshot);
  }

  private publishLive(encoded: Uint8Array): void {
    const transport = this.#liveTransport;
    if (transport === undefined) {
      return;
    }
    try {
      transport.publish(encoded.slice());
    } catch (error) {
      this.report(error);
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
        await this.persistence.acknowledge(this.persistenceKey, update.sequence);
        this.#outbox.delete(update.sequence);
      });
    }
    await this.compactAfterReceipt();
  }

  /** See NativeBrowserDocument.compactAfterReceipt for the durable-prefix invariant. */
  private async compactAfterReceipt(): Promise<void> {
    const version = this.#mutationVersion;
    await this.#persistenceQueue;
    if (this.#reportedPersistenceError !== undefined || version !== this.#mutationVersion) {
      return;
    }
    const snapshot = snapshotIfComplete(this.document);
    if (snapshot === undefined) {
      return;
    }
    await this.enqueue(async () => {
      const stored = await this.persistence.load(this.persistenceKey);
      if (stored !== undefined) {
        await this.maybeCompact(stored.updates.length, stored.logBytes, snapshot);
      }
    });
  }

  private emit(event: RGAWasmBrowserUpdateEvent): void {
    for (const listener of [...this.#updateListeners]) {
      listener({ encoded: event.encoded.slice(), local: event.local });
    }
  }

  private report(error: unknown): void {
    for (const listener of [...this.#errorListeners]) {
      try {
        listener(error);
      } catch {
        // Error observers cannot bypass the recovery boundary.
      }
    }
  }

  private assertUsable(): void {
    this.assertOpen();
    if (this.#reportedPersistenceError !== undefined) {
      throw this.#reportedPersistenceError;
    }
  }

  private assertOpen(): void {
    if (this.#closed) {
      throw new NativeBrowserError("document_closed");
    }
  }
}

/** Deterministic RGA persistence for tests and SSR-capable application adapters. */
export class MemoryRGAWasmBrowserPersistence implements RGAWasmBrowserPersistence {
  readonly #documents = new Map<string, RGAWasmStoredDocument>();

  async load(key: string): Promise<RGAWasmStoredDocument | undefined> {
    const record = this.#documents.get(key);
    return record === undefined ? undefined : copyRGAStoredDocument(record);
  }

  async append(
    key: string,
    encoded: Uint8Array,
    local: boolean,
    limits: Readonly<BrowserPersistenceLimits>,
  ): Promise<{ readonly sequence: number; readonly updateCount: number; readonly logBytes: number }> {
    const record = this.#documents.get(key) ?? emptyRGAStoredDocument();
    const nextBytes = record.logBytes + encoded.byteLength;
    if (record.updates.length >= limits.maxUpdates || nextBytes > limits.maxBytes || record.nextSequence >= Number.MAX_SAFE_INTEGER) {
      throw new NativeBrowserError("persistence_limit");
    }
    const sequence = record.nextSequence + 1;
    const update: BrowserPersistedUpdate = { sequence, encoded: encoded.slice(), local, pending: local };
    const next: RGAWasmStoredDocument = {
      snapshot: record.snapshot === undefined ? undefined : copyRGASnapshot(record.snapshot),
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

  async compact(key: string, snapshot: RGASnapshot): Promise<void> {
    const record = this.#documents.get(key);
    if (record?.updates.some((update) => update.local && update.pending)) {
      throw new NativeBrowserError("persistence_failed");
    }
    this.#documents.set(key, {
      snapshot: copyRGASnapshot(snapshot),
      updates: [],
      nextSequence: 0,
      logBytes: 0,
    });
  }
}

/** IndexedDB-backed persistence for canonical Go/Wasm RGA frames. */
export class IndexedDBRGAWasmPersistence implements RGAWasmBrowserPersistence {
  readonly #database: IDBDatabase;

  private constructor(database: IDBDatabase) {
    this.#database = database;
  }

  static async open(databaseName = DEFAULT_DATABASE_NAME): Promise<IndexedDBRGAWasmPersistence> {
    if (typeof indexedDB === "undefined") {
      throw new NativeBrowserError("persistence_unavailable");
    }
    if (typeof databaseName !== "string" || databaseName.length === 0) {
      throw new NativeBrowserError("invalid_argument");
    }
    try {
      const request = indexedDB.open(databaseName, DATABASE_FORMAT);
      request.onupgradeneeded = () => {
        const database = request.result;
        if (!database.objectStoreNames.contains(DOCUMENT_STORE)) {
          database.createObjectStore(DOCUMENT_STORE, { keyPath: "key" });
        }
        if (!database.objectStoreNames.contains(UPDATE_STORE)) {
          database.createObjectStore(UPDATE_STORE, { keyPath: ["key", "sequence"] });
        }
        if (!database.objectStoreNames.contains(RGA_DOCUMENT_STORE)) {
          database.createObjectStore(RGA_DOCUMENT_STORE, { keyPath: "key" });
        }
        if (!database.objectStoreNames.contains(RGA_UPDATE_STORE)) {
          database.createObjectStore(RGA_UPDATE_STORE, { keyPath: ["key", "sequence"] });
        }
      };
      return new IndexedDBRGAWasmPersistence(await requestValue(request));
    } catch (error) {
      throw new NativeBrowserError("persistence_failed", error);
    }
  }

  async load(key: string): Promise<RGAWasmStoredDocument | undefined> {
    try {
      const transaction = this.#database.transaction([RGA_DOCUMENT_STORE, RGA_UPDATE_STORE], "readonly");
      const documentRequest = transaction.objectStore(RGA_DOCUMENT_STORE).get(key);
      const updatesRequest = readUpdates(transaction.objectStore(RGA_UPDATE_STORE), key);
      const [record, updates] = await Promise.all([requestValue(documentRequest), updatesRequest]);
      await transactionComplete(transaction);
      return record === undefined ? undefined : rgaStoredFromIndexedRecord(record, updates);
    } catch (error) {
      throw asPersistenceError(error);
    }
  }

  async append(
    key: string,
    encoded: Uint8Array,
    local: boolean,
    limits: Readonly<BrowserPersistenceLimits>,
  ): Promise<{ readonly sequence: number; readonly updateCount: number; readonly logBytes: number }> {
    try {
      const transaction = this.#database.transaction([RGA_DOCUMENT_STORE, RGA_UPDATE_STORE], "readwrite");
      const documentStore = transaction.objectStore(RGA_DOCUMENT_STORE);
      const existing = await requestValue(documentStore.get(key));
      const record = existing === undefined ? indexedEmptyRGARecord(key) : requireIndexedRGARecord(existing);
      const nextBytes = record.logBytes + encoded.byteLength;
      if (record.updateCount >= limits.maxUpdates || nextBytes > limits.maxBytes || record.nextSequence >= Number.MAX_SAFE_INTEGER) {
        transaction.abort();
        throw new NativeBrowserError("persistence_limit");
      }
      const sequence = record.nextSequence + 1;
      transaction.objectStore(RGA_UPDATE_STORE).add({ key, sequence, encoded: encoded.slice(), local, pending: local });
      documentStore.put({
        ...record,
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
      const transaction = this.#database.transaction(RGA_UPDATE_STORE, "readwrite");
      const store = transaction.objectStore(RGA_UPDATE_STORE);
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

  async compact(key: string, snapshot: RGASnapshot): Promise<void> {
    try {
      const transaction = this.#database.transaction([RGA_DOCUMENT_STORE, RGA_UPDATE_STORE], "readwrite");
      const updateStore = transaction.objectStore(RGA_UPDATE_STORE);
      const updates = await readUpdates(updateStore, key);
      if (updates.some((update) => update.local && update.pending)) {
        transaction.abort();
        throw new NativeBrowserError("persistence_failed");
      }
      for (const update of updates) {
        updateStore.delete([key, update.sequence]);
      }
      transaction.objectStore(RGA_DOCUMENT_STORE).put({
        key,
        format: RGA_BROWSER_PERSISTENCE_FORMAT,
        snapshot: copyRGASnapshot(snapshot),
        nextSequence: 0,
        updateCount: 0,
        logBytes: 0,
      } satisfies IndexedRGAWasmDocumentRecord);
      await transactionComplete(transaction);
    } catch (error) {
      throw asPersistenceError(error);
    }
  }

  close(): void {
    this.#database.close();
  }
}

/** A same-origin, volatile multi-tab live path; it is not authentication or durable delivery. */
export class BroadcastChannelNativeTransport implements NativeBrowserLiveTransport {
  readonly #channel: BroadcastChannel;
  readonly #source = createBrowserReplicaID("tab");

  constructor(channelName: string) {
    if (typeof BroadcastChannel === "undefined") {
      throw new NativeBrowserError("persistence_unavailable");
    }
    assertStorageKey(channelName);
    this.#channel = new BroadcastChannel(channelName);
  }

  publish(encoded: Uint8Array): void {
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

interface IndexedRGAWasmDocumentRecord {
  readonly key: string;
  readonly format: number;
  readonly snapshot?: RGASnapshot;
  readonly nextSequence: number;
  readonly updateCount: number;
  readonly logBytes: number;
}

function isRGAWasmRuntime(value: unknown): value is RGAWasmRuntime {
  return isRecord(value) && typeof value.create === "function" && typeof value.restore === "function";
}

function rgaPersistenceKey(documentID: string, replicaID: string): string {
  // JSON preserves the two-component boundary even when caller IDs include a
  // control character, preventing document/actor key aliasing in IndexedDB.
  return JSON.stringify([documentID, replicaID]);
}

function restoreRGADocument(
  runtime: RGAWasmRuntime,
  replicaID: string,
  stored: RGAWasmStoredDocument | undefined,
): RGAWasmDocument {
  try {
    let document: RGAWasmDocument;
    if (stored?.snapshot === undefined) {
      document = runtime.create(replicaID);
    } else {
      if (stored.snapshot.clock.replicaID !== replicaID) {
        throw new NativeBrowserError("persistence_failed");
      }
      document = runtime.restore(stored.snapshot);
    }
    for (const update of stored?.updates ?? []) {
      document.applyDelta(update.encoded);
    }
    return document;
  } catch (error) {
    throw asPersistenceError(error);
  }
}

function snapshotIfComplete(document: RGAWasmDocument): RGASnapshot | undefined {
  try {
    return document.snapshot();
  } catch (error) {
    if (error instanceof CRDTRuntimeError && error.code === "incomplete_state") {
      return undefined;
    }
    throw error;
  }
}

function emptyRGAStoredDocument(): RGAWasmStoredDocument {
  return { snapshot: undefined, updates: [], nextSequence: 0, logBytes: 0 };
}

function indexedEmptyRGARecord(key: string): IndexedRGAWasmDocumentRecord {
  return {
    key,
    format: RGA_BROWSER_PERSISTENCE_FORMAT,
    nextSequence: 0,
    updateCount: 0,
    logBytes: 0,
  };
}

function copyRGAStoredDocument(document: RGAWasmStoredDocument): RGAWasmStoredDocument {
  return {
    snapshot: document.snapshot === undefined ? undefined : copyRGASnapshot(document.snapshot),
    updates: document.updates.map(copyPersistedUpdate),
    nextSequence: document.nextSequence,
    logBytes: document.logBytes,
  };
}

function copyRGASnapshot(snapshot: RGASnapshot): RGASnapshot {
  if (
    !isRecord(snapshot) ||
    !(snapshot.state instanceof Uint8Array) ||
    !isRGATag(snapshot.clock) ||
    !Array.isArray(snapshot.frontier) ||
    !snapshot.frontier.every(isRGATag)
  ) {
    throw new NativeBrowserError("persistence_failed");
  }
  const frontier = snapshot.frontier.map(copyRGATag);
  if (new Set(frontier.map((tag) => tag.replicaID)).size !== frontier.length) {
    throw new NativeBrowserError("persistence_failed");
  }
  return { state: snapshot.state.slice(), clock: copyRGATag(snapshot.clock), frontier };
}

function isRGATag(value: unknown): value is RGASnapshot["clock"] {
  return isRecord(value) &&
    typeof value.replicaID === "string" && value.replicaID.trim() !== "" &&
    typeof value.wallTime === "bigint" && value.wallTime >= 0n &&
    typeof value.logical === "bigint" && value.logical >= 0n;
}

function copyRGATag(tag: RGASnapshot["clock"]): RGASnapshot["clock"] {
  return { replicaID: tag.replicaID, wallTime: tag.wallTime, logical: tag.logical };
}

function rgaStoredFromIndexedRecord(value: unknown, updates: readonly IndexedUpdateRecord[]): RGAWasmStoredDocument {
  const record = requireIndexedRGARecord(value);
  if (record.updateCount !== updates.length || record.logBytes !== updates.reduce((total, update) => total + update.encoded.byteLength, 0)) {
    throw new NativeBrowserError("persistence_failed");
  }
  return {
    snapshot: record.snapshot === undefined ? undefined : copyRGASnapshot(record.snapshot),
    updates: updates.map(copyPersistedUpdate),
    nextSequence: record.nextSequence,
    logBytes: record.logBytes,
  };
}

function requireIndexedRGARecord(value: unknown): IndexedRGAWasmDocumentRecord {
  if (
    !isRecord(value) ||
    value.format !== RGA_BROWSER_PERSISTENCE_FORMAT ||
    typeof value.key !== "string" ||
    !(value.snapshot === undefined || isRecord(value.snapshot)) ||
    typeof value.nextSequence !== "number" ||
    !Number.isSafeInteger(value.nextSequence) ||
    value.nextSequence < 0 ||
    typeof value.updateCount !== "number" ||
    !Number.isSafeInteger(value.updateCount) ||
    value.updateCount < 0 ||
    typeof value.logBytes !== "number" ||
    !Number.isSafeInteger(value.logBytes) ||
    value.logBytes < 0
  ) {
    throw new NativeBrowserError("persistence_failed");
  }
  if (value.snapshot !== undefined) {
    copyRGASnapshot(value.snapshot as unknown as RGASnapshot);
  }
  return value as unknown as IndexedRGAWasmDocumentRecord;
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
    stored.metadata.stateVector === undefined
      ? { roots: stored.metadata.roots, updates: stored.baseUpdates, counter: stored.metadata.counter }
      : {
        roots: stored.metadata.roots,
        updates: stored.baseUpdates,
        counter: stored.metadata.counter,
        stateVector: stored.metadata.stateVector,
      },
    options,
  );
  for (const update of stored.updates) {
    document.applyEncodedUpdate(update.encoded, "persistence-replay");
  }
  return document;
}

function nativeSnapshotIfComplete(document: NativeDocument): NativeSnapshot | undefined {
  try {
    return document.snapshot();
  } catch (error) {
    // An unresolved array parent cannot form a complete compacted base. Its
    // append log stays the recovery source until a later update resolves it.
    if (error instanceof NativeCRDTError && error.code === "incomplete_state") {
      return undefined;
    }
    throw error;
  }
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
  const copied: NativePersistenceMetadata = {
    roots: metadata.roots.map(copyRoot),
    counter: metadata.counter,
  };
  if (metadata.stateVector !== undefined) {
    return { ...copied, stateVector: copyStateVector(metadata.stateVector) };
  }
  return copied;
}

function snapshotMetadata(snapshot: NativeSnapshot): NativePersistenceMetadata {
  return copyMetadata(
    snapshot.stateVector === undefined
      ? { roots: snapshot.roots, counter: snapshot.counter }
      : { roots: snapshot.roots, counter: snapshot.counter, stateVector: snapshot.stateVector },
  );
}

function copyRoot(root: NativeRoot): NativeRoot {
  return { name: root.name, type: root.type };
}

function copyStateVector(vector: NativeStateVector): NativeStateVector {
  return {
    version: vector.version,
    entries: vector.entries.map((entry) => ({
      actor: entry.actor,
      ranges: entry.ranges.map((range) => ({ from: range.from, to: range.to })),
    })),
  };
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

function assertLiveTransport(value: unknown): asserts value is NativeBrowserLiveTransport {
  if (!isRecord(value) || typeof value.publish !== "function" || typeof value.subscribe !== "function") {
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
