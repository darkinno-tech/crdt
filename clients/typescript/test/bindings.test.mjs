import assert from "node:assert/strict";
import test from "node:test";

import {
  bindCodeMirrorPlainText,
  bindLexicalPlainText,
  bindQuillPlainText,
  bindRGAPlainText,
  bindTiptapPlainText,
} from "../dist/bindings.js";

class FakeRGA {
  constructor(text = "", limits = {}) {
    this.value = text;
    this.protocol = {
      maxLocalEditBytes: limits.maxLocalEditBytes ?? 64,
      maxLocalEditRunes: limits.maxLocalEditRunes ?? 16,
    };
  }

  text() {
    return this.value;
  }

  insert(offset, value) {
    const runes = Array.from(this.value);
    runes.splice(offset, 0, ...Array.from(value));
    this.value = runes.join("");
    return new TextEncoder().encode(JSON.stringify({ kind: "insert", offset, value }));
  }

  delete(offset, count) {
    const runes = Array.from(this.value);
    runes.splice(offset, count);
    this.value = runes.join("");
    return new TextEncoder().encode(JSON.stringify({ kind: "delete", offset, count }));
  }

  replace(offset, count, value) {
    const runes = Array.from(this.value);
    runes.splice(offset, count, ...Array.from(value));
    this.value = runes.join("");
    return new TextEncoder().encode(JSON.stringify({ kind: "replace", offset, count, value }));
  }

  applyDelta(frame) {
    const operation = JSON.parse(new TextDecoder().decode(frame));
    if (operation.kind === "insert") {
      this.insert(operation.offset, operation.value);
    } else if (operation.kind === "delete") {
      this.delete(operation.offset, operation.count);
    } else {
      this.replace(operation.offset, operation.count, operation.value);
    }
  }
}

class TextPort {
  constructor(value = "") {
    this.value = value;
    this.listeners = new Set();
  }

  readText() {
    return this.value;
  }

  writeText(value) {
    this.value = value;
    for (const listener of [...this.listeners]) listener();
  }

  observeText(listener) {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  userWrite(value) {
    this.writeText(value);
  }
}

class CodeMirrorPort {
  constructor(value = "") {
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

class TiptapPort {
  constructor(value = "") {
    this.value = tiptapJSON(value);
    this.listeners = new Set();
    this.commands = {
      setContent: (content, options) => {
        this.value = content;
        if (options.emitUpdate) this.#emit();
        return true;
      },
    };
  }

  getJSON() {
    return this.value;
  }

  on(_event, listener) {
    this.listeners.add(listener);
  }

  off(_event, listener) {
    this.listeners.delete(listener);
  }

  userWrite(value) {
    this.value = tiptapJSON(value);
    this.#emit();
  }

  userWriteJSON(value) {
    this.value = value;
    this.#emit();
  }

  #emit() {
    for (const listener of [...this.listeners]) listener({});
  }
}

class LexicalPort {
  constructor(value = "") {
    this.value = value;
    this.listeners = new Set();
  }

  readText() {
    return this.value;
  }

  replaceText(value) {
    this.value = value;
    this.#emit();
  }

  registerTextContentListener(listener) {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  userWrite(value) {
    this.value = value;
    this.#emit();
  }

  #emit() {
    for (const listener of [...this.listeners]) listener(this.value);
  }
}

test("plain-text binding emits bounded rune-aware RGA frames and prevents remote echo", () => {
  const document = new FakeRGA("", { maxLocalEditRunes: 8, maxLocalEditBytes: 16 });
  const port = new TextPort("");
  const frames = [];
  const binding = bindRGAPlainText(document, port, { onLocalFrame: (frame) => frames.push(frame) });

  port.userWrite("ab🙂cd");
  assert.equal(document.text(), "ab🙂cd");
  assert.equal(frames.length, 1);

  const remote = new FakeRGA("ab🙂cd");
  const deleteFrame = remote.delete(2, 1);
  binding.applyRemote(deleteFrame);
  assert.equal(port.readText(), "abcd");
  assert.equal(frames.length, 1);

  assert.equal(binding.destroy(), true);
  port.userWrite("local after close");
  assert.equal(document.text(), "abcd");
  assert.equal(binding.destroy(), false);
});

test("editor initial content can be explicitly imported and replacements preserve RGA offsets", () => {
  const document = new FakeRGA("", { maxLocalEditRunes: 64 });
  const port = new TextPort("draft");
  const frames = [];
  bindRGAPlainText(document, port, { initialContent: "editor", onLocalFrame: (frame) => frames.push(frame) });
  assert.equal(document.text(), "draft");
  assert.equal(frames.length, 1);

  port.userWrite("daft!");
  assert.equal(document.text(), "daft!");
  const operations = frames.slice(1).map((frame) => JSON.parse(new TextDecoder().decode(frame)));
  assert.deepEqual(operations, [
    { kind: "replace", offset: 1, count: 4, value: "aft!" },
  ]);
});

test("a replacement over the negotiated bound restores the editor without mutating the RGA", () => {
  const document = new FakeRGA("safe", { maxLocalEditBytes: 4, maxLocalEditRunes: 4 });
  const port = new TextPort("safe");
  const frames = [];
  bindRGAPlainText(document, port, { onLocalFrame: (frame) => frames.push(frame) });

  assert.throws(() => port.userWrite("banana"), /resource_limit/);
  assert.equal(document.text(), "safe");
  assert.equal(port.readText(), "safe");
  assert.equal(frames.length, 0);
});

test("Quill adapter accepts user text changes and ignores API writes used for remote merges", () => {
  const listeners = new Set();
  const quill = {
    value: "\n",
    getText() {
      return this.value;
    },
    setText(value, source = "api") {
      this.value = value.endsWith("\n") ? value : `${value}\n`;
      for (const listener of [...listeners]) listener({}, {}, source);
    },
    on(_event, listener) {
      listeners.add(listener);
    },
    off(_event, listener) {
      listeners.delete(listener);
    },
    userWrite(value) {
      this.setText(value, "user");
    },
  };
  const document = new FakeRGA("\n");
  const frames = [];
  const binding = bindQuillPlainText(document, quill, { onLocalFrame: (frame) => frames.push(frame) });
  quill.userWrite("hello\n");
  assert.equal(document.text(), "hello\n");
  assert.equal(frames.length, 1);

  const remote = new FakeRGA("hello\n");
  binding.applyRemote(remote.insert(0, "remote "));
  assert.equal(quill.getText(), "remote hello\n");
  assert.equal(frames.length, 1);
});

test("CodeMirror adapter sends user updates through its configured view listener without echo", () => {
  const document = new FakeRGA("code");
  const view = new CodeMirrorPort("code");
  const frames = [];
  const binding = bindCodeMirrorPlainText(document, view, { onLocalFrame: (frame) => frames.push(frame) });

  view.userWrite("code\nreview");
  binding.applyViewUpdate({ docChanged: true });
  assert.equal(document.text(), "code\nreview");
  assert.equal(frames.length, 1);

  const remote = new FakeRGA("code\nreview");
  binding.applyRemote(remote.replace(0, 0, "remote "));
  assert.equal(view.state.doc.toString(), "remote code\nreview");
  assert.equal(frames.length, 1);
  assert.equal(binding.destroy(), true);
});

test("Tiptap adapter accepts only canonical plain-text documents and restores rejected rich input", () => {
  const document = new FakeRGA("draft");
  const editor = new TiptapPort("draft");
  const frames = [];
  const binding = bindTiptapPlainText(document, editor, { onLocalFrame: (frame) => frames.push(frame) });

  editor.userWrite("draft\nreview");
  assert.equal(document.text(), "draft\nreview");
  assert.equal(frames.length, 1);

  assert.throws(() => editor.userWriteJSON({
    type: "doc",
    content: [{ type: "paragraph", content: [{ type: "text", text: "bold", marks: [{ type: "bold" }] }] }],
  }), /unsupported_rich_text/);
  assert.deepEqual(editor.getJSON(), tiptapJSON("draft\nreview"));
  assert.equal(document.text(), "draft\nreview");
  assert.equal(frames.length, 1);
  assert.equal(binding.destroy(), true);
});

test("Tiptap adapter rejects malformed and schema-expanding JSON without leaking a native exception", () => {
  const document = new FakeRGA("safe");
  const editor = new TiptapPort("safe");
  const binding = bindTiptapPlainText(document, editor, { onLocalFrame() {} });
  const invalidDocuments = [
    null,
    [],
    {},
    { type: "doc", content: "not-an-array" },
    { type: "doc", content: [{ type: "heading", content: [] }] },
    { type: "doc", content: [{ type: "paragraph", attrs: { align: "center" }, content: [] }] },
    { type: "doc", content: [{ type: "paragraph", content: [null] }] },
    { type: "doc", content: [{ type: "paragraph", content: [{ type: "text", text: 42 }] }] },
  ];

  for (const invalid of invalidDocuments) {
    assert.throws(() => editor.userWriteJSON(invalid), /unsupported_rich_text/);
    assert.deepEqual(editor.getJSON(), tiptapJSON("safe"));
    assert.equal(document.text(), "safe");
  }
  assert.equal(binding.destroy(), true);
});

test("Lexical text leaf adapter sends local text and suppresses remote listener echoes", () => {
  const document = new FakeRGA("hello");
  const editor = new LexicalPort("hello");
  const frames = [];
  const binding = bindLexicalPlainText(document, editor, { onLocalFrame: (frame) => frames.push(frame) });

  editor.userWrite("hello lexical");
  assert.equal(document.text(), "hello lexical");
  assert.equal(frames.length, 1);

  const remote = new FakeRGA("hello lexical");
  binding.applyRemote(remote.replace(0, 0, "remote "));
  assert.equal(editor.readText(), "remote hello lexical");
  assert.equal(frames.length, 1);
  assert.equal(binding.destroy(), true);
});

function tiptapJSON(value) {
  return {
    type: "doc",
    content: value.split("\n").map((paragraph) => ({
      type: "paragraph",
      content: paragraph === "" ? [] : [{ type: "text", text: paragraph }],
    })),
  };
}
