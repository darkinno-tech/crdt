import assert from "node:assert/strict";
import { once } from "node:events";
import { chmod, mkdtemp, readdir, writeFile } from "node:fs/promises";
import { request as httpRequest } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import * as Y from "yjs";

import { createYJSStoreServer, loadConfig } from "./server.mjs";

const token = "yjs-store-test-token-0123456789abcdef";

test("the bundled sidecar only permits literal loopback listeners", async () => {
  const dataDir = await mkdtemp(join(tmpdir(), "darkinno-yjs-store-loopback-"));
  const base = {
    YJS_STORE_DATA_DIR: dataDir,
    YJS_STORE_TOKEN: token,
    YJS_STORE_PORT: "0",
  };
  for (const host of ["0.0.0.0", "::", "localhost", "store.example"]) {
    await assert.rejects(() => loadConfig({ ...base, YJS_STORE_HOST: host }), /loopback/);
  }
  const ipv6 = await loadConfig({ ...base, YJS_STORE_HOST: "::1" });
  assert.equal(ipv6.host, "::1");
  assert.throws(() => createYJSStoreServer({ ...ipv6, host: "0.0.0.0" }), /invalid Yjs store configuration/);
  await assert.rejects(() => loadConfig({ ...base, YJS_STORE_MAX_CONCURRENT_REQUESTS: "0" }), /out of range/);
  await assert.rejects(() => loadConfig({ ...base, YJS_STORE_REQUEST_TIMEOUT_MS: "999" }), /out of range/);
});

test("the sidecar rechecks its durable directory before listening", async () => {
  const dataDir = await mkdtemp(join(tmpdir(), "darkinno-yjs-store-permissions-"));
  const config = await loadConfig({
    YJS_STORE_DATA_DIR: dataDir,
    YJS_STORE_TOKEN: token,
    YJS_STORE_PORT: "0",
  });
  await chmod(dataDir, 0o755);
  const service = createYJSStoreServer(config);
  await assert.rejects(() => service.listen(), /non-symlink 0700 directory/);
});

test("the sidecar rejects an over-cap active request and releases capacity after the request completes", async (context) => {
  const running = await startStore(context, undefined, { maxConcurrentRequests: 1 });
  const document = testDocument("v1");
  const author = new Y.Doc();
  const update = captureUpdate(author, "update", () => author.getText("bounded").insert(0, "write"));
  const requestStarted = once(running.service.server, "request");
  const held = holdJSONRequest(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(update) });
  try {
    await requestStarted;
    const rejected = await request(running.endpoint, "/v1/yjs/snapshot", { document });
    assert.equal(rejected.status, 503);
    assert.equal(rejected.body.code, "unavailable");

    held.finish();
    const completed = await held.result;
    assert.equal(completed.status, 200);
    const snapshot = await request(running.endpoint, "/v1/yjs/snapshot", { document });
    assert.equal(snapshot.status, 200, "completion releases the sidecar admission slot");
  } finally {
    held.finish();
    await held.result.catch(() => {});
    author.destroy();
  }
});

test("the sidecar times out an incomplete body before it can retain its admission slot", async (context) => {
  const running = await startStore(context, undefined, { maxConcurrentRequests: 1, requestTimeoutMillis: 1000 });
  const document = testDocument("v1");
  const author = new Y.Doc();
  const update = captureUpdate(author, "update", () => author.getText("timeout").insert(0, "write"));
  const requestStarted = once(running.service.server, "request");
  const held = holdJSONRequest(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(update) });
  try {
    await requestStarted;
    const timedOut = await held.result;
    assert.equal(timedOut.status, 408);
    const snapshot = await request(running.endpoint, "/v1/yjs/snapshot", { document });
    assert.equal(snapshot.status, 200, "a timed-out body releases its slot for later durable work");
  } finally {
    held.finish();
    await held.result.catch(() => {});
    author.destroy();
  }
});

test("real Yjs v1 state-vector diff, merge, and durable recovery converge", async (context) => {
  const running = await startStore(context);
  const document = testDocument("v1");
  const author = new Y.Doc();
  const base = captureUpdate(author, "update", () => author.transact(() => {
    author.getText("title").insert(0, "initial");
    const metadata = author.getMap("metadata");
    metadata.set("labels", new Y.Array(["seed"]));
    metadata.set("nested", new Y.Map([["published", false]]));
  }));
  assert.equal((await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(base) })).body.applied, true);

  const replica = new Y.Doc();
  Y.applyUpdate(replica, base);
  const remoteVector = Y.encodeStateVector(replica);
  const incremental = captureUpdate(author, "update", () => author.transact(() => {
    author.getText("title").insert(author.getText("title").length, " document");
    author.getMap("metadata").get("labels").push(["durable"]);
    author.getMap("metadata").get("nested").set("published", true);
  }));
  const applied = await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(incremental) });
  assert.equal(applied.status, 200);
  assert.equal(applied.body.applied, true);
  assert.equal(applied.body.cursor, 2);

  const delta = await request(running.endpoint, "/v1/yjs/diff", { document, stateVector: toBase64(remoteVector) });
  assert.equal(delta.status, 200);
  Y.applyUpdate(replica, fromBase64(delta.body.update));
  assert.deepEqual(documentView(replica), documentView(author));

  const snapshot = await request(running.endpoint, "/v1/yjs/snapshot", { document });
  assert.equal(snapshot.status, 200);
  assert.equal(snapshot.body.cursor, 2);
  const restored = new Y.Doc();
  Y.applyUpdate(restored, fromBase64(snapshot.body.update));
  assert.deepEqual(documentView(restored), documentView(author));
  assert.deepEqual([...fromBase64(snapshot.body.stateVector)], [...Y.encodeStateVector(author)]);

  const left = new Y.Doc();
  const right = new Y.Doc();
  Y.applyUpdate(left, base);
  Y.applyUpdate(right, base);
  const leftUpdate = captureUpdate(left, "update", () => left.getText("title").insert(0, "left "));
  const rightUpdate = captureUpdate(right, "update", () => right.getMap("metadata").set("reviewed", true));
  const merged = await request(running.endpoint, "/v1/yjs/merge", { document, updates: [toBase64(base), toBase64(leftUpdate), toBase64(rightUpdate)] });
  assert.equal(merged.status, 200);
  const mergedReplica = new Y.Doc();
  Y.applyUpdate(mergedReplica, fromBase64(merged.body.update));
  const expected = new Y.Doc();
  Y.applyUpdate(expected, base);
  Y.applyUpdate(expected, leftUpdate);
  Y.applyUpdate(expected, rightUpdate);
  assert.deepEqual(documentView(mergedReplica), documentView(expected));

  const originalSnapshot = snapshot.body;
  await running.close();
  const restarted = await startStore(context, running.dataDir);
  const afterRestart = await request(restarted.endpoint, "/v1/yjs/snapshot", { document });
  assert.deepEqual(afterRestart.body, originalSnapshot);
});

test("real Yjs V2 stays format-pinned and synchronizes from a state vector", async (context) => {
  const running = await startStore(context);
  const document = testDocument("v2");
  const author = new Y.Doc();
  const base = captureUpdate(author, "updateV2", () => author.getText("title").insert(0, "V2"));
  assert.equal((await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(base) })).status, 200);
  const replica = new Y.Doc();
  Y.applyUpdateV2(replica, base);
  const vector = Y.encodeStateVector(replica);
  const incremental = captureUpdate(author, "updateV2", () => author.getText("title").insert(2, " semantic storage"));
  await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(incremental) });
  const delta = await request(running.endpoint, "/v1/yjs/diff", { document, stateVector: toBase64(vector) });
  Y.applyUpdateV2(replica, fromBase64(delta.body.update));
  assert.deepEqual(documentView(replica), documentView(author));
  const wrongFormat = await request(running.endpoint, "/v1/yjs/apply", { document: testDocument("v1"), update: toBase64(base) });
  assert.equal(wrongFormat.status, 400);
  assert.equal(wrongFormat.body.code, "wrong_format");
});

test("pure Yjs deletions persist even though their state vector does not advance", async (context) => {
  const running = await startStore(context);
  for (const format of ["v1", "v2"]) {
    const eventName = format === "v1" ? "update" : "updateV2";
    const applyUpdate = format === "v1" ? Y.applyUpdate : Y.applyUpdateV2;
    const document = testDocument(format);
    const author = new Y.Doc();
    const deleter = new Y.Doc();
    const base = captureUpdate(author, eventName, () => author.getText("shared").insert(0, "delete-me"));
    try {
      const seeded = await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(base) });
      assert.equal(seeded.status, 200);
      assert.equal(seeded.body.cursor, 1);
      const beforeDelete = await request(running.endpoint, "/v1/yjs/state-vector", { document });

      applyUpdate(deleter, base);
      const sourceBeforeDelete = Y.encodeStateVector(deleter);
      const deletion = captureUpdate(deleter, eventName, () => deleter.getText("shared").delete(0, deleter.getText("shared").length));
      assert.deepEqual([...Y.encodeStateVector(deleter)], [...sourceBeforeDelete], `${format} deletion must not be inferred from a clock change`);

      const first = await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(deletion) });
      assert.equal(first.status, 200);
      assert.equal(first.body.applied, true, `${format} delete set must advance the durable cursor`);
      assert.equal(first.body.cursor, 2);
      const afterDelete = await request(running.endpoint, "/v1/yjs/state-vector", { document });
      assert.deepEqual(afterDelete.body.stateVector, beforeDelete.body.stateVector, `${format} deletion leaves the state vector unchanged`);
      const beforeDuplicate = await request(running.endpoint, "/v1/yjs/snapshot", { document });
      const restored = new Y.Doc();
      applyUpdate(restored, fromBase64(beforeDuplicate.body.update));
      assert.equal(restored.getText("shared").toString(), "");
      restored.destroy();

      const duplicate = await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(deletion) });
      assert.equal(duplicate.status, 200);
      assert.equal(duplicate.body.applied, false, `${format} duplicate delete must not persist twice`);
      assert.equal(duplicate.body.cursor, 2);
      const afterDuplicate = await request(running.endpoint, "/v1/yjs/snapshot", { document });
      assert.deepEqual(afterDuplicate.body, beforeDuplicate.body);
    } finally {
      author.destroy();
      deleter.destroy();
    }
  }
});

test("each request destroys its materialized Y.Doc after a durable operation", async (context) => {
  const running = await startStore(context);
  const document = testDocument("v1");
  const author = new Y.Doc();
  const update = captureUpdate(author, "update", () => author.getText("scoped").insert(0, "release"));
  const vector = Y.encodeStateVector(author);
  const destroy = Y.Doc.prototype.destroy;
  let destroyed = 0;
  Y.Doc.prototype.destroy = function destroyScopedDocument(...argumentsList) {
    destroyed += 1;
    return destroy.apply(this, argumentsList);
  };
  try {
    assert.equal((await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(update) })).status, 200);
    assert.equal((await request(running.endpoint, "/v1/yjs/state-vector", { document })).status, 200);
    assert.equal((await request(running.endpoint, "/v1/yjs/diff", { document, stateVector: toBase64(vector) })).status, 200);
    assert.equal((await request(running.endpoint, "/v1/yjs/snapshot", { document })).status, 200);
  } finally {
    Y.Doc.prototype.destroy = destroy;
    author.destroy();
  }
  assert.equal(destroyed, 4, "every request-scoped materialization must be released");
});

test("parallel offline writers converge and a duplicate is not persisted twice", async (context) => {
  const running = await startStore(context, undefined, { maxConcurrentRequests: 16 });
  const document = testDocument("v1");
  const seed = new Y.Doc();
  const base = captureUpdate(seed, "update", () => seed.getText("shared").insert(0, "base"));
  await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(base) });
  const updates = Array.from({ length: 16 }, (_, index) => {
    const writer = new Y.Doc();
    Y.applyUpdate(writer, base);
    return captureUpdate(writer, "update", () => writer.getText("shared").insert(0, `[${index}]`));
  });
  const results = await Promise.all(updates.map((update) => request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(update) })));
  assert.ok(results.every((result) => result.status === 200 && result.body.applied));
  const duplicate = await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(updates[0]) });
  assert.equal(duplicate.status, 200);
  assert.equal(duplicate.body.applied, false);
  assert.equal(duplicate.body.cursor, 17);

  const snapshot = await request(running.endpoint, "/v1/yjs/snapshot", { document });
  const actual = new Y.Doc();
  Y.applyUpdate(actual, fromBase64(snapshot.body.update));
  const expected = new Y.Doc();
  Y.applyUpdate(expected, base);
  for (const update of updates) {
    Y.applyUpdate(expected, update);
  }
  assert.deepEqual(documentView(actual), documentView(expected));
});

test("invalid, oversized, unauthorized, and corrupt data never replaces the last snapshot", async (context) => {
  const running = await startStore(context, undefined, { maxUpdateBytes: 128, maxSnapshotBytes: 1024, maxStateVectorBytes: 128, maxMergeUpdates: 4 });
  const document = testDocument("v1");
  const author = new Y.Doc();
  const update = captureUpdate(author, "update", () => author.getText("safe").insert(0, "state"));
  assert.ok(update.length <= 128);
  await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(update) });
  const before = await request(running.endpoint, "/v1/yjs/snapshot", { document });

  const malformed = await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(new Uint8Array([255, 255, 255])) });
  assert.equal(malformed.status, 400);
  assert.equal(malformed.body.code, "invalid_update");
  const oversized = await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(new Uint8Array(129)) });
  assert.equal(oversized.status, 413);
  const unauthenticated = await request(running.endpoint, "/v1/yjs/snapshot", { document }, "wrong-token");
  assert.equal(unauthenticated.status, 401);
  const afterRejected = await request(running.endpoint, "/v1/yjs/snapshot", { document });
  assert.deepEqual(afterRejected.body, before.body);

  await running.close();
  const persisted = (await readdir(running.dataDir)).find((name) => name.endsWith(".json"));
  assert.ok(persisted);
  await writeFile(join(running.dataDir, persisted), "{}", { mode: 0o600 });
  const restarted = await startStore(context, running.dataDir, { maxUpdateBytes: 128, maxSnapshotBytes: 1024, maxStateVectorBytes: 128, maxMergeUpdates: 4 });
  const corrupt = await request(restarted.endpoint, "/v1/yjs/snapshot", { document });
  assert.equal(corrupt.status, 500);
  assert.equal(corrupt.body.code, "corrupt_store");
});

test("deterministic malformed-update fuzzing preserves the last durable snapshot on rejection", async (context) => {
  const running = await startStore(context);
  const document = testDocument("v1");
  const author = new Y.Doc();
  const update = captureUpdate(author, "update", () => author.getText("fuzz").insert(0, "seed"));
  await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(update) });
  let previous = (await request(running.endpoint, "/v1/yjs/snapshot", { document })).body;
  let state = 0x9e3779b9;
  for (let index = 0; index < 256; index++) {
    const candidate = new Uint8Array(update);
    for (let offset = 0; offset < candidate.length; offset++) {
      state ^= state << 13;
      state ^= state >>> 17;
      state ^= state << 5;
      if ((state & 7) === 0) {
        candidate[offset] ^= state & 255;
      }
    }
    const result = await request(running.endpoint, "/v1/yjs/apply", { document, update: toBase64(candidate) });
    const current = (await request(running.endpoint, "/v1/yjs/snapshot", { document })).body;
    if (result.status !== 200) {
      assert.deepEqual(current, previous, `rejected mutation ${index} changed persistence`);
    }
    previous = current;
  }
});

function testDocument(format) {
  return { tenant: "tenant-a", room: "notes", epoch: "7", schema: "prosemirror-v1", format };
}

async function startStore(context, existingDataDir, overrides = {}) {
  const dataDir = existingDataDir ?? await mkdtemp(join(tmpdir(), "darkinno-yjs-store-"));
  const config = await loadConfig({
    YJS_STORE_DATA_DIR: dataDir,
    YJS_STORE_TOKEN: token,
    YJS_STORE_PORT: "0",
    YJS_STORE_MAX_UPDATE_BYTES: `${overrides.maxUpdateBytes ?? 4096}`,
    YJS_STORE_MAX_STATE_VECTOR_BYTES: `${overrides.maxStateVectorBytes ?? 1024}`,
    YJS_STORE_MAX_SNAPSHOT_BYTES: `${overrides.maxSnapshotBytes ?? 32768}`,
    YJS_STORE_MAX_MERGE_UPDATES: `${overrides.maxMergeUpdates ?? 32}`,
    YJS_STORE_MAX_CONCURRENT_REQUESTS: `${overrides.maxConcurrentRequests ?? 4}`,
    YJS_STORE_REQUEST_TIMEOUT_MS: `${overrides.requestTimeoutMillis ?? 10000}`,
  });
  const service = createYJSStoreServer(config);
  const endpoint = await service.listen();
  let closed = false;
  const close = async () => {
    if (!closed) {
      closed = true;
      await service.close();
    }
  };
  context.after(close);
  return { dataDir, endpoint, service, close };
}

function holdJSONRequest(endpoint, path, value) {
  const payload = Buffer.from(JSON.stringify(value));
  const request = httpRequest(new URL(`${endpoint}${path}`), {
    method: "POST",
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
      "content-length": `${payload.length}`,
    },
  });
  const result = new Promise((resolveResult, rejectResult) => {
    request.once("error", rejectResult);
    request.once("response", (response) => {
      const chunks = [];
      response.on("data", (chunk) => chunks.push(chunk));
      response.once("error", rejectResult);
      response.once("end", () => {
        try {
          const body = Buffer.concat(chunks);
          resolveResult({ status: response.statusCode, body: body.length === 0 ? undefined : JSON.parse(body.toString("utf8")) });
        } catch (error) {
          rejectResult(error);
        }
      });
    });
  });
  const boundary = Math.max(1, Math.floor(payload.length / 2));
  request.write(payload.subarray(0, boundary));
  let finished = false;
  return {
    result,
    finish() {
      if (!finished && !request.destroyed) {
        finished = true;
        request.end(payload.subarray(boundary));
      }
    },
  };
}

async function request(endpoint, path, body, requestToken = token) {
  const response = await fetch(`${endpoint}${path}`, {
    method: "POST",
    headers: { authorization: `Bearer ${requestToken}`, "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  return { status: response.status, body: await response.json() };
}

function captureUpdate(document, event, operation) {
  let update;
  document.once(event, (candidate) => { update = candidate; });
  operation();
  assert.ok(update instanceof Uint8Array && update.length > 0);
  return update;
}

function toBase64(value) {
  return Buffer.from(value).toString("base64");
}

function fromBase64(value) {
  return new Uint8Array(Buffer.from(value, "base64"));
}

function documentView(document) {
  const metadata = document.getMap("metadata");
  return {
    title: document.getText("title").toString(),
    metadata: metadata.size === 0 ? undefined : metadata.toJSON(),
  };
}
