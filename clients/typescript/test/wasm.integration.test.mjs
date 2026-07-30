import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createServer } from "node:http";
import { join } from "node:path";
import test, { after } from "node:test";
import { pathToFileURL } from "node:url";

import { decodeFrame } from "../dist/frame.js";
import { bindRGAPlainText } from "../dist/bindings.js";
import {
  CRDTRuntimeError,
  RGA_PROTOCOL_RUN_V2,
  RGA_PROTOCOL_V1,
  RGA_WASM_GLOBAL,
} from "../dist/wasm.js";

const wasmDirectory = process.env.CRDT_WASM_DIR;
if (typeof wasmDirectory !== "string" || wasmDirectory === "") {
  throw new Error("CRDT_WASM_DIR must point to artifacts built by make wasm");
}
const artifactProtocol = protocolForArtifact(process.env.CRDT_RGA_PROTOCOL);
const incompatibleProtocol = artifactProtocol === RGA_PROTOCOL_V1 ? RGA_PROTOCOL_RUN_V2 : RGA_PROTOCOL_V1;

await import(pathToFileURL(join(wasmDirectory, "wasm_exec.js")).href);
const assets = await startAssetServer(wasmDirectory);
const wasmModule = await import("../dist/wasm.js");
const loaderRuntime = await wasmModule.initRGAWasm(
  artifactProtocol === RGA_PROTOCOL_RUN_V2
    ? { wasmURL: `${assets.url}/crdt-rga.wasm` }
    : { wasmURL: `${assets.url}/crdt-rga.wasm`, expectedProtocol: artifactProtocol },
);
const rawAPI = globalThis[RGA_WASM_GLOBAL];
after(async () => {
  await new Promise((resolve, reject) => {
    assets.server.close((error) => (error === undefined ? resolve() : reject(error)));
  });
});

test("TypeScript loader starts the real Go Wasm module over application/wasm", () => {
  const document = loaderRuntime.create("loader");
  const frame = document.insert(0, "loader local merge");
  assert.equal(decodeFrame(frame).typeID, artifactProtocol.deltaTypeID);
  assert.equal(document.text(), "loader local merge");
  assert.equal(document.close(), true);
});

test("plain-text binding exchanges actual Go Wasm RGA frames without echoing remote updates", () => {
  const aliceDocument = loaderRuntime.create("binding-alice");
  const bobDocument = loaderRuntime.create("binding-bob");
  const aliceEditor = new TestTextPort("Hello");
  const bobEditor = new TestTextPort("");
  const aliceFrames = [];
  const bobFrames = [];
  const alice = bindRGAPlainText(aliceDocument, aliceEditor, {
    initialContent: "editor",
    onLocalFrame: (frame) => aliceFrames.push(frame),
  });
  const bob = bindRGAPlainText(bobDocument, bobEditor, {
    onLocalFrame: (frame) => bobFrames.push(frame),
  });
  for (const frame of aliceFrames) {
    bob.applyRemote(frame);
  }
  assert.equal(bobEditor.readText(), "Hello");
  assert.equal(bobFrames.length, 0);

  bobEditor.userWrite("Hello collaborative world");
  for (const frame of bobFrames) {
    alice.applyRemote(frame);
  }
  assert.equal(aliceEditor.readText(), "Hello collaborative world");
  assert.equal(aliceFrames.length, 1);
  assert.equal(alice.destroy(), true);
  assert.equal(bob.destroy(), true);
  assert.equal(aliceDocument.close(), true);
  assert.equal(bobDocument.close(), true);
});

test("TypeScript loader rejects a Wasm artifact whose expected protocol does not match", async () => {
  await assert.rejects(
    () => wasmModule.initRGAWasm({ wasmURL: `${assets.url}/crdt-rga.wasm`, expectedProtocol: incompatibleProtocol }),
    (error) => error instanceof CRDTRuntimeError && error.code === "protocol_mismatch",
  );
  await assert.rejects(
    () =>
      wasmModule.initRGAWasm({
        wasmURL: `${assets.url}/crdt-rga.wasm`,
        expectedProtocol: { stateTypeID: 99n, deltaTypeID: 100n, semanticsVersion: 1n },
      }),
    (error) => error instanceof CRDTRuntimeError && error.code === "protocol_mismatch",
  );
});

test("TypeScript wrapper bounds persistence input before entering Go Wasm", () => {
  const document = loaderRuntime.create("snapshot-bounds");
  const snapshot = document.snapshot();
  assertRuntimeError(
    () => document.insert(0, "x".repeat(loaderRuntime.protocol.maxLocalEditBytes + 1)),
    "resource_limit",
  );
  assertRuntimeError(
    () => loaderRuntime.create("x".repeat(loaderRuntime.protocol.maxStringBytes + 1)),
    "resource_limit",
  );
  assertRuntimeError(
    () => loaderRuntime.restore({ ...snapshot, state: new Uint8Array(loaderRuntime.protocol.maxFrameBytes + 1) }),
    "invalid_snapshot",
  );
  assertRuntimeError(
    () =>
      loaderRuntime.restore({
        ...snapshot,
        clock: { ...snapshot.clock, replicaID: "x".repeat(loaderRuntime.protocol.maxStringBytes + 1) },
      }),
    "invalid_snapshot",
  );
  assertRuntimeError(
    () => loaderRuntime.restore({ ...snapshot, frontier: "not-an-array" }),
    "invalid_snapshot",
  );
  assert.equal(document.close(), true);
});

test("actual Go Wasm emits the negotiated RGA frames accepted by the TypeScript decoder", () => {
  const protocol = unwrap(rawAPI.protocol());
  assert.equal(protocol.stateTypeID, artifactProtocol.stateTypeID.toString());
  assert.equal(protocol.deltaTypeID, artifactProtocol.deltaTypeID.toString());
  assert.equal(protocol.semanticsVersion, artifactProtocol.semanticsVersion.toString());
  assert.equal(protocol.maxFrameBytes, 1 << 20);
  assert.equal(protocol.maxTags, 100_000);
  assert.equal(protocol.maxStringBytes, 64 << 10);
  assert.equal(protocol.maxLocalEditBytes, 64 << 10);
  assert.equal(protocol.maxLocalEditRunes, 16 << 10);

  const alice = create("alice");
  const bob = create("bob");
  const frame = copiedBytes(unwrap(rawAPI.insert(alice, 0, "hello")));
  const decoded = decodeFrame(frame);
  assert.equal(decoded.typeID, artifactProtocol.deltaTypeID);
  assert.equal(decoded.codecID.length, 0);
  assert.ok(decoded.payload.length > 0);

  assert.equal(unwrap(rawAPI.applyDelta(bob, frame)), undefined);
  assert.equal(unwrap(rawAPI.text(bob)), "hello");
});

test("actual Go Wasm converges three clients under duplicate, reordered, and snapshot-recovery delivery", () => {
  const alice = create("sim-alice");
  const bob = create("sim-bob");
  const carol = create("sim-carol");
  const base = copiedBytes(unwrap(rawAPI.insert(alice, 0, "Draft")));
  apply(bob, base);
  apply(carol, base);
  const bobEdit = copiedBytes(unwrap(rawAPI.insert(bob, 5, " for review")));
  const carolEdit = copiedBytes(unwrap(rawAPI.insert(carol, 5, " collaboratively")));
  apply(alice, carolEdit);
  apply(alice, bobEdit);
  const deleteEdit = copiedBytes(unwrap(rawAPI.delete(alice, 1, 2)));

  const changes = [base, bobEdit, carolEdit, deleteEdit];
  for (const [index, handle] of [alice, bob, carol].entries()) {
    deliverDuplicatedAndShuffled(handle, changes, 20_260_729 + index);
  }
  const expected = unwrap(rawAPI.text(alice));
  assert.equal(unwrap(rawAPI.text(bob)), expected);
  assert.equal(unwrap(rawAPI.text(carol)), expected);
  assert.equal(unwrap(rawAPI.pendingCount(bob)), 0);
  assert.equal(unwrap(rawAPI.pendingCount(carol)), 0);

  const snapshot = unwrap(rawAPI.snapshot(bob));
  const recovered = documentHandle(unwrap(rawAPI.restore(snapshot)));
  assert.equal(unwrap(rawAPI.text(recovered)), expected);
  assert.equal(unwrap(rawAPI.drop(recovered)), true);

  const before = unwrap(rawAPI.text(bob));
  const malformed = base.slice();
  malformed[5] ^= 1;
  const failed = rawAPI.applyDelta(bob, malformed);
  assert.equal(failed.ok, false);
  assert.equal(failed.error, "invalid_frame");
  assert.equal(unwrap(rawAPI.text(bob)), before);
});

function create(replicaID) {
  return documentHandle(unwrap(rawAPI.create(replicaID)));
}

function apply(handle, frame) {
  assert.equal(unwrap(rawAPI.applyDelta(handle, frame)), undefined);
}

function deliverDuplicatedAndShuffled(handle, changes, seed) {
  const frames = changes.flatMap((frame) => [frame, frame]);
  let state = seed >>> 0;
  for (let index = frames.length - 1; index > 0; index -= 1) {
    state = nextRandom(state);
    const swap = state % (index + 1);
    [frames[index], frames[swap]] = [frames[swap], frames[index]];
  }
  for (const frame of frames) {
    apply(handle, frame);
  }
}

async function startAssetServer(directory) {
  const wasm = await readFile(join(directory, "crdt-rga.wasm"));
  const server = createServer((request, response) => {
    if (request.url !== "/crdt-rga.wasm") {
      response.writeHead(404).end();
      return;
    }
    response.writeHead(200, {
      "content-length": wasm.length,
      "content-type": "application/wasm",
    });
    response.end(wasm);
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error("Wasm test server did not expose a TCP port");
  }
  return { server, url: `http://127.0.0.1:${address.port}` };
}

function unwrap(result) {
  assert.equal(result.ok, true, `Wasm operation failed: ${result.error}`);
  return result.value;
}

function assertRuntimeError(operation, code) {
  assert.throws(operation, (error) => error instanceof CRDTRuntimeError && error.code === code);
}

function copiedBytes(value) {
  assert.ok(value instanceof Uint8Array);
  return value.slice();
}

function documentHandle(value) {
  assert.equal(typeof value, "number");
  assert.ok(Number.isSafeInteger(value) && value > 0);
  return value;
}

function nextRandom(value) {
  return (Math.imul(value, 1_664_525) + 1_013_904_223) >>> 0;
}

function protocolForArtifact(value) {
  if (value === undefined || value === "run-v2") {
    return RGA_PROTOCOL_RUN_V2;
  }
  if (value === "v1") {
    return RGA_PROTOCOL_V1;
  }
  throw new Error("CRDT_RGA_PROTOCOL must be run-v2 or v1");
}

class TestTextPort {
  #listeners = new Set();

  constructor(value) {
    this.value = value;
  }

  readText() {
    return this.value;
  }

  writeText(value) {
    this.value = value;
    for (const listener of [...this.#listeners]) {
      listener();
    }
  }

  observeText(listener) {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  }

  userWrite(value) {
    this.writeText(value);
  }
}
