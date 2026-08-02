import assert from "node:assert/strict";
import test, { after } from "node:test";

import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import * as decoding from "lib0/decoding";
import * as encoding from "lib0/encoding";
import { Awareness, encodeAwarenessUpdate } from "y-protocols/awareness.js";
import * as syncProtocol from "y-protocols/sync.js";
import * as Y from "yjs";
import { JSDOM } from "jsdom";

import {
  bindYjsCodeMirrorPlainText,
  observeYjsDeep,
  YjsTextBinding,
} from "../dist/yjs.js";

const limits = Object.freeze({
  maxUpdateBytes: 1 << 20,
  maxAwarenessBytes: 1 << 16,
  maxTextUTF16: 1 << 20,
  maxCursorBytes: 256,
});

const dom = new JSDOM("<!doctype html><html><body></body></html>");
dom.window.requestAnimationFrame = (callback) => setTimeout(() => callback(Date.now()), 0);
dom.window.cancelAnimationFrame = (handle) => clearTimeout(handle);
dom.window.ResizeObserver = class {
  observe() {}
  unobserve() {}
  disconnect() {}
};
const previousDOMGlobals = new Map();
for (const [key, value] of Object.entries({
  document: dom.window.document,
  window: dom.window,
  MutationObserver: dom.window.MutationObserver,
  navigator: dom.window.navigator,
  requestAnimationFrame: dom.window.requestAnimationFrame,
  cancelAnimationFrame: dom.window.cancelAnimationFrame,
  ResizeObserver: dom.window.ResizeObserver,
})) {
  previousDOMGlobals.set(key, Object.getOwnPropertyDescriptor(globalThis, key));
  Object.defineProperty(globalThis, key, { configurable: true, value, writable: true });
}
after(() => {
  for (const [key, descriptor] of previousDOMGlobals) {
    if (descriptor === undefined) {
      delete globalThis[key];
    } else {
      Object.defineProperty(globalThis, key, descriptor);
    }
  }
  dom.window.close();
});

test("native Yjs binding applies a remote Y.Text transaction as its exact CodeMirror range", () => {
  const source = new Y.Doc();
  const sourceText = source.getText("content");
  const initial = `${"a".repeat(8192)}🙂${"b".repeat(8192)}`;
  sourceText.insert(0, initial);

  const target = new Y.Doc();
  Y.applyUpdate(target, Y.encodeStateAsUpdate(source));
  const targetText = target.getText("content");
  const remoteChanges = [];
  let binding;
  const view = new EditorView({
    state: EditorState.create({
      doc: targetText.toString(),
      extensions: [EditorView.updateListener.of((update) => {
        if (!update.docChanged) return;
        for (const transaction of update.transactions) {
          transaction.changes.iterChanges((fromA, toA, _fromB, _toB, inserted) => {
            remoteChanges.push({ from: fromA, to: toA, insert: inserted.toString() });
          });
        }
        binding?.applyViewUpdate(update);
      })],
    }),
    parent: dom.window.document.body,
  });
  let localUpdates = 0;
  binding = bindYjsCodeMirrorPlainText(target, targetText, view, {
    ...limits,
    onLocalUpdate(update) {
      localUpdates += 1;
      Y.applyUpdate(source, update);
    },
  });
  source.on("update", (update) => binding.applyRemoteUpdate(update));

  const offset = 8192;
  source.transact(() => {
    sourceText.delete(offset, 2);
    sourceText.insert(offset, "Z");
  });
  assert.equal(targetText.toString(), `${"a".repeat(8192)}Z${"b".repeat(8192)}`);
  assert.equal(view.state.doc.toString(), targetText.toString());
  assert.deepEqual(remoteChanges, [{ from: offset, to: offset + 2, insert: "Z" }]);
  assert.equal(localUpdates, 0, "remote updates must not echo into the local transport outbox");

  remoteChanges.length = 0;
  source.transact(() => {
    sourceText.insert(64, "Q");
    sourceText.delete(128, 1);
  });
  assert.equal(view.state.doc.toString(), targetText.toString());
  assert.equal(remoteChanges.length, 2, "one Yjs transaction may contain multiple incremental editor ranges");
  assert.equal(remoteChanges.some((change) => change.from === 0 && change.to >= initial.length), false);

  remoteChanges.length = 0;
  view.dispatch({ changes: { from: 100, to: 101, insert: "X" } });
  assert.equal(targetText.toString(), sourceText.toString());
  assert.equal(localUpdates, 1);
  assert.deepEqual(remoteChanges, [{ from: 100, to: 101, insert: "X" }]);

  assert.equal(binding.destroy(), true);
  view.destroy();
  source.destroy();
  target.destroy();
});

test("Yjs V2 updates and state-vector differences remain native and format-pinned", () => {
  const author = new Y.Doc();
  const replica = new Y.Doc();
  const authorText = author.getText("content");
  const replicaText = replica.getText("content");
  const binding = new YjsTextBinding(replica, replicaText, { ...limits, updateFormat: "v2" });
  author.on("updateV2", (update) => binding.applyRemoteUpdate(update));

  authorText.insert(0, "native v2");
  assert.equal(replicaText.toString(), "native v2");
  const stateVector = binding.encodeStateVector();
  authorText.insert(authorText.length, " update");
  const incremental = binding.encodeStateAsUpdate(stateVector);
  const later = new Y.Doc();
  Y.applyUpdateV2(later, incremental);
  assert.equal(later.getText("content").toString(), "", "a state-vector diff needs its base state");
  Y.applyUpdateV2(later, binding.encodeStateAsUpdate());
  assert.equal(later.getText("content").toString(), "native v2 update");

  assert.throws(
    () => binding.applyRemoteUpdate(Y.encodeStateAsUpdate(author)),
    (error) => error?.code === "invalid_update",
  );
  assert.equal(binding.destroy(), true);
  author.destroy();
  replica.destroy();
  later.destroy();
});

test("a plain-text binding stops instead of flattening a formatted remote Y.Text", () => {
  const source = new Y.Doc();
  const target = new Y.Doc();
  const sourceText = source.getText("content");
  sourceText.insert(0, "safe");
  Y.applyUpdate(target, Y.encodeStateAsUpdate(source));
  const targetText = target.getText("content");
  const errors = [];
  const changes = [];
  const binding = new YjsTextBinding(target, targetText, {
    ...limits,
    onTextChanges(delta) {
      changes.push(...delta);
    },
    onError(error) {
      errors.push(error.code);
    },
  });
  source.on("update", (update) => binding.applyRemoteUpdate(update));

  sourceText.format(0, 1, { bold: true });
  assert.equal(targetText.toString(), "safe");
  assert.deepEqual(changes, []);
  assert.deepEqual(errors, ["unsupported_text"]);
  assert.throws(
    () => binding.applyLocalReplacement({ from: 0, to: 0, insert: "x" }),
    (error) => error?.code === "unsupported_text",
  );
  binding.destroy();
  source.destroy();
  target.destroy();
});

test("inbound Yjs and awareness byte boundaries reject malformed input before a binding changes state", () => {
  const document = new Y.Doc();
  const text = document.getText("content");
  text.insert(0, "safe");
  const awareness = new Awareness(document);
  const binding = new YjsTextBinding(document, text, { ...limits, maxUpdateBytes: 32, maxAwarenessBytes: 32 }, awareness);

  assert.throws(
    () => binding.applyRemoteUpdate(new Uint8Array(33)),
    (error) => error?.code === "resource_limit",
  );
  assert.throws(
    () => binding.applyRemoteAwarenessUpdate(new Uint8Array(33)),
    (error) => error?.code === "resource_limit",
  );
  assert.equal(text.toString(), "safe");
  assert.deepEqual([...awareness.getStates().keys()], [document.clientID]);

  assert.throws(
    () => binding.applyRemoteUpdate(new Uint8Array([255])),
    (error) => error?.code === "invalid_update",
  );
  assert.throws(
    () => binding.applyRemoteAwarenessUpdate(new Uint8Array([255])),
    (error) => error?.code === "invalid_update",
  );
  assert.equal(text.toString(), "safe");
  assert.deepEqual([...awareness.getStates().keys()], [document.clientID]);
  binding.destroy();
  awareness.destroy();
  document.destroy();
});

test("native Yjs relative cursor and y-protocols awareness resolve after a concurrent document update", () => {
  const source = new Y.Doc();
  const target = new Y.Doc();
  const sourceText = source.getText("content");
  sourceText.insert(0, "abcd");
  Y.applyUpdate(target, Y.encodeStateAsUpdate(source));
  const targetText = target.getText("content");
  const sourceAwareness = new Awareness(source);
  const targetAwareness = new Awareness(target);
  const sourceBinding = new YjsTextBinding(source, sourceText, limits, sourceAwareness);
  let emittedAwareness = 0;
  const targetBinding = new YjsTextBinding(target, targetText, {
    ...limits,
    onLocalAwarenessUpdate() {
      emittedAwareness += 1;
    },
  }, targetAwareness);
  source.on("update", (update) => targetBinding.applyRemoteUpdate(update));

  sourceBinding.setLocalCursor({ anchor: 2, head: 2 });
  targetBinding.applyRemoteAwarenessUpdate(encodeAwarenessUpdate(sourceAwareness, [source.clientID]));
  assert.deepEqual(targetBinding.remoteCursors(), [{ clientID: source.clientID, selection: { anchor: 2, head: 2 } }]);
  assert.equal(emittedAwareness, 0, "a remote awareness message must not re-enter the local outbox");

  sourceText.insert(0, "x");
  assert.deepEqual(targetBinding.remoteCursors(), [{ clientID: source.clientID, selection: { anchor: 3, head: 3 } }]);

  targetBinding.setLocalCursor({ anchor: 1, head: 3 });
  assert.equal(emittedAwareness, 1);
  assert.throws(
    () => targetBinding.applyRemoteAwarenessUpdate(new Uint8Array(limits.maxAwarenessBytes + 1)),
    (error) => error?.code === "resource_limit",
  );

  sourceBinding.destroy();
  targetBinding.destroy();
  sourceAwareness.destroy();
  targetAwareness.destroy();
  source.destroy();
  target.destroy();
});

test("simulated delayed, duplicate, and reordered Yjs updates converge across three documents", () => {
  const source = new Y.Doc();
  const sourceText = source.getText("content");
  sourceText.insert(0, "seed");
  const replicas = [source, new Y.Doc(), new Y.Doc()];
  for (const replica of replicas.slice(1)) {
    Y.applyUpdate(replica, Y.encodeStateAsUpdate(source));
  }
  const queued = [];
  let collecting = true;
  for (const [index, replica] of replicas.entries()) {
    replica.on("update", (update) => {
      if (collecting) queued.push({ source: index, update: update.slice() });
    });
  }

  let random = 0x13579bdf;
  const nextRandom = () => {
    random = (random * 1664525 + 1013904223) >>> 0;
    return random;
  };
  for (let operation = 0; operation < 180; operation += 1) {
    const replica = replicas[nextRandom() % replicas.length];
    const text = replica.getText("content");
    const offset = nextRandom() % (text.length + 1);
    if (text.length > 4 && (nextRandom() & 3) === 0) {
      text.delete(offset === text.length ? offset - 1 : offset, 1);
    } else {
      text.insert(offset, String.fromCharCode(97 + (nextRandom() % 26)));
    }
  }
  collecting = false;
  for (let index = queued.length - 1; index > 0; index -= 1) {
    const swap = nextRandom() % (index + 1);
    [queued[index], queued[swap]] = [queued[swap], queued[index]];
  }
  for (const message of queued) {
    for (const [index, replica] of replicas.entries()) {
      if (index !== message.source) {
        Y.applyUpdate(replica, message.update);
        if ((nextRandom() & 7) === 0) {
          Y.applyUpdate(replica, message.update);
        }
      }
    }
  }
  const expected = source.getText("content").toString();
  for (const replica of replicas.slice(1)) {
    assert.equal(replica.getText("content").toString(), expected);
  }
  for (const replica of replicas) replica.destroy();
});

test("native Yjs relative positions survive concurrent text changes and reject a foreign shared type", () => {
  const source = new Y.Doc();
  const target = new Y.Doc();
  const sourceText = source.getText("content");
  sourceText.insert(0, "abcd");
  Y.applyUpdate(target, Y.encodeStateAsUpdate(source));
  const targetText = target.getText("content");
  const binding = new YjsTextBinding(target, targetText, limits);
  source.on("update", (update) => binding.applyRemoteUpdate(update));

  const after = binding.createRelativePosition(2);
  const before = binding.createRelativePosition(2, -1);
  sourceText.insert(0, "x");
  assert.equal(binding.resolveRelativePosition(after), 3);
  assert.equal(binding.resolveRelativePosition(before), 3);

  const foreign = source.getText("foreign");
  foreign.insert(0, "z");
  const encodedForeign = Buffer.from(Y.encodeRelativePosition(Y.createRelativePositionFromTypeIndex(foreign, 0))).toString("base64");
  assert.throws(
    () => binding.resolveRelativePosition({ version: 1, encoded: encodedForeign }),
    (error) => error?.code === "invalid_relative_position",
  );
  assert.throws(
    () => binding.resolveRelativePosition({ version: 2, encoded: after.encoded }),
    (error) => error?.code === "invalid_relative_position",
  );

  binding.destroy();
  source.destroy();
  target.destroy();
});

test("binding-scoped undo emits compensating updates, excludes remote edits, and supports redo", () => {
  const author = new Y.Doc();
  const authorText = author.getText("content");
  authorText.insert(0, "seed");
  const editor = new Y.Doc();
  Y.applyUpdate(editor, Y.encodeStateAsUpdate(author));
  const editorText = editor.getText("content");
  const localUpdates = [];
  const binding = new YjsTextBinding(editor, editorText, {
    ...limits,
    onLocalUpdate(update) {
      localUpdates.push(update.slice());
      Y.applyUpdate(author, update);
    },
  });
  author.on("update", (update) => binding.applyRemoteUpdate(update));
  const undo = binding.createUndoManager({ captureTimeout: 0 });

  binding.applyLocalReplacement({ from: 4, to: 4, insert: "A" });
  undo.stopCapturing();
  binding.applyLocalReplacement({ from: 5, to: 5, insert: "B" });
  assert.equal(editorText.toString(), "seedAB");
  assert.equal(undo.canUndo(), true);

  authorText.insert(0, "R");
  assert.equal(editorText.toString(), "RseedAB");
  assert.equal(localUpdates.length, 2, "remote updates must not enter local undo transport output");

  assert.equal(undo.undo(), true);
  assert.equal(editorText.toString(), "RseedA");
  assert.equal(authorText.toString(), "RseedA");
  assert.equal(localUpdates.length, 3, "undo is a compensating local Yjs update");
  assert.equal(undo.redo(), true);
  assert.equal(editorText.toString(), "RseedAB");
  assert.equal(authorText.toString(), "RseedAB");
  assert.equal(localUpdates.length, 4);

  assert.equal(undo.destroy(), true);
  assert.equal(undo.destroy(), false);
  binding.destroy();
  author.destroy();
  editor.destroy();
});

test("bounded Yjs undo history safely resets before a local replacement exceeds its stack cap", () => {
  const document = new Y.Doc();
  const text = document.getText("content");
  const binding = new YjsTextBinding(document, text, limits);
  const undo = binding.createUndoManager({ captureTimeout: 0, maxStackItems: 2 });

  binding.applyLocalReplacement({ from: 0, to: 0, insert: "A" });
  binding.applyLocalReplacement({ from: 1, to: 1, insert: "B" });
  binding.applyLocalReplacement({ from: 2, to: 2, insert: "C" });
  assert.equal(text.toString(), "ABC");

  assert.equal(undo.undo(), true, "the replacement recorded after reset remains undoable");
  assert.equal(text.toString(), "AB");
  assert.equal(undo.undo(), false, "older stack items are released as one safe history reset");
  assert.equal(undo.redo(), true);
  assert.equal(text.toString(), "ABC");

  binding.applyLocalReplacement({ from: 3, to: 3, insert: "D" });
  assert.equal(undo.undo(), true, "a later local edit clears stale redo history and remains undoable");
  assert.equal(text.toString(), "ABC");
  assert.equal(undo.undo(), true, "the current bounded undo stack retains the preceding captured edit");
  assert.equal(text.toString(), "AB");
  assert.equal(undo.undo(), false);

  assert.throws(
    () => binding.createUndoManager({ maxStackItems: 0 }),
    (error) => error?.code === "invalid_options",
  );
  undo.destroy();
  binding.destroy();
  document.destroy();
});

test("bounded Yjs observeDeep reports nested paths once and fails closed on overflow or callback failure", () => {
  const document = new Y.Doc();
  const board = document.getMap("board");
  const card = new Y.Map();
  const labels = new Y.Array();
  board.set("card", card);
  card.set("labels", labels);
  const batches = [];
  const observer = observeYjsDeep(board, {
    maxEventsPerTransaction: 4,
    maxPathDepth: 3,
    onChanges(changes) {
      batches.push(changes);
    },
  });

  document.transact(() => {
    card.set("title", "draft");
    labels.push(["planning"]);
  });
  assert.equal(batches.length, 1);
  assert.deepEqual(batches[0].map((change) => JSON.stringify(change.path)).sort(), ["[\"card\",\"labels\"]", "[\"card\"]"]);
  assert.equal(Object.isFrozen(batches[0][0].path), true);
  assert.equal(batches[0].some((change) => change.target === card), true);
  assert.equal(observer.destroy(), true);
  assert.equal(observer.destroy(), false);

  const errors = [];
  const overflow = observeYjsDeep(board, {
    maxEventsPerTransaction: 1,
    maxPathDepth: 3,
    onChanges() {
      assert.fail("overflow observer must not receive a partial batch");
    },
    onError(error) {
      errors.push(error.code);
    },
  });
  document.transact(() => {
    card.set("status", "open");
    labels.push(["urgent"]);
  });
  assert.deepEqual(errors, ["resource_limit"]);
  assert.equal(overflow.destroy(), false, "overflow stops and unregisters the observer");

  const callbackErrors = [];
  const failing = observeYjsDeep(board, {
    maxEventsPerTransaction: 4,
    maxPathDepth: 3,
    onChanges() {
      throw new Error("view failed");
    },
    onError(error) {
      callbackErrors.push(error.code);
    },
  });
  card.set("status", "closed");
  assert.deepEqual(callbackErrors, ["observer_failed"]);
  assert.equal(failing.destroy(), false);
  document.destroy();
});

test("bounded V1 y-protocols SyncStep1/2 interoperates with the official implementation and rejects malformed envelopes", () => {
  const server = new Y.Doc();
  server.getText("content").insert(0, "server");
  const client = new Y.Doc();
  client.getText("content").insert(0, "client");
  const clientBinding = new YjsTextBinding(client, client.getText("content"), limits);
  const clientSync = clientBinding.createSyncProtocol({ maxMessageBytes: 1 << 20 });

  const officialReplyEncoder = encoding.createEncoder();
  syncProtocol.readSyncMessage(
    decoding.createDecoder(clientSync.encodeSyncStep1()),
    officialReplyEncoder,
    server,
    "official-server",
  );
  const serverReply = encoding.toUint8Array(officialReplyEncoder);
  assert.equal(clientSync.receive(serverReply), undefined);
  const officialStep1Encoder = encoding.createEncoder();
  syncProtocol.writeSyncStep1(officialStep1Encoder, server);
  const clientReply = clientSync.receive(encoding.toUint8Array(officialStep1Encoder));
  assert.notEqual(clientReply, undefined);
  syncProtocol.readSyncMessage(
    decoding.createDecoder(clientReply),
    encoding.createEncoder(),
    server,
    "official-server",
  );
  assert.equal(server.getText("content").toString(), client.getText("content").toString());
  assert.equal(server.getText("content").toString().includes("server"), true);
  assert.equal(server.getText("content").toString().includes("client"), true);

  const before = client.getText("content").toString();
  assert.throws(
    () => clientSync.receive(Uint8Array.from([...clientSync.encodeSyncStep1(), 0])),
    (error) => error?.code === "invalid_update",
  );
  assert.throws(
    () => clientSync.receive(new Uint8Array([3, 0])),
    (error) => error?.code === "invalid_update",
  );
  assert.equal(client.getText("content").toString(), before);

  const v2Document = new Y.Doc();
  const v2 = new YjsTextBinding(v2Document, v2Document.getText("content"), { ...limits, updateFormat: "v2" });
  assert.throws(
    () => v2.createSyncProtocol({ maxMessageBytes: 64 }),
    (error) => error?.code === "sync_mismatch",
  );

  clientBinding.destroy();
  v2.destroy();
  server.destroy();
  client.destroy();
  v2Document.destroy();
});
