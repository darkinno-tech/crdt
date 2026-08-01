import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createServer } from "node:http";
import { join } from "node:path";
import test, { after } from "node:test";
import { pathToFileURL } from "node:url";

import { BlockNoteEditor } from "@blocknote/core";

import { decodeFrame } from "../dist/frame.js";
import { bindBlockNoteRichText, bindCodeMirrorPlainText, bindQuillRichText, bindRGAPlainText } from "../dist/bindings.js";
import {
  CRDTRuntimeError,
  RICH_TEXT_PROTOCOL,
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
const richTextRuntime = await wasmModule.initRichTextWasm({
  wasmURL: `${assets.url}/crdt-rga.wasm`,
  expectedRGAProtocol: artifactProtocol,
});
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

test("TypeScript wrapper atomically replaces text", () => {
	const document = loaderRuntime.create("relative-position");
	document.insert(0, "abc");
	const frame = document.replace(1, 1, "XY");
	assert.equal(decodeFrame(frame).typeID, artifactProtocol.deltaTypeID);
	assert.equal(document.text(), "aXYc");
	assert.equal(document.close(), true);
});

test("Position/Tag anchors survive a concurrent insert and snapshot recovery", () => {
  const alice = loaderRuntime.create("anchor-alice");
  const bob = loaderRuntime.create("anchor-bob");
  const base = alice.insert(0, "ab🙂cd");
  bob.applyDelta(base);
  const anchor = alice.anchorAt(3);
  assert.equal(anchor.association, "before");
  assert.ok(anchor.position);
  assert.equal(typeof anchor.position.wallTime, "bigint");

  const concurrent = bob.insert(3, "X");
  alice.applyDelta(concurrent);
  assert.equal(alice.resolveAnchor(anchor), 4);
  const restored = loaderRuntime.restore(alice.snapshot());
  assert.equal(restored.resolveAnchor(anchor), 4);
  assert.deepEqual(alice.anchorAt(Array.from(alice.text()).length), { association: "after" });
  assertRuntimeError(
    () => alice.resolveAnchor({ association: "before", position: { replicaID: "", wallTime: 1n, logical: 0n } }),
    "invalid_anchor",
  );
  assert.equal(alice.close(), true);
  assert.equal(bob.close(), true);
  assert.equal(restored.close(), true);
});

test("plain-text binding preserves a UTF-16 selection through a real remote RGA merge", () => {
  const aliceDocument = loaderRuntime.create("selection-alice");
  const bobDocument = loaderRuntime.create("selection-bob");
  const port = new SelectionTextPort("a🙂bc", { anchor: 3, head: 3 });
  const frames = [];
  const alice = bindRGAPlainText(aliceDocument, port, {
    initialContent: "editor",
    onLocalFrame: (frame) => frames.push(frame),
  });
  bobDocument.applyDelta(frames[0]);
  const before = alice.captureSelection();
  assert.ok(before);
  const remote = bobDocument.insert(0, "X");
  alice.applyRemote(remote);
  const expectedRune = aliceDocument.resolveAnchor(before.anchor);
  assert.equal(port.selection.anchor, utf16OffsetAtRune(aliceDocument.text(), expectedRune));
  assert.equal(port.selection.head, port.selection.anchor);
  assert.equal(alice.destroy(), true);
  assert.equal(aliceDocument.close(), true);
  assert.equal(bobDocument.close(), true);
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

test("Quill rich-text binding exchanges actual Go Wasm rich-text frames without losing approved formats", () => {
  const aliceDocument = richTextRuntime.create("rich-binding-alice");
  const bobDocument = richTextRuntime.create("rich-binding-bob");
  const aliceEditor = new TestRichQuillPort({ ops: [{ insert: "Hello\n" }] });
  const bobEditor = new TestRichQuillPort({ ops: [{ insert: "\n" }] });
  const aliceFrames = [];
  const bobFrames = [];
  const alice = bindQuillRichText(aliceDocument, aliceEditor, {
    onLocalFrame: (frame) => aliceFrames.push(frame),
    attributes: testRichTextAttributes,
  });
  bobDocument.applyDelta(aliceFrames[0]);
  const bob = bindQuillRichText(bobDocument, bobEditor, {
    onLocalFrame: (frame) => bobFrames.push(frame),
    attributes: testRichTextAttributes,
    initialContent: "document",
  });
  assert.deepEqual(bobEditor.getContents(), { ops: [{ insert: "Hello\n" }] });
  assert.equal(bobFrames.length, 0);

  aliceEditor.userDelta({
    ops: [
      { retain: 5, attributes: { bold: true } },
      { insert: " world", attributes: { italic: true } },
    ],
  });
  const aliceEdit = aliceFrames.at(-1);
  assert.ok(aliceEdit instanceof Uint8Array);
  assert.equal(decodeFrame(aliceEdit).typeID, RICH_TEXT_PROTOCOL.deltaTypeID);
  bob.applyRemote(aliceEdit);
  assert.deepEqual(bobEditor.getContents(), {
    ops: [
      { insert: "Hello", attributes: { bold: true } },
      { insert: " world", attributes: { italic: true } },
      { insert: "\n" },
    ],
  });
  assert.equal(bobFrames.length, 0);

  aliceEditor.userDelta({ ops: [{ retain: 11 }, { retain: 1, attributes: { header: 2 } }] });
  const blockEdit = aliceFrames.at(-1);
  assert.ok(blockEdit instanceof Uint8Array);
  bob.applyRemote(blockEdit);
  assert.deepEqual(bobEditor.getContents(), {
    ops: [
      { insert: "Hello", attributes: { bold: true } },
      { insert: " world", attributes: { italic: true } },
      { insert: "\n", attributes: { header: 2 } },
    ],
  });

  const snapshot = aliceDocument.snapshot();
  const recovered = richTextRuntime.restore(snapshot);
  assert.deepEqual(recovered.spans(), aliceDocument.spans());
  assert.equal(recovered.close(), true);
  assert.equal(alice.destroy(), true);
  assert.equal(bob.destroy(), true);
  assert.equal(aliceDocument.close(), true);
  assert.equal(bobDocument.close(), true);
});

test("BlockNote default text blocks exchange actual Go Wasm rich-text frames without a remote echo", () => {
  const aliceDocument = richTextRuntime.create("blocknote-binding-alice");
  const bobDocument = richTextRuntime.create("blocknote-binding-bob");
  const aliceEditor = BlockNoteEditor.create({
    initialContent: [{
      type: "heading",
      props: { level: 2, isToggleable: true },
      content: [{ type: "text", text: "Release", styles: { bold: true } }],
      children: [{ type: "checkListItem", props: { checked: true }, content: "Validate" }],
    }],
  });
  const bobEditor = BlockNoteEditor.create();
  const aliceFrames = [];
  const bobFrames = [];
  const alice = bindBlockNoteRichText(aliceDocument, aliceEditor, {
    initialContent: "editor",
    onLocalFrame: (frame) => aliceFrames.push(frame),
  });
  bobDocument.applyDelta(aliceFrames[0]);
  const bob = bindBlockNoteRichText(bobDocument, bobEditor, {
    initialContent: "document",
    onLocalFrame: (frame) => bobFrames.push(frame),
  });
  assert.equal(bobEditor.document[0].type, "heading");
  assert.equal(bobEditor.document[0].children[0].type, "checkListItem");
  assert.equal(bobFrames.length, 0);

  aliceEditor.updateBlock(aliceEditor.document[0], { content: "Release candidate" });
  const edit = aliceFrames.at(-1);
  assert.ok(edit instanceof Uint8Array);
  assert.equal(decodeFrame(edit).typeID, RICH_TEXT_PROTOCOL.deltaTypeID);
  bob.applyRemote(edit);
  bob.applyRemote(edit); // duplicate delivery must stay idempotent at the editor boundary.
  assert.equal(bobEditor.document[0].content[0].text, "Release candidate");
  assert.equal(bobEditor.document[0].children[0].props.checked, true);
  assert.equal(bobFrames.length, 0);

  const recovered = richTextRuntime.restore(aliceDocument.snapshot());
  assert.deepEqual(recovered.spans(), aliceDocument.spans());
  assert.equal(recovered.close(), true);
  assert.equal(alice.destroy(), true);
  assert.equal(bob.destroy(), true);
  assert.equal(aliceDocument.close(), true);
  assert.equal(bobDocument.close(), true);
});

test("CodeMirror-shaped binding exchanges actual Go Wasm RGA frames without echoing remote updates", () => {
  const aliceDocument = loaderRuntime.create("codemirror-binding-alice");
  const bobDocument = loaderRuntime.create("codemirror-binding-bob");
  const aliceEditor = new TestCodeMirrorPort("const draft = true;");
  const bobEditor = new TestTextPort("");
  const aliceFrames = [];
  const bobFrames = [];
  const alice = bindCodeMirrorPlainText(aliceDocument, aliceEditor, {
    initialContent: "editor",
    onLocalFrame: (frame) => aliceFrames.push(frame),
  });
  const bob = bindRGAPlainText(bobDocument, bobEditor, {
    onLocalFrame: (frame) => bobFrames.push(frame),
  });
  for (const frame of aliceFrames) {
    bob.applyRemote(frame);
  }
  assert.equal(bobEditor.readText(), "const draft = true;");
  assert.equal(bobFrames.length, 0);

  aliceEditor.userWrite("const draft = false;");
  alice.applyViewUpdate({ docChanged: true });
  const aliceEdit = aliceFrames.at(-1);
  assert.ok(aliceEdit instanceof Uint8Array);
  bob.applyRemote(aliceEdit);
  assert.equal(bobEditor.readText(), "const draft = false;");
  assert.equal(bobFrames.length, 0);

  bobEditor.userWrite("const reviewed = false;");
  const bobEdit = bobFrames.at(-1);
  assert.ok(bobEdit instanceof Uint8Array);
  alice.applyRemote(bobEdit);
  assert.equal(aliceEditor.state.doc.toString(), "const reviewed = false;");
  assert.equal(aliceFrames.length, 2);
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

class TestCodeMirrorPort {
  constructor(value) {
    this.value = value;
  }

  get state() {
    return {
      doc: {
        length: this.value.length,
        toString: () => this.value,
      },
    };
  }

  dispatch({ changes }) {
    this.value = `${this.value.slice(0, changes.from)}${changes.insert}${this.value.slice(changes.to)}`;
  }

  userWrite(value) {
    this.value = value;
  }
}

const testRichTextAttributes = {
  toDocumentChanges(attributes, operation) {
    const changes = [];
    for (const [key, value] of Object.entries(attributes)) {
      if (key === "header" && value === 2) {
        changes.push({ key: "rt.block", value: "heading:2" });
        continue;
      }
      if (key !== "bold" && key !== "italic") throw new CRDTRuntimeError("unsupported_rich_text");
      if (operation === "retain" && value === null) {
        changes.push({ key: `rt.${key}`, remove: true });
      } else if (value === true) {
        changes.push({ key: `rt.${key}`, value: "true" });
      } else {
        throw new CRDTRuntimeError("unsupported_rich_text");
      }
    }
    return changes;
  },
  toEditorAttributes(attributes, text) {
    const output = {};
    for (const [key, value] of Object.entries(attributes)) {
      if (key === "rt.bold" && value === "true") output.bold = true;
      else if (key === "rt.italic" && value === "true") output.italic = true;
      else if (key === "rt.block" && value === "heading:2" && text.endsWith("\n")) output.header = 2;
      else if (key === "rt.block" && value === "heading:2") continue;
      else throw new CRDTRuntimeError("unsupported_rich_text");
    }
    return output;
  },
};

class TestRichQuillPort {
  #listeners = new Set();

  constructor(contents) {
    this.contents = contents;
  }

  getContents() {
    return this.contents;
  }

  setContents(contents, source = "api") {
    this.contents = contents;
    for (const listener of [...this.#listeners]) listener(contents, {}, source);
  }

  on(_event, listener) {
    this.#listeners.add(listener);
  }

  off(_event, listener) {
    this.#listeners.delete(listener);
  }

  userDelta(delta) {
    for (const listener of [...this.#listeners]) listener(delta, this.contents, "user");
  }
}

class SelectionTextPort extends TestTextPort {
  constructor(value, selection) {
    super(value);
    this.selection = selection;
  }

  readSelection() {
    return this.selection;
  }

  writeSelection(selection) {
    this.selection = selection;
  }
}

function utf16OffsetAtRune(value, offset) {
  return Array.from(value).slice(0, offset).join("").length;
}
