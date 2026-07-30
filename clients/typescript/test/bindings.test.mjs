import assert from "node:assert/strict";
import test from "node:test";

import { bindQuillPlainText, bindRGAPlainText } from "../dist/bindings.js";

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

  applyDelta(frame) {
    const operation = JSON.parse(new TextDecoder().decode(frame));
    if (operation.kind === "insert") {
      this.insert(operation.offset, operation.value);
    } else {
      this.delete(operation.offset, operation.count);
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

test("plain-text binding emits bounded rune-aware RGA frames and prevents remote echo", () => {
  const document = new FakeRGA("", { maxLocalEditRunes: 2, maxLocalEditBytes: 8 });
  const port = new TextPort("");
  const frames = [];
  const binding = bindRGAPlainText(document, port, { onLocalFrame: (frame) => frames.push(frame) });

  port.userWrite("ab🙂cd");
  assert.equal(document.text(), "ab🙂cd");
  assert.equal(frames.length, 3);

  const remote = new FakeRGA("ab🙂cd");
  const deleteFrame = remote.delete(2, 1);
  binding.applyRemote(deleteFrame);
  assert.equal(port.readText(), "abcd");
  assert.equal(frames.length, 3);

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
    { kind: "delete", offset: 1, count: 4 },
    { kind: "insert", offset: 1, value: "aft!" },
  ]);
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
