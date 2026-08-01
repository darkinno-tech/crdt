import assert from "node:assert/strict";
import test from "node:test";

import {
  bindProseMirrorRichText,
  bindTiptapRichText,
  TIPTAP_CORE_RICH_TEXT_SCHEMA_ID,
} from "../dist/bindings.js";

const encoder = new TextEncoder();
const decoder = new TextDecoder();

const mentionCodec = {
  kind: "mention",
  nodeType: "mention",
  encode(node) {
    if (node.type !== "mention" || !isRecord(node.attrs) || !hasOnlyKeys(node.attrs, ["id", "label"]) ||
      typeof node.attrs.id !== "string" || typeof node.attrs.label !== "string") {
      throw new Error("invalid mention");
    }
    return { id: node.attrs.id, label: node.attrs.label };
  },
  decode(payload) {
    if (!isRecord(payload) || !hasOnlyKeys(payload, ["id", "label"]) || typeof payload.id !== "string" || typeof payload.label !== "string") {
      throw new Error("invalid mention payload");
    }
    return { type: "mention", attrs: { id: payload.id, label: payload.label } };
  },
};

test("Tiptap rich-text profile preserves approved blocks, marks, hard breaks, and atomic embeds", () => {
  const editor = new TiptapPort(initialDocument());
  const document = new FakeRichText();
  const frames = [];
  const binding = bindTiptapRichText(document, editor, {
    initialContent: "editor",
    embeds: [mentionCodec],
    onLocalFrame: (frame) => frames.push(frame),
  });

  assert.equal(TIPTAP_CORE_RICH_TEXT_SCHEMA_ID, "darkinno:tiptap-core-richtext-v1");
  assert.equal(document.text(), "Release\nHello \uFFFC\nworld\none\ntwo\n");
  assert.equal(frames.length, 1);
  assert.equal(attributeAt(document.spans(), "Release", "rt.block"), "heading:2");
  assert.equal(attributeAt(document.spans(), "Release", "rt.bold"), "true");
  assert.equal(attributeAt(document.spans(), "\uFFFC", "rt.embed.kind"), "mention");
  assert.equal(attributeAt(document.spans(), "\uFFFC", "rt.embed.data"), '{"id":"u-7","label":"Ada"}');

  const local = initialDocument();
  local.content[1].content[0] = { type: "text", text: "Welcome ", marks: [{ type: "italic" }] };
  editor.userSet(local);
  assert.equal(document.text(), "Release\nWelcome \uFFFC\nworld\none\ntwo\n");
  assert.equal(frames.length, 2);

  const remote = new FakeRichText(document.spans());
  const frame = remote.applyEditorDelta([
    { insert: "Remote ", changes: [{ key: "rt.block", value: "heading:2" }, { key: "rt.bold", value: "true" }] },
    { retain: Array.from(document.text()).length },
  ]);
  binding.applyRemote(frame);
  assert.equal(document.text(), "Remote Release\nWelcome \uFFFC\nworld\none\ntwo\n");
  assert.equal(editor.getJSON().content[0].type, "heading");
  assert.equal(editor.getJSON().content[1].type, "paragraph");
  assert.deepEqual(editor.getJSON().content[1].content[1], { type: "mention", attrs: { id: "u-7", label: "Ada" } });
  assert.equal(frames.length, 2, "remote writes must not echo into the outbox");
  assert.equal(binding.destroy(), true);
});

test("Tiptap rich-text profile rejects unknown nodes and restores the canonical CRDT view", () => {
  const editor = new TiptapPort({ type: "doc", content: [{ type: "paragraph", content: [{ type: "text", text: "safe" }] }] });
  const document = new FakeRichText();
  const binding = bindTiptapRichText(document, editor, {
    initialContent: "editor",
    embeds: [mentionCodec],
    onLocalFrame() {},
  });
  const before = structuredClone(editor.getJSON());
  const textBefore = document.text();

  assert.throws(() => editor.userSet({
    type: "doc",
    content: [{ type: "paragraph", content: [{ type: "image", attrs: { src: "https://untrusted.invalid/image.png" } }] }],
  }));
  assert.equal(document.text(), textBefore);
  assert.deepEqual(editor.getJSON(), before);

  assert.throws(() => editor.userSet({
    type: "doc",
    content: [{ type: "paragraph", content: [{ type: "mention", attrs: { id: "u-7", label: "Ada", href: "javascript:alert(1)" } }] }],
  }));
  assert.equal(document.text(), textBefore);
  assert.deepEqual(editor.getJSON(), before);
  binding.destroy();
});

test("Tiptap rich-text profile applies limits before a local mutation", () => {
  const editor = new TiptapPort({ type: "doc", content: [{ type: "paragraph", content: [{ type: "text", text: "safe" }] }] });
  const document = new FakeRichText();
  const binding = bindTiptapRichText(document, editor, {
    initialContent: "editor",
    maxTextRunes: 8,
    onLocalFrame() {},
  });
  const before = structuredClone(editor.getJSON());
  assert.throws(() => editor.userSet({ type: "doc", content: [{ type: "paragraph", content: [{ type: "text", text: "too long text" }] }] }));
  assert.equal(document.text(), "safe\n");
  assert.deepEqual(editor.getJSON(), before);
  binding.destroy();
});

test("Tiptap rich-text profile freezes rather than renders a remotely admitted wrong-schema embed", () => {
  const editor = new TiptapPort({ type: "doc", content: [{ type: "paragraph", content: [{ type: "text", text: "safe" }] }] });
  const document = new FakeRichText();
  const binding = bindTiptapRichText(document, editor, {
    initialContent: "editor",
    embeds: [mentionCodec],
    onLocalFrame() {},
  });
  const before = structuredClone(editor.getJSON());
  const forged = new FakeRichText();
  const frame = forged.applyEditorDelta([{
    insert: "\uFFFC\n",
    changes: [
      { key: "rt.block", value: "paragraph" },
      { key: "rt.embed.kind", value: "unknown-widget" },
      { key: "rt.embed.data", value: "{}" },
    ],
  }]);
  assert.throws(() => binding.applyRemote(frame));
  assert.deepEqual(editor.getJSON(), before, "the unrecognised remote projection must not render");
  assert.throws(() => binding.applyRemote(frame), { code: "binding_closed" });
  binding.destroy();
});

test("ProseMirror rich-text port keeps remote transactions out of its local observer", () => {
  const port = new ProseMirrorPort({ type: "doc", content: [{ type: "paragraph", content: [{ type: "text", text: "draft" }] }] });
  const document = new FakeRichText();
  const frames = [];
  const binding = bindProseMirrorRichText(document, port, {
    initialContent: "editor",
    onLocalFrame: (frame) => frames.push(frame),
  });
  assert.equal(document.text(), "draft\n");

  port.userSet({ type: "doc", content: [{ type: "paragraph", content: [{ type: "text", text: "review" }] }] });
  assert.equal(document.text(), "review\n");
  assert.equal(frames.length, 2);

  const remote = new FakeRichText(document.spans());
  binding.applyRemote(remote.applyEditorDelta([
    { insert: "remote ", changes: [{ key: "rt.block", value: "paragraph" }] },
    { retain: Array.from(document.text()).length },
  ]));
  assert.equal(port.readJSON().content[0].content[0].text, "remote review");
  assert.equal(frames.length, 2);
  binding.destroy();
});

class TiptapPort {
  #listeners = new Set();

  constructor(value) {
    this.value = structuredClone(value);
    this.commands = {
      setContent: (content, options) => {
        this.value = structuredClone(content);
        if (options.emitUpdate) {
          this.#emit();
        }
        return true;
      },
    };
  }

  getJSON() {
    return structuredClone(this.value);
  }

  on(event, listener) {
    assert.equal(event, "update");
    this.#listeners.add(listener);
  }

  off(event, listener) {
    assert.equal(event, "update");
    this.#listeners.delete(listener);
  }

  userSet(value) {
    this.value = structuredClone(value);
    this.#emit();
  }

  #emit() {
    for (const listener of this.#listeners) {
      listener({ editor: this });
    }
  }
}

class ProseMirrorPort {
  #listeners = new Set();

  constructor(value) {
    this.value = structuredClone(value);
  }

  readJSON() {
    return structuredClone(this.value);
  }

  replaceJSON(content) {
    this.value = structuredClone(content);
    return true;
  }

  observeUpdate(listener) {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  }

  userSet(value) {
    this.value = structuredClone(value);
    for (const listener of this.#listeners) {
      listener();
    }
  }
}

class FakeRichText {
  constructor(spans = []) {
    this.values = [];
    for (const span of spans) {
      for (const rune of span.text) {
        this.values.push({ rune, attributes: { ...(span.attributes ?? {}) } });
      }
    }
  }

  text() {
    return this.values.map((value) => value.rune).join("");
  }

  spans() {
    const spans = [];
    for (const value of this.values) {
      const previous = spans.at(-1);
      if (previous !== undefined && sameAttributes(previous.attributes, value.attributes)) {
        previous.text += value.rune;
      } else {
        spans.push({ text: value.rune, attributes: { ...value.attributes } });
      }
    }
    return spans;
  }

  applyEditorDelta(operations) {
    let offset = 0;
    for (const operation of operations) {
      if (operation.retain !== undefined) {
        for (let index = offset; index < offset + operation.retain; index++) {
          this.#applyChanges(this.values[index].attributes, operation.changes ?? []);
        }
        offset += operation.retain;
      } else if (operation.delete !== undefined) {
        this.values.splice(offset, operation.delete);
      } else if (operation.insert !== undefined) {
        const attributes = {};
        this.#applyChanges(attributes, operation.changes ?? []);
        this.values.splice(offset, 0, ...Array.from(operation.insert, (rune) => ({ rune, attributes: { ...attributes } })));
        offset += Array.from(operation.insert).length;
      } else {
        throw new Error("invalid operation");
      }
    }
    return encoder.encode(JSON.stringify(operations));
  }

  applyDelta(frame) {
    this.applyEditorDelta(JSON.parse(decoder.decode(frame)));
  }

  #applyChanges(attributes, changes) {
    for (const change of changes) {
      if (change.remove) {
        delete attributes[change.key];
      } else {
        attributes[change.key] = change.value;
      }
    }
  }
}

function initialDocument() {
  return {
    type: "doc",
    content: [
      { type: "heading", attrs: { level: 2 }, content: [{ type: "text", text: "Release", marks: [{ type: "bold" }] }] },
      {
        type: "paragraph",
        content: [
          { type: "text", text: "Hello ", marks: [{ type: "italic" }] },
          { type: "mention", attrs: { id: "u-7", label: "Ada" } },
          { type: "hardBreak" },
          { type: "text", text: "world" },
        ],
      },
      { type: "codeBlock", content: [{ type: "text", text: "one\ntwo" }] },
    ],
  };
}

function attributeAt(spans, text, key) {
  return spans.find((span) => span.text.includes(text)).attributes[key];
}

function sameAttributes(left, right) {
  const keys = Object.keys(left);
  return keys.length === Object.keys(right).length && keys.every((key) => left[key] === right[key]);
}

function isRecord(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function hasOnlyKeys(value, allowed) {
  return Object.keys(value).every((key) => allowed.includes(key));
}
