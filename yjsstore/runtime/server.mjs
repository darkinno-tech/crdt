import { createHash, randomBytes, timingSafeEqual } from "node:crypto";
import { createServer } from "node:http";
import { promises as fs } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import * as Y from "yjs";

const recordVersion = 1;
const maximumUpdateBytes = 64 << 20;
const maximumStateVectorBytes = 1 << 20;
const maximumSnapshotBytes = 128 << 20;
const maximumMergeUpdates = 1 << 16;
const maximumRequestBytes = 160 << 20;
const maximumCursor = Number.MAX_SAFE_INTEGER;
const emptyV1 = Y.encodeStateAsUpdate(new Y.Doc());
const emptyV2 = Y.encodeStateAsUpdateV2(new Y.Doc());

export class YJSStoreError extends Error {
  constructor(code, status) {
    super(code);
    this.code = code;
    this.status = status;
  }
}

// loadConfig is exported for tests and deployment wrappers. The data directory
// and token are mandatory because this service is a persistence boundary, not
// a browser-facing development server.
export async function loadConfig(environment = process.env) {
  const dataDir = environment.YJS_STORE_DATA_DIR;
  const token = environment.YJS_STORE_TOKEN;
  if (typeof dataDir !== "string" || dataDir.length === 0 || !validToken(token)) {
    throw new Error("YJS_STORE_DATA_DIR and a 32+ byte YJS_STORE_TOKEN are required");
  }
  const config = {
    dataDir: resolve(dataDir),
    token,
    host: environment.YJS_STORE_HOST ?? "127.0.0.1",
    port: boundedEnvironmentInteger(environment.YJS_STORE_PORT ?? "8080", 0, 65535, "YJS_STORE_PORT"),
    maxUpdateBytes: boundedEnvironmentInteger(environment.YJS_STORE_MAX_UPDATE_BYTES ?? `${1 << 20}`, 1, maximumUpdateBytes, "YJS_STORE_MAX_UPDATE_BYTES"),
    maxStateVectorBytes: boundedEnvironmentInteger(environment.YJS_STORE_MAX_STATE_VECTOR_BYTES ?? `${64 << 10}`, 1, maximumStateVectorBytes, "YJS_STORE_MAX_STATE_VECTOR_BYTES"),
    maxSnapshotBytes: boundedEnvironmentInteger(environment.YJS_STORE_MAX_SNAPSHOT_BYTES ?? `${16 << 20}`, 1, maximumSnapshotBytes, "YJS_STORE_MAX_SNAPSHOT_BYTES"),
    maxMergeUpdates: boundedEnvironmentInteger(environment.YJS_STORE_MAX_MERGE_UPDATES ?? "256", 1, maximumMergeUpdates, "YJS_STORE_MAX_MERGE_UPDATES"),
  };
  if (config.maxSnapshotBytes < config.maxUpdateBytes) {
    throw new Error("YJS_STORE_MAX_SNAPSHOT_BYTES must cover one update");
  }
  const maximumMergeBytes = Math.min(config.maxSnapshotBytes, config.maxUpdateBytes * config.maxMergeUpdates);
  config.maxRequestBytes = Math.min(maximumRequestBytes, Math.max(
    encodedByteLength(config.maxSnapshotBytes) + encodedByteLength(config.maxStateVectorBytes) + 4096,
    encodedByteLength(maximumMergeBytes) + 4096,
  ));
  await ensureSecureDataDirectory(config.dataDir);
  return config;
}

// createYJSStoreServer constructs the local semantic service. It never adds
// CORS headers and should normally listen only on loopback; a gateway owns
// client authentication, authorization, origin checks, rate limits, and TLS.
export function createYJSStoreServer(config) {
  validateConfig(config);
  const locks = new KeyedLock();
  const server = createServer((request, response) => {
    void handleRequest(config, locks, request, response).catch((error) => sendError(response, error));
  });
  return {
    server,
    async listen() {
      await new Promise((resolveListen, rejectListen) => {
        server.once("error", rejectListen);
        server.listen(config.port, config.host, () => {
          server.off("error", rejectListen);
          resolveListen();
        });
      });
      const address = server.address();
      if (address === null || typeof address === "string") {
        throw new Error("Yjs store did not bind a TCP address");
      }
      return `http://${address.address.includes(":") ? `[${address.address}]` : address.address}:${address.port}`;
    },
    async close() {
      await new Promise((resolveClose, rejectClose) => server.close((error) => error ? rejectClose(error) : resolveClose()));
    },
  };
}

async function handleRequest(config, locks, request, response) {
  if (request.method !== "POST" || !isSupportedPath(request.url)) {
    throw new YJSStoreError("invalid_request", 404);
  }
  if (!authorized(request.headers.authorization, config.token)) {
    request.resume();
    throw new YJSStoreError("unauthorized", 401);
  }
  if (typeof request.headers["content-type"] !== "string" || !request.headers["content-type"].toLowerCase().startsWith("application/json")) {
    throw new YJSStoreError("invalid_request", 400);
  }
  const length = request.headers["content-length"];
  if (length !== undefined && (!/^(0|[1-9][0-9]*)$/.test(length) || Number(length) > config.maxRequestBytes)) {
    throw new YJSStoreError("limit_exceeded", 413);
  }
  const payload = parseJSON(await readBody(request, config.maxRequestBytes));
  let result;
  switch (request.url) {
    case "/v1/yjs/apply":
      result = await apply(config, locks, payload);
      break;
    case "/v1/yjs/state-vector":
      result = await stateVector(config, locks, payload);
      break;
    case "/v1/yjs/diff":
      result = await diff(config, locks, payload);
      break;
    case "/v1/yjs/snapshot":
      result = await snapshot(config, locks, payload);
      break;
    case "/v1/yjs/merge":
      result = await merge(config, payload);
      break;
    default:
      throw new YJSStoreError("invalid_request", 404);
  }
  const body = Buffer.from(JSON.stringify(result));
  response.writeHead(200, { "content-type": "application/json", "content-length": body.length, "cache-control": "no-store" });
  response.end(body);
}

async function apply(config, locks, payload) {
  requireKeys(payload, ["document", "update"]);
  const document = parseDocument(payload.document);
  const update = decodeBytes(payload.update, config.maxUpdateBytes, false);
  const key = documentKey(document);
  return locks.run(key, async () => {
    const state = await loadDocument(config, document, key);
    const engine = engineFor(document.format);
    const updateError = validateUpdateFormat(document.format, update);
    if (updateError !== null) {
      throw new YJSStoreError(updateError, 400);
    }
    const before = boundedUpdate(engine.encode(state.document), config.maxSnapshotBytes);
    try {
      engine.apply(state.document, update);
    } catch {
      throw new YJSStoreError("invalid_update", 400);
    }
    const merged = boundedUpdate(engine.encode(state.document), config.maxSnapshotBytes);
    const vector = boundedUpdate(Y.encodeStateVector(state.document), config.maxStateVectorBytes);
    if (equalBytes(before, merged)) {
      return { applied: false, cursor: state.cursor, stateVector: encodeBytes(vector) };
    }
    if (state.cursor >= maximumCursor) {
      throw new YJSStoreError("limit_exceeded", 413);
    }
    const cursor = state.cursor + 1;
    await persistDocument(config, document, key, cursor, merged, vector);
    return { applied: true, cursor, stateVector: encodeBytes(vector) };
  });
}

async function stateVector(config, locks, payload) {
  requireKeys(payload, ["document"]);
  const document = parseDocument(payload.document);
  const key = documentKey(document);
  return locks.run(key, async () => {
    const state = await loadDocument(config, document, key);
    return { stateVector: encodeBytes(boundedUpdate(Y.encodeStateVector(state.document), config.maxStateVectorBytes)) };
  });
}

async function diff(config, locks, payload) {
  requireKeys(payload, ["document", "stateVector"]);
  const document = parseDocument(payload.document);
  const remoteVector = decodeBytes(payload.stateVector, config.maxStateVectorBytes, false);
  const key = documentKey(document);
  return locks.run(key, async () => {
    const state = await loadDocument(config, document, key);
    const engine = engineFor(document.format);
    try {
      return { update: encodeBytes(boundedUpdate(engine.encode(state.document, remoteVector), config.maxSnapshotBytes)) };
    } catch {
      throw new YJSStoreError("invalid_update", 400);
    }
  });
}

async function snapshot(config, locks, payload) {
  requireKeys(payload, ["document"]);
  const document = parseDocument(payload.document);
  const key = documentKey(document);
  return locks.run(key, async () => {
    const state = await loadDocument(config, document, key);
    const engine = engineFor(document.format);
    return {
      cursor: state.cursor,
      update: encodeBytes(boundedUpdate(engine.encode(state.document), config.maxSnapshotBytes)),
      stateVector: encodeBytes(boundedUpdate(Y.encodeStateVector(state.document), config.maxStateVectorBytes)),
    };
  });
}

async function merge(config, payload) {
  requireKeys(payload, ["document", "updates"]);
  const document = parseDocument(payload.document);
  if (!Array.isArray(payload.updates) || payload.updates.length === 0 || payload.updates.length > config.maxMergeUpdates) {
    throw new YJSStoreError("limit_exceeded", 413);
  }
  let total = 0;
  const engine = engineFor(document.format);
  const updates = payload.updates.map((encoded) => {
    const update = decodeBytes(encoded, config.maxUpdateBytes, false);
    if (update.length > config.maxSnapshotBytes - total) {
      throw new YJSStoreError("limit_exceeded", 413);
    }
    total += update.length;
    const updateError = validateUpdateFormat(document.format, update);
    if (updateError !== null) {
      throw new YJSStoreError(updateError, 400);
    }
    return update;
  });
  try {
    return { update: encodeBytes(boundedUpdate(engine.merge(updates), config.maxSnapshotBytes)) };
  } catch {
    throw new YJSStoreError("invalid_update", 400);
  }
}

function engineFor(format) {
  if (format === "v1") {
    const engine = { apply: Y.applyUpdate, encode: Y.encodeStateAsUpdate, merge: Y.mergeUpdates, decode: Y.decodeUpdate, empty: emptyV1 };
    return { ...engine, valid: (update) => validEngineUpdate(engine, update) };
  }
  const engine = { apply: Y.applyUpdateV2, encode: Y.encodeStateAsUpdateV2, merge: Y.mergeUpdatesV2, decode: Y.decodeUpdateV2, empty: emptyV2 };
  return { ...engine, valid: (update) => validEngineUpdate(engine, update) };
}

function validEngineUpdate(engine, update) {
  try {
    const decoded = engine.decode(update);
    return decoded.structs.length > 0 || decoded.ds.clients.size > 0 || equalBytes(update, engine.empty);
  } catch {
    return false;
  }
}

function validateUpdateFormat(format, update) {
  if (engineFor(format).valid(update)) {
    return null;
  }
  return engineFor(format === "v1" ? "v2" : "v1").valid(update) ? "wrong_format" : "invalid_update";
}

async function loadDocument(config, document, key) {
  const record = await readRecord(config, document, key);
  const value = new Y.Doc({ gc: true });
  if (record === null) {
    return { document: value, cursor: 0 };
  }
  try {
    if (!engineFor(document.format).valid(record.update)) {
      throw new Error("wrong snapshot format");
    }
    engineFor(document.format).apply(value, record.update);
    const actualVector = boundedUpdate(Y.encodeStateVector(value), config.maxStateVectorBytes);
    if (!equalBytes(actualVector, record.stateVector)) {
      throw new Error("state vector mismatch");
    }
  } catch {
    value.destroy();
    throw new YJSStoreError("corrupt_store", 500);
  }
  return { document: value, cursor: record.cursor };
}

async function readRecord(config, document, key) {
  const file = documentPath(config, key);
  let info;
  try {
    info = await fs.lstat(file);
  } catch (error) {
    if (error && error.code === "ENOENT") {
      return null;
    }
    throw new YJSStoreError("unavailable", 503);
  }
  if (!info.isFile() || info.isSymbolicLink() || (info.mode & 0o077) !== 0 || info.size > encodedByteLength(config.maxSnapshotBytes) + encodedByteLength(config.maxStateVectorBytes) + 4096) {
    throw new YJSStoreError("corrupt_store", 500);
  }
  let parsed;
  try {
    parsed = parseJSON(await fs.readFile(file));
    requireKeys(parsed, ["version", "document", "cursor", "update", "stateVector", "checksum"]);
    if (parsed.version !== recordVersion || !sameDocument(parseDocument(parsed.document), document) || !Number.isSafeInteger(parsed.cursor) || parsed.cursor < 1) {
      throw new Error("invalid record metadata");
    }
    const update = decodeBytes(parsed.update, config.maxSnapshotBytes, false);
    const stateVector = decodeBytes(parsed.stateVector, config.maxStateVectorBytes, false);
    if (typeof parsed.checksum !== "string" || !equalStrings(parsed.checksum, recordChecksum(document, parsed.cursor, update, stateVector))) {
      throw new Error("record checksum mismatch");
    }
    return { cursor: parsed.cursor, update, stateVector };
  } catch (error) {
    if (error instanceof YJSStoreError && error.code === "unavailable") {
      throw error;
    }
    throw new YJSStoreError("corrupt_store", 500);
  }
}

async function persistDocument(config, document, key, cursor, update, stateVector) {
  const record = {
    version: recordVersion,
    document,
    cursor,
    update: encodeBytes(update),
    stateVector: encodeBytes(stateVector),
    checksum: recordChecksum(document, cursor, update, stateVector),
  };
  const destination = documentPath(config, key);
  const temporary = join(config.dataDir, `.${key}.${randomBytes(16).toString("hex")}.tmp`);
  let handle;
  try {
    handle = await fs.open(temporary, "wx", 0o600);
    await handle.writeFile(JSON.stringify(record));
    await handle.sync();
    await handle.close();
    handle = undefined;
    await fs.rename(temporary, destination);
    const directory = await fs.open(config.dataDir, "r");
    try {
      await directory.sync();
    } finally {
      await directory.close();
    }
  } catch {
    if (handle !== undefined) {
      await handle.close().catch(() => {});
    }
    await fs.unlink(temporary).catch(() => {});
    throw new YJSStoreError("unavailable", 503);
  }
}

function parseDocument(value) {
  requireKeys(value, ["tenant", "room", "epoch", "schema", "format"]);
  if (!validIdentifier(value.tenant) || !validIdentifier(value.room) || !validIdentifier(value.schema) ||
    typeof value.epoch !== "string" || !/^(0|[1-9][0-9]{0,19})$/.test(value.epoch) || BigInt(value.epoch) > 18446744073709551615n ||
    (value.format !== "v1" && value.format !== "v2")) {
    throw new YJSStoreError("invalid_request", 400);
  }
  return { tenant: value.tenant, room: value.room, epoch: value.epoch, schema: value.schema, format: value.format };
}

function validIdentifier(value) {
  return typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(value);
}

function validToken(value) {
  return typeof value === "string" && /^[\x21-\x7e]{32,}$/.test(value);
}

function documentKey(document) {
  return createHash("sha256").update(`${document.tenant}\u0000${document.room}\u0000${document.epoch}\u0000${document.schema}\u0000${document.format}`).digest("hex");
}

function documentPath(config, key) {
  return join(config.dataDir, `${key}.json`);
}

function sameDocument(left, right) {
  return left.tenant === right.tenant && left.room === right.room && left.epoch === right.epoch && left.schema === right.schema && left.format === right.format;
}

function recordChecksum(document, cursor, update, stateVector) {
  return createHash("sha256")
    .update(documentKey(document))
    .update("\u0000")
    .update(String(cursor))
    .update("\u0000")
    .update(update)
    .update("\u0000")
    .update(stateVector)
    .digest("hex");
}

function decodeBytes(encoded, maximum, allowEmpty) {
  if (typeof encoded !== "string" || (!allowEmpty && encoded.length === 0) ||
    !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(encoded)) {
    throw new YJSStoreError("invalid_request", 400);
  }
  if (encoded.length > encodedByteLength(maximum)) {
    throw new YJSStoreError("limit_exceeded", 413);
  }
  const decoded = Buffer.from(encoded, "base64");
  if (decoded.length > maximum) {
    throw new YJSStoreError("limit_exceeded", 413);
  }
  if ((!allowEmpty && decoded.length === 0) || encodeBytes(decoded) !== encoded) {
    throw new YJSStoreError("invalid_request", 400);
  }
  return new Uint8Array(decoded);
}

function boundedUpdate(update, maximum) {
  if (!(update instanceof Uint8Array) || update.length === 0 || update.length > maximum) {
    throw new YJSStoreError("limit_exceeded", 413);
  }
  return update;
}

function encodeBytes(value) {
  return Buffer.from(value).toString("base64");
}

function encodedByteLength(length) {
  return 4 * Math.ceil(length / 3);
}

function equalBytes(left, right) {
  return left.length === right.length && timingSafeEqual(Buffer.from(left), Buffer.from(right));
}

function requireKeys(value, keys) {
  if (value === null || typeof value !== "object" || Array.isArray(value) || Object.keys(value).length !== keys.length || !keys.every((key) => Object.hasOwn(value, key))) {
    throw new YJSStoreError("invalid_request", 400);
  }
}

function parseJSON(value) {
  try {
    return JSON.parse(Buffer.from(value).toString("utf8"));
  } catch {
    throw new YJSStoreError("invalid_request", 400);
  }
}

async function readBody(request, maximum) {
  const chunks = [];
  let total = 0;
  for await (const chunk of request) {
    total += chunk.length;
    if (total > maximum) {
      throw new YJSStoreError("limit_exceeded", 413);
    }
    chunks.push(chunk);
  }
  return Buffer.concat(chunks, total);
}

function isSupportedPath(value) {
  return value === "/v1/yjs/apply" || value === "/v1/yjs/state-vector" || value === "/v1/yjs/diff" || value === "/v1/yjs/snapshot" || value === "/v1/yjs/merge";
}

function authorized(header, token) {
  if (typeof header !== "string") {
    return false;
  }
  const expected = Buffer.from(`Bearer ${token}`);
  const received = Buffer.from(header);
  return received.length === expected.length && timingSafeEqual(received, expected);
}

function equalStrings(left, right) {
  const leftBytes = Buffer.from(left);
  const rightBytes = Buffer.from(right);
  return leftBytes.length === rightBytes.length && timingSafeEqual(leftBytes, rightBytes);
}

function sendError(response, error) {
  if (response.headersSent) {
    response.destroy();
    return;
  }
  const storeError = error instanceof YJSStoreError ? error : new YJSStoreError("unavailable", 503);
  const body = Buffer.from(JSON.stringify({ code: storeError.code }));
  response.writeHead(storeError.status, { "content-type": "application/json", "content-length": body.length, "cache-control": "no-store" });
  response.end(body);
}

function boundedEnvironmentInteger(value, minimum, maximum, name) {
  if (typeof value !== "string" || !/^(0|[1-9][0-9]*)$/.test(value)) {
    throw new Error(`${name} must be a decimal integer`);
  }
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed < minimum || parsed > maximum) {
    throw new Error(`${name} is out of range`);
  }
  return parsed;
}

async function ensureSecureDataDirectory(dataDir) {
  await fs.mkdir(dataDir, { recursive: true, mode: 0o700 });
  const info = await fs.lstat(dataDir);
  if (!info.isDirectory() || info.isSymbolicLink() || (info.mode & 0o077) !== 0) {
    throw new Error("YJS_STORE_DATA_DIR must be a non-symlink 0700 directory");
  }
}

function validateConfig(config) {
  if (config === null || typeof config !== "object" || typeof config.dataDir !== "string" || !validToken(config.token) ||
    !Number.isInteger(config.maxUpdateBytes) || config.maxUpdateBytes < 1 || config.maxUpdateBytes > maximumUpdateBytes ||
    !Number.isInteger(config.maxStateVectorBytes) || config.maxStateVectorBytes < 1 || config.maxStateVectorBytes > maximumStateVectorBytes ||
    !Number.isInteger(config.maxSnapshotBytes) || config.maxSnapshotBytes < config.maxUpdateBytes || config.maxSnapshotBytes > maximumSnapshotBytes ||
    !Number.isInteger(config.maxMergeUpdates) || config.maxMergeUpdates < 1 || config.maxMergeUpdates > maximumMergeUpdates ||
    !Number.isInteger(config.maxRequestBytes) || config.maxRequestBytes < config.maxUpdateBytes || config.maxRequestBytes > maximumRequestBytes ||
    typeof config.host !== "string" || !Number.isInteger(config.port) || config.port < 0 || config.port > 65535) {
    throw new Error("invalid Yjs store configuration");
  }
}

class KeyedLock {
  #tails = new Map();

  async run(key, operation) {
    const previous = this.#tails.get(key) ?? Promise.resolve();
    let release;
    const current = new Promise((resolveCurrent) => { release = resolveCurrent; });
    this.#tails.set(key, current);
    await previous;
    try {
      return await operation();
    } finally {
      release();
      if (this.#tails.get(key) === current) {
        this.#tails.delete(key);
      }
    }
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  try {
    const config = await loadConfig();
    const service = createYJSStoreServer(config);
    const endpoint = await service.listen();
    process.stdout.write(`YJSStore listening on ${endpoint}\n`);
    const stop = async () => {
      await service.close();
      process.exitCode = 0;
    };
    process.once("SIGINT", stop);
    process.once("SIGTERM", stop);
  } catch (error) {
    process.stderr.write(`YJSStore failed to start: ${error instanceof Error ? error.message : "unknown error"}\n`);
    process.exitCode = 1;
  }
}
