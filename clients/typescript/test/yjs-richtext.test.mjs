import assert from "node:assert/strict";
import test from "node:test";

import * as Y from "yjs";

import {
  bindYjsQuillRichText,
  YjsBindingError,
  YjsRichTextBinding,
} from "../dist/index.js";

const limits = Object.freeze({
  maxUpdateBytes: 1 << 20,
  maxTextUTF16: 1 << 20,
  maxDeltaOperations: 64,
  maxAttributesPerOperation: 8,
  maxAttributeKeyBytes: 64,
  maxAttributeValueBytes: 1024,
  maxEmbedBytes: 4096,
  allowedAttributes: ["bold", "header"],
  allowedEmbeds: ["image"],
});

test("native Yjs Quill binding converges approved formats and embeds without an RGA translation", () => {
  const author = new Y.Doc();
  const authorText = author.getText("content");
  authorText.insert(0, "hello\n");

  const replica = new Y.Doc();
  Y.applyUpdate(replica, Y.encodeStateAsUpdate(author));
  const replicaText = replica.getText("content");
  const quill = new QuillModel({ ops: [{ insert: "stale\n" }] });
  let localUpdates = 0;
  const binding = bindYjsQuillRichText(replica, replicaText, quill, {
    ...limits,
    onLocalUpdate(update) {
      localUpdates += 1;
      Y.applyUpdate(author, update);
    },
  });
  author.on("update", (update) => binding.applyRemoteUpdate(update));

  assert.deepEqual(quill.getContents(), deltaFrom(authorText));
  quill.userDelta({ ops: [{ retain: 5, attributes: { bold: true } }] });
  assert.equal(localUpdates, 1);
  assert.deepEqual(deltaFrom(replicaText), deltaFrom(authorText));

  authorText.applyDelta([{ retain: 5 }, { insert: { image: "cover-7" } }]);
  assert.deepEqual(quill.getContents(), deltaFrom(replicaText));
  assert.equal(quill.apiWrites, 2, "initial restoration and remote Delta each use the editor's incremental API");

  quill.userDelta({ ops: [{ retain: 6 }, { insert: "!" }] });
  assert.equal(localUpdates, 2);
  assert.deepEqual(quill.getContents(), deltaFrom(replicaText));
  assert.deepEqual(deltaFrom(replicaText), deltaFrom(authorText));

  binding.destroy();
  author.destroy();
  replica.destroy();
  quill.destroy();
});

test("Yjs Quill binding restores unsupported local Deltas and freezes on an unsupported remote schema", () => {
  const document = new Y.Doc();
  const text = document.getText("content");
  text.insert(0, "safe\n");
  const quill = new QuillModel(deltaFrom(text));
  const binding = bindYjsQuillRichText(document, text, quill, limits);

  assert.throws(
    () => quill.userDelta({ ops: [{ retain: 4, attributes: { italic: true } }] }),
    (error) => error instanceof YjsBindingError && error.code === "unsupported_rich_text",
  );
  assert.deepEqual(quill.getContents(), deltaFrom(text), "an unsupported local editor mutation cannot remain locally visible");

  const source = new Y.Doc();
  Y.applyUpdate(source, Y.encodeStateAsUpdate(document));
  const sourceText = source.getText("content");
  const errors = [];
  const protectedBinding = new YjsRichTextBinding(document, text, {
    ...limits,
    onError(error) {
      errors.push(error.code);
    },
  });
  source.on("update", (update) => protectedBinding.applyRemoteUpdate(update));
  sourceText.format(0, 4, { italic: true });

  assert.deepEqual(errors, ["unsupported_rich_text"]);
  assert.throws(
    () => protectedBinding.applyLocalDelta({ ops: [{ retain: 5 }, { insert: "x" }] }),
    (error) => error instanceof YjsBindingError && error.code === "unsupported_rich_text",
  );

  protectedBinding.destroy();
  binding.destroy();
  source.destroy();
  document.destroy();
  quill.destroy();
});

test("Yjs rich-text hand-off failure is latched after its committed Yjs transaction", () => {
  const document = new Y.Doc();
  const text = document.getText("content");
  const errors = [];
  let calls = 0;
  const binding = new YjsRichTextBinding(document, text, {
    ...limits,
    onLocalUpdate() {
      calls += 1;
      throw new Error("outbox unavailable");
    },
    onError(error) {
      errors.push(error.code);
    },
  });

  assert.throws(
    () => binding.applyLocalDelta({ ops: [{ insert: "A" }] }),
    (error) => error instanceof YjsBindingError && error.code === "local_update_failed",
  );
  assert.equal(text.toString(), "A", "Yjs commits before a manual transport callback can fail");
  assert.deepEqual(errors, ["local_update_failed"]);
  assert.equal(calls, 1);
  assert.throws(
    () => binding.applyLocalDelta({ ops: [{ retain: 1 }, { insert: "B" }] }),
    (error) => error instanceof YjsBindingError && error.code === "local_update_failed",
  );
  assert.equal(text.toString(), "A");
  assert.equal(calls, 1, "the latched binding cannot generate another unhanded update");

  binding.destroy();
  document.destroy();
});

test("Yjs rich-text Delta limits reject malformed operations before local Y.Text mutation", () => {
  const document = new Y.Doc();
  const text = document.getText("content");
  text.insert(0, "safe\n");
  const binding = new YjsRichTextBinding(document, text, { ...limits, maxDeltaOperations: 1 });
  const before = deltaFrom(text);

  assert.throws(
    () => binding.applyLocalDelta({ ops: [{ retain: 4 }, { insert: { image: "cover" } }] }),
    (error) => error instanceof YjsBindingError && error.code === "resource_limit",
  );
  assert.throws(
    () => binding.applyLocalDelta({ ops: [{ retain: 4, attributes: { bold: { nested: true } } }] }),
    (error) => error instanceof YjsBindingError && error.code === "unsupported_rich_text",
  );
  assert.deepEqual(deltaFrom(text), before);

  binding.destroy();
  document.destroy();
});

test("Yjs rich-text Delta preflight preserves Quill insert/delete source-cursor semantics", () => {
  const document = new Y.Doc();
  const text = document.getText("content");
  text.insert(0, "safe\n");
  const binding = new YjsRichTextBinding(document, text, limits);

  binding.applyLocalDelta(new QuillDelta([{ insert: "R" }, { retain: 5 }]));
  assert.equal(text.toString(), "Rsafe\n", "an insert does not consume source text before a later retain");
  binding.applyLocalDelta({ ops: [{ delete: 1 }, { retain: 5 }] });
  assert.equal(text.toString(), "safe\n", "a delete consumes source text before a later retain");

  binding.destroy();
  document.destroy();
});

test("deterministic malformed rich-text Delta fuzzing fails before local Y.Text mutation", () => {
  let state = 0x6d2b79f5;
  for (let sample = 0; sample < 256; sample += 1) {
    const document = new Y.Doc();
    const text = document.getText("content");
    text.insert(0, "safe\n");
    const binding = new YjsRichTextBinding(document, text, limits);
    const before = deltaFrom(text);
    const value = fuzzDeltaValue(() => {
      state = (state * 1664525 + 1013904223) >>> 0;
      return state;
    }, 0);
    try {
      binding.applyLocalDelta(value);
    } catch (error) {
      assert.equal(error instanceof YjsBindingError, true, `sample ${sample} must not expose a raw parser error`);
      assert.deepEqual(deltaFrom(text), before, `sample ${sample} must reject atomically`);
    }
    binding.destroy();
    document.destroy();
  }
});

function deltaFrom(text) {
  return { ops: text.toDelta().map((operation) => structuredClone(operation)) };
}

class QuillModel {
  constructor(initial) {
    this.document = new Y.Doc();
    this.text = this.document.getText("content");
    this.listeners = new Set();
    this.apiWrites = 0;
    this.setContents(initial, "silent");
  }

  getContents() {
    return deltaFrom(this.text);
  }

  setContents(delta, source = "api") {
    const old = this.getContents();
    this.document.transact(() => {
      if (this.text.length > 0) this.text.delete(0, this.text.length);
      this.text.applyDelta(delta.ops);
    });
    if (source === "api") this.apiWrites += 1;
    this.#emit(delta, old, source);
  }

  updateContents(delta, source = "api") {
    const old = this.getContents();
    this.text.applyDelta(delta.ops);
    if (source === "api") this.apiWrites += 1;
    this.#emit(delta, old, source);
  }

  userDelta(delta) {
    this.updateContents(delta, "user");
  }

  on(event, listener) {
    assert.equal(event, "text-change");
    this.listeners.add(listener);
  }

  off(event, listener) {
    assert.equal(event, "text-change");
    this.listeners.delete(listener);
  }

  destroy() {
    this.listeners.clear();
    this.document.destroy();
  }

  #emit(delta, old, source) {
    if (source === "silent") return;
    for (const listener of this.listeners) listener(delta, old, source);
  }
}

function fuzzDeltaValue(next, depth) {
  const kind = next() % 7;
  if (depth > 3 || kind === 0) return [undefined, null, true, false, next(), `v${next()}`][next() % 6];
  if (kind === 1) return Array.from({ length: next() % 4 }, () => fuzzDeltaValue(next, depth + 1));
  const record = {};
  for (let index = 0; index < next() % 4; index += 1) {
    record[`k${next() % 5}`] = fuzzDeltaValue(next, depth + 1);
  }
  if (kind === 2) record.ops = fuzzDeltaValue(next, depth + 1);
  if (kind === 3) record.insert = fuzzDeltaValue(next, depth + 1);
  if (kind === 4) record.retain = fuzzDeltaValue(next, depth + 1);
  if (kind === 5) record.delete = fuzzDeltaValue(next, depth + 1);
  return record;
}

class QuillDelta {
  constructor(ops) {
    this.ops = ops;
  }
}
