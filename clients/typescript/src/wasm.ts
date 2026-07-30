import { FrameType } from "./frame.js";

/** The global installed by the Go js/wasm entrypoint after it has started. */
export const RGA_WASM_GLOBAL = "__darkinnoCRDTRGA";
const MAX_UINT64 = (1n << 64n) - 1n;
const TEXT_ENCODER = new TextEncoder();

/** One exact RGA frame contract selected by an authenticated manifest. */
export interface RGAProtocolExpectation {
  readonly stateTypeID: bigint;
  readonly deltaTypeID: bigint;
  readonly semanticsVersion: bigint;
}

/** Explicit compatibility contract for legacy scalar RGA v1 groups. */
export const RGA_PROTOCOL_V1: Readonly<RGAProtocolExpectation> = Object.freeze({
  stateTypeID: FrameType.RGAState,
  deltaTypeID: FrameType.RGADelta,
  semanticsVersion: 1n,
});

/** Default compatibility contract for new Go RGA replication groups. */
export const RGA_PROTOCOL_RUN_V2: Readonly<RGAProtocolExpectation> = Object.freeze({
  stateTypeID: FrameType.RGARunState,
  deltaTypeID: FrameType.RGARunDelta,
  semanticsVersion: 2n,
});

export interface InitRGAWasmOptions {
  /** URL of the `crdt-rga.wasm` artifact built with `make wasm`. */
  readonly wasmURL: string | URL;
  /**
   * Exact protocol contract authenticated for the replication group. Defaults
   * to run-v2; pass RGA_PROTOCOL_V1 only with a separately built v1 artifact.
   */
  readonly expectedProtocol?: Readonly<RGAProtocolExpectation>;
  /** Maximum time to wait for the Go runtime to publish its API. */
  readonly startupTimeoutMs?: number;
}

export interface RGAProtocol {
  readonly stateTypeID: bigint;
  readonly deltaTypeID: bigint;
  readonly semanticsVersion: bigint;
  readonly maxFrameBytes: number;
  readonly maxTags: number;
  readonly maxStringBytes: number;
  readonly maxLocalEditBytes: number;
  readonly maxLocalEditRunes: number;
}

export interface RGATag {
  readonly replicaID: string;
  readonly wallTime: bigint;
  readonly logical: bigint;
}

/**
 * Persist every field in one atomic write. `state` without `clock`/`frontier`
 * is unsafe when the same replica ID may be restored after a restart.
 */
export interface RGASnapshot {
  readonly state: Uint8Array;
  readonly clock: RGATag;
  readonly frontier: readonly RGATag[];
}

export class CRDTRuntimeError extends Error {
  readonly code: string;

  constructor(code: string) {
    super(code);
    this.name = "CRDTRuntimeError";
    this.code = code;
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/** A local RGA document backed by the canonical Go Wasm implementation. */
export class RGAWasmDocument {
  #closed = false;

  constructor(
    private readonly api: RawRGAAPI,
    private readonly handle: number,
    private readonly limits: RGAProtocol,
  ) {}

  /** Exact negotiated limits used by editor bindings to split local edits. */
  get protocol(): RGAProtocol {
    this.assertOpen();
    return this.limits;
  }

  /** Inserts UTF-8 text before the visible rune offset and returns a negotiated RGA delta frame. */
  insert(offset: number, value: string): Uint8Array {
    this.assertOpen();
    assertBoundedString(value, this.limits.maxLocalEditBytes, "resource_limit");
    return copiedBytes(unwrap(this.api.insert(this.handle, offset, value)));
  }

  /** Deletes visible runes and returns a canonical tombstone delta frame. */
  delete(offset: number, count: number): Uint8Array {
    this.assertOpen();
    return copiedBytes(unwrap(this.api.delete(this.handle, offset, count)));
  }

  /** Validates and joins one untrusted canonical frame for this runtime's negotiated RGA format. */
  applyDelta(encoded: Uint8Array): void {
    this.assertOpen();
    unwrap<void>(this.api.applyDelta(this.handle, encoded));
  }

  /** Returns the current visible text projection. */
  text(): string {
    this.assertOpen();
    const value = unwrap(this.api.text(this.handle));
    if (typeof value !== "string") {
      throw new CRDTRuntimeError("invalid_runtime_response");
    }
    return value;
  }

  /** Reports accepted nodes still waiting for an out-of-order parent. */
  pendingCount(): number {
    this.assertOpen();
    return nonNegativeSafeInteger(unwrap(this.api.pendingCount(this.handle)));
  }

  /** Returns a fully copied, complete persistence unit. */
  snapshot(): RGASnapshot {
    this.assertOpen();
    return snapshotFromRaw(unwrap(this.api.snapshot(this.handle)), this.limits);
  }

  /** Releases the local document handle. It is safe to call more than once. */
  close(): boolean {
    if (this.#closed) {
      return false;
    }
    this.#closed = true;
    const value = unwrap(this.api.drop(this.handle));
    if (typeof value !== "boolean") {
      throw new CRDTRuntimeError("invalid_runtime_response");
    }
    return value;
  }

  private assertOpen(): void {
    if (this.#closed) {
      throw new CRDTRuntimeError("document_closed");
    }
  }
}

/** One initialized Go Wasm module; it may own multiple RGA documents. */
export class RGAWasmRuntime {
  readonly protocol: RGAProtocol;

  constructor(
    private readonly api: RawRGAAPI,
    protocol: RGAProtocol,
  ) {
    this.protocol = protocol;
  }

  create(replicaID: string): RGAWasmDocument {
    assertBoundedString(replicaID, this.protocol.maxStringBytes, "resource_limit");
    const handle = documentHandle(unwrap(this.api.create(replicaID)));
    return new RGAWasmDocument(this.api, handle, this.protocol);
  }

  restore(snapshot: RGASnapshot): RGAWasmDocument {
    const handle = documentHandle(unwrap(this.api.restore(snapshotToRaw(snapshot, this.protocol))));
    return new RGAWasmDocument(this.api, handle, this.protocol);
  }
}

/**
 * Initializes the RGA Wasm module. Load the `wasm_exec.js` copied by
 * `make wasm` before calling this function; it must come from the same Go
 * toolchain that built the `.wasm` file.
 */
export async function initRGAWasm(options: InitRGAWasmOptions): Promise<RGAWasmRuntime> {
  const expectedProtocol = expectedRGAProtocol(options.expectedProtocol);
  const existing = rawAPIFromGlobal();
  if (existing !== undefined) {
    return new RGAWasmRuntime(existing, readAndValidateProtocol(existing, expectedProtocol));
  }

  const GoConstructor = globalThis.Go;
  if (typeof GoConstructor !== "function") {
    throw new CRDTRuntimeError("missing_wasm_exec");
  }
  const go = new GoConstructor();
  const response = await fetch(options.wasmURL);
  if (!response.ok) {
    throw new CRDTRuntimeError("wasm_fetch_failed");
  }

  const fallback = response.clone();
  let instance: WebAssembly.Instance;
  try {
    if (typeof WebAssembly.instantiateStreaming !== "function") {
      throw new Error("streaming instantiation unavailable");
    }
    const result = await WebAssembly.instantiateStreaming(Promise.resolve(response), go.importObject);
    instance = result.instance;
  } catch {
    const bytes = await fallback.arrayBuffer();
    const result = await WebAssembly.instantiate(bytes, go.importObject);
    instance = result.instance;
  }

  let runFailure: unknown;
  void Promise.resolve()
    .then(() => go.run(instance))
    .catch((error: unknown) => {
      runFailure = error;
    });

  const api = await waitForAPI(options.startupTimeoutMs ?? 5_000, () => runFailure);
  return new RGAWasmRuntime(api, readAndValidateProtocol(api, expectedProtocol));
}

interface GoRuntime {
  readonly importObject: WebAssembly.Imports;
  run(instance: WebAssembly.Instance): Promise<void> | void;
}

interface GoConstructor {
  new (): GoRuntime;
}

declare global {
  var Go: GoConstructor | undefined;
}

interface RawResult {
  readonly ok: unknown;
  readonly value?: unknown;
  readonly error?: unknown;
}

interface RawRGAAPI {
  protocol(): RawResult;
  create(replicaID: string): RawResult;
  drop(handle: number): RawResult;
  insert(handle: number, offset: number, value: string): RawResult;
  delete(handle: number, offset: number, count: number): RawResult;
  applyDelta(handle: number, encoded: Uint8Array): RawResult;
  text(handle: number): RawResult;
  pendingCount(handle: number): RawResult;
  snapshot(handle: number): RawResult;
  restore(snapshot: RawSnapshot): RawResult;
}

interface RawSnapshot {
  readonly state: Uint8Array;
  readonly clock: RawTag;
  readonly frontier: readonly RawTag[];
}

interface RawTag {
  readonly replicaID: string;
  readonly wallTime: string;
  readonly logical: string;
}

type SnapshotLimits = Pick<RGAProtocol, "maxFrameBytes" | "maxTags" | "maxStringBytes">;

async function waitForAPI(timeoutMs: number, runFailure: () => unknown): Promise<RawRGAAPI> {
  if (!Number.isSafeInteger(timeoutMs) || timeoutMs <= 0) {
    throw new CRDTRuntimeError("invalid_startup_timeout");
  }
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const api = rawAPIFromGlobal();
    if (api !== undefined) {
      return api;
    }
    if (runFailure() !== undefined) {
      throw new CRDTRuntimeError("wasm_start_failed");
    }
    if (Date.now() >= deadline) {
      throw new CRDTRuntimeError("wasm_start_timeout");
    }
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
  }
}

function rawAPIFromGlobal(): RawRGAAPI | undefined {
  const candidate = (globalThis as Record<string, unknown>)[RGA_WASM_GLOBAL];
  if (!isRecord(candidate)) {
    return undefined;
  }
  const names = [
    "protocol",
    "create",
    "drop",
    "insert",
    "delete",
    "applyDelta",
    "text",
    "pendingCount",
    "snapshot",
    "restore",
  ] as const;
  if (names.some((name) => typeof candidate[name] !== "function")) {
    return undefined;
  }
  return candidate as unknown as RawRGAAPI;
}

function unwrap<T>(result: RawResult): T {
  if (!isRecord(result) || typeof result.ok !== "boolean") {
    throw new CRDTRuntimeError("invalid_runtime_response");
  }
  if (!result.ok) {
    throw new CRDTRuntimeError(typeof result.error === "string" ? result.error : "operation_failed");
  }
  return result.value as T;
}

function readAndValidateProtocol(api: RawRGAAPI, expected: Readonly<RGAProtocolExpectation>): RGAProtocol {
  const raw = unwrap(api.protocol());
  if (!isRecord(raw)) {
    throw new CRDTRuntimeError("invalid_runtime_response");
  }
  const protocol: RGAProtocol = {
    stateTypeID: parseUnsignedInteger(raw.stateTypeID),
    deltaTypeID: parseUnsignedInteger(raw.deltaTypeID),
    semanticsVersion: parseUnsignedInteger(raw.semanticsVersion),
    maxFrameBytes: nonNegativeSafeInteger(raw.maxFrameBytes),
    maxTags: nonNegativeSafeInteger(raw.maxTags),
    maxStringBytes: nonNegativeSafeInteger(raw.maxStringBytes),
    maxLocalEditBytes: nonNegativeSafeInteger(raw.maxLocalEditBytes),
    maxLocalEditRunes: nonNegativeSafeInteger(raw.maxLocalEditRunes),
  };
  if (
    protocol.stateTypeID !== expected.stateTypeID ||
    protocol.deltaTypeID !== expected.deltaTypeID ||
    protocol.semanticsVersion !== expected.semanticsVersion ||
    protocol.maxFrameBytes <= 0 ||
    protocol.maxTags <= 0 ||
    protocol.maxStringBytes <= 0 ||
    protocol.maxLocalEditBytes <= 0 ||
    protocol.maxLocalEditRunes <= 0 ||
    protocol.maxLocalEditBytes > protocol.maxFrameBytes
  ) {
    throw new CRDTRuntimeError("protocol_mismatch");
  }
  return protocol;
}

function expectedRGAProtocol(value: unknown): Readonly<RGAProtocolExpectation> {
  const expected = value ?? RGA_PROTOCOL_RUN_V2;
  if (isKnownRGAProtocol(expected)) {
    return expected;
  }
  throw new CRDTRuntimeError("protocol_mismatch");
}

function isKnownRGAProtocol(value: unknown): value is Readonly<RGAProtocolExpectation> {
  if (
    !isRecord(value) ||
    typeof value.stateTypeID !== "bigint" ||
    typeof value.deltaTypeID !== "bigint" ||
    typeof value.semanticsVersion !== "bigint"
  ) {
    return false;
  }
  return (
    (value.stateTypeID === RGA_PROTOCOL_V1.stateTypeID &&
      value.deltaTypeID === RGA_PROTOCOL_V1.deltaTypeID &&
      value.semanticsVersion === RGA_PROTOCOL_V1.semanticsVersion) ||
    (value.stateTypeID === RGA_PROTOCOL_RUN_V2.stateTypeID &&
      value.deltaTypeID === RGA_PROTOCOL_RUN_V2.deltaTypeID &&
      value.semanticsVersion === RGA_PROTOCOL_RUN_V2.semanticsVersion)
  );
}

function snapshotFromRaw(raw: unknown, limits: SnapshotLimits): RGASnapshot {
  if (
    !isRecord(raw) ||
    !(raw.state instanceof Uint8Array) ||
    raw.state.byteLength > limits.maxFrameBytes ||
    !Array.isArray(raw.frontier) ||
    raw.frontier.length > limits.maxTags
  ) {
    throw new CRDTRuntimeError("invalid_runtime_response");
  }
  const frontier = raw.frontier.map((tag) => tagFromRaw(tag, limits));
  if (new Set(frontier.map((tag) => tag.replicaID)).size !== frontier.length) {
    throw new CRDTRuntimeError("invalid_runtime_response");
  }
  return {
    state: copiedBytes(raw.state),
    clock: tagFromRaw(raw.clock, limits),
    frontier,
  };
}

function snapshotToRaw(snapshot: RGASnapshot, limits: SnapshotLimits): RawSnapshot {
  if (
    !isRecord(snapshot) ||
    !(snapshot.state instanceof Uint8Array) ||
    snapshot.state.byteLength > limits.maxFrameBytes ||
    !Array.isArray(snapshot.frontier) ||
    snapshot.frontier.length > limits.maxTags
  ) {
    throw new CRDTRuntimeError("invalid_snapshot");
  }
  const frontier = snapshot.frontier.map((tag) => tagToRaw(tag, limits));
  const replicaIDs = new Set(frontier.map((tag) => tag.replicaID));
  if (replicaIDs.size !== frontier.length) {
    throw new CRDTRuntimeError("invalid_snapshot");
  }
  return {
    state: snapshot.state.slice(),
    clock: tagToRaw(snapshot.clock, limits),
    frontier,
  };
}

function tagFromRaw(raw: unknown, limits: SnapshotLimits): RGATag {
  if (
    !isRecord(raw) ||
    typeof raw.replicaID !== "string" ||
    raw.replicaID.trim() === "" ||
    utf8ByteLength(raw.replicaID) > limits.maxStringBytes
  ) {
    throw new CRDTRuntimeError("invalid_runtime_response");
  }
  const wallTime = parseUnsignedInteger(raw.wallTime);
  const logical = parseUnsignedInteger(raw.logical);
  if (!isUint64(wallTime) || !isUint64(logical)) {
    throw new CRDTRuntimeError("invalid_runtime_response");
  }
  return {
    replicaID: raw.replicaID,
    wallTime,
    logical,
  };
}

function tagToRaw(tag: RGATag, limits: SnapshotLimits): RawTag {
  if (
    !isRecord(tag) ||
    typeof tag.replicaID !== "string" ||
    tag.replicaID.trim() === "" ||
    utf8ByteLength(tag.replicaID) > limits.maxStringBytes ||
    !isUint64(tag.wallTime) ||
    !isUint64(tag.logical)
  ) {
    throw new CRDTRuntimeError("invalid_snapshot");
  }
  return {
    replicaID: tag.replicaID,
    wallTime: tag.wallTime.toString(),
    logical: tag.logical.toString(),
  };
}

function isUint64(value: unknown): value is bigint {
  return typeof value === "bigint" && value >= 0n && value <= MAX_UINT64;
}

function utf8ByteLength(value: string): number {
  return TEXT_ENCODER.encode(value).byteLength;
}

function assertBoundedString(value: unknown, maxBytes: number, errorCode: string): asserts value is string {
  if (typeof value !== "string") {
    throw new CRDTRuntimeError("invalid_argument");
  }
  if (utf8ByteLength(value) > maxBytes) {
    throw new CRDTRuntimeError(errorCode);
  }
}

function copiedBytes(value: unknown): Uint8Array {
  if (!(value instanceof Uint8Array)) {
    throw new CRDTRuntimeError("invalid_runtime_response");
  }
  return value.slice();
}

function documentHandle(value: unknown): number {
  const handle = nonNegativeSafeInteger(value);
  if (handle === 0) {
    throw new CRDTRuntimeError("invalid_runtime_response");
  }
  return handle;
}

function nonNegativeSafeInteger(value: unknown): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) {
    throw new CRDTRuntimeError("invalid_runtime_response");
  }
  return value;
}

function parseUnsignedInteger(value: unknown): bigint {
  if (typeof value !== "string" || !/^(0|[1-9][0-9]*)$/.test(value)) {
    throw new CRDTRuntimeError("invalid_runtime_response");
  }
  return BigInt(value);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}
