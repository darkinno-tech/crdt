import assert from "node:assert/strict";
import test from "node:test";

import { bindBlockNoteRichText, BLOCKNOTE_RICH_TEXT_SCHEMA_ID } from "../dist/bindings.js";

class FakeRichText {
  constructor(spans = []) {
    this.values = [];
    for (const span of spans) {
      for (const rune of Array.from(span.text)) this.values.push({ rune, attributes: { ...span.attributes } });
    }
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

  text() {
    return this.values.map((value) => value.rune).join("");
  }

  applyEditorDelta(operations) {
    let offset = 0;
    for (const operation of operations) {
      if (operation.retain !== undefined) {
        for (let index = offset; index < offset + operation.retain; index += 1) applyChanges(this.values[index].attributes, operation.changes ?? []);
        offset += operation.retain;
      } else if (operation.delete !== undefined) {
        this.values.splice(offset, operation.delete);
      } else {
        const inserted = Array.from(operation.insert).map((rune) => ({ rune, attributes: attributesFromChanges(operation.changes ?? []) }));
        this.values.splice(offset, 0, ...inserted);
        offset += inserted.length;
      }
    }
    return new TextEncoder().encode(JSON.stringify(operations));
  }

  applyDelta(frame) {
    this.applyEditorDelta(JSON.parse(new TextDecoder().decode(frame)));
  }
}

class FakeBlockNotePort {
  constructor(document) {
    this.document = structuredClone(document);
    this.listeners = new Set();
  }

  replaceBlocks(_remove, insert) {
    this.document = structuredClone(insert);
    this.#emit();
    return {};
  }

  onChange(listener) {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  userReplace(document) {
    this.document = structuredClone(document);
    this.#emit();
  }

  #emit() {
    for (const listener of [...this.listeners]) listener();
  }
}

test("BlockNote text-block binding preserves nested defaults, styles, and remote no-echo", () => {
  assert.equal(BLOCKNOTE_RICH_TEXT_SCHEMA_ID, "darkinno:blocknote-text-v1");
  const port = new FakeBlockNotePort([
    block("heading", { level: 2, isToggleable: true }, [text("Road", { bold: true, textColor: "red" }), text("map")], [
      block("bulletListItem", {}, [text("Ship", { italic: true })]),
    ]),
    block("checkListItem", { checked: true }, [text("Done", { underline: true, backgroundColor: "yellow" })]),
    block("numberedListItem", { textAlignment: "center" }, [text("Sequence", { strike: true })]),
    block("toggleListItem", { backgroundColor: "blue" }, [text("Expand")]),
    block("quote", { textColor: "purple" }, [text("Review")]),
    block("codeBlock", { language: "typescript" }, [text("const ready = true", { code: true })]),
  ]);
  const document = new FakeRichText();
  const frames = [];
  const binding = bindBlockNoteRichText(document, port, { initialContent: "editor", onLocalFrame: (frame) => frames.push(frame) });

  assert.equal(document.text(), "Roadmap\nShip\nDone\nSequence\nExpand\nReview\nconst ready = true\n");
  assert.equal(frames.length, 1);
  assert.deepEqual(port.document, [
    block("heading", { level: 2, isToggleable: true }, [text("Road", { bold: true, textColor: "red" }), text("map")], [
      block("bulletListItem", {}, [text("Ship", { italic: true })]),
    ]),
    block("checkListItem", { checked: true }, [text("Done", { underline: true, backgroundColor: "yellow" })]),
    block("numberedListItem", { textAlignment: "center" }, [text("Sequence", { strike: true })]),
    block("toggleListItem", { backgroundColor: "blue" }, [text("Expand")]),
    block("quote", { textColor: "purple" }, [text("Review")]),
    block("codeBlock", { language: "typescript" }, [text("const ready = true", { code: true })]),
  ]);

  const finalBlockMarker = document.spans().find((span) => span.text.includes("Done")).attributes["rt.block"];
  const remoteDocument = new FakeRichText(document.spans());
  const doneOffset = Array.from(document.text().slice(0, document.text().indexOf("Done") + "Done".length)).length;
  const remote = remoteDocument.applyEditorDelta([
    { retain: doneOffset },
    { insert: "!", changes: [{ key: "rt.block", value: finalBlockMarker }] },
  ]);
  binding.applyRemote(remote);

  assert.equal(document.text(), "Roadmap\nShip\nDone!\nSequence\nExpand\nReview\nconst ready = true\n");
  assert.equal(frames.length, 1);
  assert.equal(port.document[1].content.map((item) => item.text).join(""), "Done!");
  assert.equal(binding.destroy(), true);
  assert.equal(binding.destroy(), false);
});

test("BlockNote binding rejects unsupported content and bounded source before a CRDT mutation", () => {
  const safe = [block("paragraph", {}, [text("safe")])];
  const port = new FakeBlockNotePort(safe);
  const document = new FakeRichText();
  const frames = [];
  bindBlockNoteRichText(document, port, { initialContent: "editor", maxBlocks: 1, onLocalFrame: (frame) => frames.push(frame) });
  const beforeText = document.text();

  assert.throws(() => port.userReplace([{ type: "table", props: { textColor: "default" }, content: {}, children: [] }]), /unsupported_rich_text/);
  assert.deepEqual(port.document, safe);
  assert.equal(document.text(), beforeText);
  assert.equal(frames.length, 1);

  assert.throws(
    () => port.userReplace([block("paragraph", {}, [{ type: "link", href: "https://example.invalid", content: [text("untrusted")] }])]),
    /unsupported_rich_text/,
  );
  assert.deepEqual(port.document, safe);
  assert.equal(document.text(), beforeText);

  assert.throws(() => port.userReplace([block("paragraph", {}, [text("one")]), block("paragraph", {}, [text("two")])]), /resource_limit/);
  assert.deepEqual(port.document, safe);
  assert.equal(document.text(), beforeText);
  assert.equal(frames.length, 1);
});

function block(type, extraProps, content, children = []) {
  const shared = { backgroundColor: "default", textColor: "default", textAlignment: "left" };
  const props = type === "quote"
    ? { backgroundColor: "default", textColor: "default", ...extraProps }
    : type === "codeBlock"
      ? { language: "text", ...extraProps }
      : { ...shared, ...extraProps };
  return { type, props, content, children };
}

function text(value, styles = {}) {
  return { type: "text", text: value, styles };
}

function sameAttributes(left, right) {
  const keys = Object.keys(left);
  return keys.length === Object.keys(right).length && keys.every((key) => left[key] === right[key]);
}

function applyChanges(attributes, changes) {
  for (const change of changes) {
    if (change.remove) delete attributes[change.key];
    else attributes[change.key] = change.value;
  }
}

function attributesFromChanges(changes) {
  const attributes = {};
  applyChanges(attributes, changes);
  return attributes;
}
