import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createServer } from "node:http";
import { join } from "node:path";
import test, { after } from "node:test";
import { pathToFileURL } from "node:url";

import { MemoryRGAWasmBrowserPersistence, openRGAWasmBrowserDocument } from "../dist/browser.js";
import { RGA_PROTOCOL_PACKED_V3, RGA_PROTOCOL_PACKED_V3_V2, RGA_PROTOCOL_RUN_V2, RGA_PROTOCOL_V1, initRGAWasm } from "../dist/wasm.js";

const wasmDirectory = process.env.CRDT_WASM_DIR;
if (typeof wasmDirectory !== "string" || wasmDirectory === "") {
  throw new Error("CRDT_WASM_DIR must point to artifacts built by make wasm");
}
const expectedProtocol = protocolForArtifact(process.env.CRDT_RGA_PROTOCOL);

await import(pathToFileURL(join(wasmDirectory, "wasm_exec.js")).href);
const assets = await startAssetServer(wasmDirectory);
const runtime = await initRGAWasm({ wasmURL: `${assets.url}/crdt-rga.wasm`, expectedProtocol });
after(async () => {
  await new Promise((resolve, reject) => {
    assets.server.close((error) => (error === undefined ? resolve() : reject(error)));
  });
});

test("Wasm RGA browser persistence restores a durable outbox, retries after receipt, and compacts", async () => {
  const persistence = new MemoryRGAWasmBrowserPersistence();
  const limits = { compactAfterUpdates: 2, compactAfterBytes: 256, maxUpdates: 100, maxBytes: 1 << 20 };
  const initial = await openRGAWasmBrowserDocument({
    documentID: "offline-rga", replicaID: "offline-alice", runtime, persistence, persistenceLimits: limits,
  });
  initial.insert(0, "offline first");
  await initial.flush();
  assert.equal(initial.pendingOutbox, 1);
  await initial.close();

  const delivered = [];
  const resumed = await openRGAWasmBrowserDocument({
    documentID: "offline-rga", replicaID: "offline-alice", runtime, persistence, persistenceLimits: limits,
    transport: {
      send(frame) { delivered.push(frame.slice()); },
      subscribe() { return () => {}; },
    },
  });
  assert.equal(resumed.text(), "offline first");
  await resumed.flush();
  assert.equal(resumed.pendingOutbox, 0);
  resumed.insert(resumed.text().length, " second");
  await resumed.flush();
  const stored = await persistence.load(JSON.stringify(["offline-rga", "offline-alice"]));
  assert.ok(stored?.snapshot);
  assert.equal(stored.updates.length, 0);

  const receiver = await openRGAWasmBrowserDocument({
    documentID: "offline-rga", replicaID: "offline-bob", runtime, persistence: new MemoryRGAWasmBrowserPersistence(),
  });
  for (const frame of delivered) receiver.applyDelta(frame);
  await receiver.flush();
  assert.equal(receiver.text(), "offline first second");
  await resumed.close();
  await receiver.close();
});

test("Wasm RGA live multi-tab delivery keeps the durable outbox pending", async () => {
  const hub = new LiveHub();
  const left = await openRGAWasmBrowserDocument({
    documentID: "live-rga-tabs",
    replicaID: "alice-tab",
    runtime,
    persistence: new MemoryRGAWasmBrowserPersistence(),
    liveTransport: hub.createTransport(),
  });
  const right = await openRGAWasmBrowserDocument({
    documentID: "live-rga-tabs",
    replicaID: "bob-tab",
    runtime,
    persistence: new MemoryRGAWasmBrowserPersistence(),
    liveTransport: hub.createTransport(),
  });

  left.insert(0, "live across tabs");
  await left.flush();
  await waitFor(() => right.text() === "live across tabs");
  await right.flush();

  assert.equal(left.pendingOutbox, 1);
  assert.equal(right.pendingOutbox, 0);
  await left.close();
  await right.close();
});

test("three offline Wasm RGA browser actors converge after reverse duplicate delivery and recovery", async () => {
  const replicaIDs = ["alice", "bob", "carol"];
  const persistences = replicaIDs.map(() => new MemoryRGAWasmBrowserPersistence());
  const clients = await Promise.all(replicaIDs.map((replicaID, index) => openRGAWasmBrowserDocument({
    documentID: "offline-three", replicaID, runtime, persistence: persistences[index],
  })));
  const updates = [];
  for (const client of clients) client.onUpdate((event) => { if (event.local) updates.push(event.encoded); });
  for (let turn = 0; turn < 48; turn += 1) {
    const client = clients[turn % clients.length];
    client.insert(client.text().length, `${turn % 10}`);
  }
  await Promise.all(clients.map((client) => client.flush()));
  for (let index = updates.length - 1; index >= 0; index -= 1) {
    for (const client of clients) {
      client.applyDelta(updates[index]);
      if ((index % 5) === 0) client.applyDelta(updates[index]);
    }
  }
  await Promise.all(clients.map((client) => client.flush()));
  const expected = clients[0].text();
  for (const client of clients.slice(1)) {
    assert.equal(client.text(), expected);
    assert.equal(client.pendingCount(), 0);
  }
  for (const client of clients) await client.close();
  const recovered = await Promise.all(replicaIDs.map((replicaID, index) => openRGAWasmBrowserDocument({
    documentID: "offline-three", replicaID, runtime, persistence: persistences[index],
  })));
  for (const client of recovered) {
    assert.equal(client.text(), expected);
    await client.close();
  }
});

test("packed-v3 Wasm restores one bounded 64Ki-rune initial snapshot", (t) => {
  if (expectedProtocol !== RGA_PROTOCOL_PACKED_V3 && expectedProtocol !== RGA_PROTOCOL_PACKED_V3_V2) {
    t.skip("requires the separately built packed-v3 artifact");
    return;
  }
  const source = runtime.create("packed-initial-source");
  const initialRunes = 64 << 10;
  const chunkRunes = 12 << 10;
  const value = "协".repeat(initialRunes);
  for (let offset = 0; offset < initialRunes; offset += chunkRunes) {
    source.insert(offset, value.slice(offset, Math.min(offset + chunkRunes, initialRunes)));
  }
  const snapshot = source.snapshot();
  const maxBytes = expectedProtocol === RGA_PROTOCOL_PACKED_V3_V2 ? 32 << 10 : 256 << 10;
  assert.ok(snapshot.state.byteLength < maxBytes, `packed state is ${snapshot.state.byteLength} bytes`);
  const receiver = runtime.restore(snapshot);
  assert.equal(receiver.text(), value);
  assert.equal(source.close(), true);
  assert.equal(receiver.close(), true);
});

test("an RGA receipt never checkpoints a later local frame whose append has failed", async () => {
  const persistence = new FailSecondRGAAppendPersistence();
  const document = await openRGAWasmBrowserDocument({
    documentID: "receipt-prefix", replicaID: "receipt-actor", runtime, persistence,
    persistenceLimits: { compactAfterUpdates: 1, compactAfterBytes: 1, maxUpdates: 10, maxBytes: 1 << 20 },
    transport: { send() {}, subscribe() { return () => {}; } },
  });
  document.insert(0, "first");
  document.insert(5, " second");
  await assert.rejects(() => document.flush(), /persistence_failed/);
  assert.equal(persistence.compactedSnapshot, undefined);
  document.disconnect();
});

class FailSecondRGAAppendPersistence {
  appendCalls = 0;
  compactedSnapshot;

  async load() {
    return undefined;
  }

  async append() {
    this.appendCalls += 1;
    if (this.appendCalls === 2) throw new Error("persistence_failed");
    return { sequence: 1, updateCount: 1, logBytes: 64 };
  }

  async acknowledge() {}

  async compact(_key, snapshot) {
    this.compactedSnapshot = snapshot;
  }
}

class LiveHub {
  #receivers = new Map();

  createTransport() {
    const hub = this;
    const source = Symbol("live-tab");
    return {
      publish(encoded) {
        const copy = encoded.slice();
        queueMicrotask(() => {
          for (const [target, receiver] of hub.#receivers) {
            if (target !== source) receiver(copy.slice());
          }
        });
      },
      subscribe(receiver) {
        hub.#receivers.set(source, receiver);
        return () => hub.#receivers.delete(source);
      },
    };
  }
}

async function waitFor(predicate) {
  const deadline = Date.now() + 1_000;
  while (!predicate()) {
    if (Date.now() >= deadline) throw new Error("live Wasm RGA multi-tab delivery timed out");
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

function protocolForArtifact(value) {
  if (value === undefined || value === "run-v2") return RGA_PROTOCOL_RUN_V2;
  if (value === "v1") return RGA_PROTOCOL_V1;
  if (value === "packed-v3") return RGA_PROTOCOL_PACKED_V3;
  if (value === "packed-v3-v2") return RGA_PROTOCOL_PACKED_V3_V2;
  throw new Error("CRDT_RGA_PROTOCOL must be run-v2, packed-v3, packed-v3-v2, or v1");
}

async function startAssetServer(directory) {
  const wasm = await readFile(join(directory, "crdt-rga.wasm"));
  const server = createServer((request, response) => {
    if (request.url !== "/crdt-rga.wasm") {
      response.writeHead(404).end();
      return;
    }
    response.writeHead(200, { "content-length": wasm.length, "content-type": "application/wasm" });
    response.end(wasm);
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  if (address === null || typeof address === "string") throw new Error("Wasm test server did not expose a TCP port");
  return { server, url: `http://127.0.0.1:${address.port}` };
}
