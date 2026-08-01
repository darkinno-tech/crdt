import assert from "node:assert/strict";
import test from "node:test";

import { BlockNoteEditor } from "@blocknote/core";

import { bindBlockNoteRichText } from "../dist/bindings.js";

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
      if (previous !== undefined && sameAttributes(previous.attributes, value.attributes)) previous.text += value.rune;
      else spans.push({ text: value.rune, attributes: { ...value.attributes } });
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
        this.values.splice(offset, 0, ...Array.from(operation.insert).map((rune) => ({ rune, attributes: attributesFromChanges(operation.changes ?? []) })));
        offset += Array.from(operation.insert).length;
      }
    }
    return new TextEncoder().encode(JSON.stringify(operations));
  }

  applyDelta(frame) {
    this.applyEditorDelta(JSON.parse(new TextDecoder().decode(frame)));
  }
}

test("real BlockNote core editor imports, updates, and renders a remote rich-text merge", () => {
  const editor = BlockNoteEditor.create({
    initialContent: [{
      type: "heading",
      props: { level: 2, isToggleable: true },
      content: [{ type: "text", text: "Release", styles: { bold: true } }],
      children: [{ type: "checkListItem", props: { checked: true }, content: "Validate" }],
    }],
  });
  const document = new FakeRichText();
  const frames = [];
  const binding = bindBlockNoteRichText(document, editor, { initialContent: "editor", onLocalFrame: (frame) => frames.push(frame) });

  assert.equal(document.text(), "Release\nValidate\n");
  assert.equal(frames.length, 1);
  editor.updateBlock(editor.document[0], { content: "Release candidate" });
  assert.equal(document.text(), "Release candidate\nValidate\n");
  assert.equal(frames.length, 2);

  const marker = document.spans().find((span) => span.text.includes("candidate")).attributes["rt.block"];
  const remoteDocument = new FakeRichText(document.spans());
  const remote = remoteDocument.applyEditorDelta([
    { retain: Array.from("Release candidate").length },
    { insert: "!", changes: [{ key: "rt.block", value: marker }] },
  ]);
  binding.applyRemote(remote);

  assert.equal(editor.document[0].content[0].text, "Release candidate!");
  assert.equal(editor.document[0].children[0].props.checked, true);
  assert.equal(frames.length, 2);
  binding.destroy();
});

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
