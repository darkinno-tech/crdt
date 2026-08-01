import assert from "node:assert/strict";
import test, { after } from "node:test";

import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { createHeadlessEditor } from "@lexical/headless";
import { $createParagraphNode, $createTextNode, $getRoot } from "lexical";
import { JSDOM } from "jsdom";
import { Editor, Mark, Node } from "@tiptap/core";
import TiptapDocument from "@tiptap/extension-document";
import TiptapParagraph from "@tiptap/extension-paragraph";
import TiptapText from "@tiptap/extension-text";

import {
  bindCodeMirrorPlainText,
  bindLexicalPlainText,
  bindTiptapRichText,
  bindTiptapPlainText,
} from "../dist/bindings.js";

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

test("actual CodeMirror 6 view emits local RGA state and applies remote state without an echo", () => {
  const document = new FakeRGA("const answer = 42;");
  const frames = [];
  let binding;
  const view = new EditorView({
    state: EditorState.create({
      doc: document.text(),
      extensions: [EditorView.updateListener.of((update) => binding?.applyViewUpdate(update))],
    }),
    parent: dom.window.document.body,
  });
  binding = bindCodeMirrorPlainText(document, view, { onLocalFrame: (frame) => frames.push(frame) });

  view.dispatch({ changes: { from: 0, to: 5, insert: "let" } });
  assert.equal(document.text(), "let answer = 42;");
  assert.equal(frames.length, 1);

  const remote = new FakeRGA(document.text());
  binding.applyRemote(remote.replace(0, 0, "// reviewed\n"));
  assert.equal(view.state.doc.toString(), "// reviewed\nlet answer = 42;");
  assert.equal(frames.length, 1);
  assert.equal(binding.destroy(), true);
  view.destroy();
});

test("actual CodeMirror 6 multi-range transaction retains one atomic RGA frame", () => {
  const document = new FakeRGA("abcd");
  const frames = [];
  let binding;
  const view = new EditorView({
    state: EditorState.create({
      doc: document.text(),
      extensions: [EditorView.updateListener.of((update) => binding?.applyViewUpdate(update))],
    }),
    parent: dom.window.document.body,
  });
  binding = bindCodeMirrorPlainText(document, view, { onLocalFrame: (frame) => frames.push(frame) });

  view.dispatch({
    changes: [
      { from: 0, to: 1, insert: "X" },
      { from: 2, to: 3, insert: "Y" },
    ],
  });
  assert.equal(document.text(), "XbYd");
  assert.equal(frames.length, 1);
  assert.equal(binding.destroy(), true);
  view.destroy();
});

test("actual Tiptap plain-text schema converges through RGA frames", () => {
  const document = new FakeRGA("draft");
  const editor = new Editor({
    element: dom.window.document.createElement("div"),
    extensions: [TiptapDocument, TiptapParagraph, TiptapText],
    content: tiptapJSON("draft"),
  });
  const frames = [];
  const binding = bindTiptapPlainText(document, editor, { onLocalFrame: (frame) => frames.push(frame) });

  assert.equal(editor.commands.setContent(tiptapJSON("draft review")), true);
  assert.equal(document.text(), "draft review");
  assert.equal(frames.length, 1);

  const remote = new FakeRGA(document.text());
  binding.applyRemote(remote.replace(0, 0, "remote "));
  assert.equal(editor.getJSON().content?.[0]?.content?.[0]?.text, "remote draft review");
  assert.equal(frames.length, 1);
  assert.equal(binding.destroy(), true);
  editor.destroy();
});

test("actual Tiptap schema preserves approved rich-text marks and atomic embeds without remote echo", () => {
  const Bold = Mark.create({ name: "bold", renderHTML: () => ["strong", 0] });
  const Mention = Node.create({
    name: "mention",
    group: "inline",
    inline: true,
    atom: true,
    addAttributes() {
      return { id: { default: null }, label: { default: null } };
    },
    renderHTML({ HTMLAttributes }) {
      return ["span", HTMLAttributes];
    },
  });
  const initial = {
    type: "doc",
    content: [{
      type: "paragraph",
      content: [
        { type: "text", text: "Hello ", marks: [{ type: "bold" }] },
        { type: "mention", attrs: { id: "u-7", label: "Ada" } },
      ],
    }],
  };
  const editor = new Editor({
    element: dom.window.document.createElement("div"),
    extensions: [TiptapDocument, TiptapParagraph, TiptapText, Bold, Mention],
    content: initial,
  });
  const document = new FakeRichText();
  const frames = [];
  const binding = bindTiptapRichText(document, editor, {
    initialContent: "editor",
    embeds: [tiptapMentionCodec],
    onLocalFrame: (frame) => frames.push(frame),
  });
  assert.equal(document.text(), "Hello \uFFFC\n");
  assert.equal(frames.length, 1);

  assert.equal(editor.commands.setContent({
    type: "doc",
    content: [{
      type: "paragraph",
      content: [
        { type: "text", text: "Hello remote ", marks: [{ type: "bold" }] },
        { type: "mention", attrs: { id: "u-7", label: "Ada" } },
      ],
    }],
  }), true);
  assert.equal(document.text(), "Hello remote \uFFFC\n");
  assert.equal(frames.length, 2);

  const remote = new FakeRichText(document.spans());
  binding.applyRemote(remote.applyEditorDelta([
    { insert: "Review: ", changes: [{ key: "rt.block", value: "paragraph" }] },
    { retain: Array.from(document.text()).length },
  ]));
  const remoteContent = editor.getJSON().content?.[0]?.content ?? [];
  assert.equal(remoteContent.filter((node) => node.type === "text").map((node) => node.text).join(""), "Review: Hello remote ");
  assert.equal(remoteContent.at(-1)?.type, "mention");
  assert.equal(frames.length, 2);
  assert.equal(binding.destroy(), true);
  editor.destroy();
});

test("actual Lexical headless plain-text leaf emits local state and suppresses remote listener echoes", async () => {
  const document = new FakeRGA("hello");
  const editor = createHeadlessEditor({
    namespace: "crdt-binding-real-test",
    onError(error) {
      throw error;
    },
  });
  const port = lexicalPlainTextPort(editor);
  port.replaceText("hello");
  const frames = [];
  const binding = bindLexicalPlainText(document, port, { onLocalFrame: (frame) => frames.push(frame) });

  port.replaceText("hello lexical");
  assert.equal(document.text(), "hello lexical");
  assert.equal(frames.length, 1);

  const remote = new FakeRGA(document.text());
  binding.applyRemote(remote.replace(0, 0, "remote "));
  await Promise.resolve();
  assert.equal(port.readText(), "remote hello lexical");
  assert.equal(frames.length, 1);
  assert.equal(binding.destroy(), true);
});

function lexicalPlainTextPort(editor) {
  return {
    readText() {
      let text = "";
      editor.getEditorState().read(() => {
        text = $getRoot().getTextContent();
      });
      return text;
    },
    replaceText(value) {
      editor.update(() => {
        const root = $getRoot();
        root.clear();
        for (const paragraphText of value.split("\n")) {
          const paragraph = $createParagraphNode();
          if (paragraphText !== "") {
            paragraph.append($createTextNode(paragraphText));
          }
          root.append(paragraph);
        }
      }, { discrete: true });
    },
    registerTextContentListener(listener) {
      return editor.registerTextContentListener(listener);
    },
  };
}

function tiptapJSON(value) {
  return {
    type: "doc",
    content: value.split("\n").map((paragraph) => ({
      type: "paragraph",
      content: paragraph === "" ? [] : [{ type: "text", text: paragraph }],
    })),
  };
}

class FakeRGA {
  constructor(text = "") {
    this.value = text;
    this.protocol = {
      maxLocalEditBytes: 64 * 1024,
      maxLocalEditRunes: 16 * 1024,
    };
  }

  text() {
    return this.value;
  }

  replace(offset, count, value) {
    const runes = Array.from(this.value);
    runes.splice(offset, count, ...Array.from(value));
    this.value = runes.join("");
    return new TextEncoder().encode(JSON.stringify({ offset, count, value }));
  }

  applyDelta(frame) {
    const { offset, count, value } = JSON.parse(new TextDecoder().decode(frame));
    this.replace(offset, count, value);
  }
}

const tiptapMentionCodec = {
  kind: "mention",
  nodeType: "mention",
  encode(node) {
    if (node.type !== "mention" || !isRecord(node.attrs) || typeof node.attrs.id !== "string" || typeof node.attrs.label !== "string") {
      throw new Error("invalid mention");
    }
    return { id: node.attrs.id, label: node.attrs.label };
  },
  decode(payload) {
    if (!isRecord(payload) || typeof payload.id !== "string" || typeof payload.label !== "string") {
      throw new Error("invalid mention payload");
    }
    return { type: "mention", attrs: { id: payload.id, label: payload.label } };
  },
};

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
          applyChanges(this.values[index].attributes, operation.changes ?? []);
        }
        offset += operation.retain;
      } else if (operation.delete !== undefined) {
        this.values.splice(offset, operation.delete);
      } else if (operation.insert !== undefined) {
        const attributes = {};
        applyChanges(attributes, operation.changes ?? []);
        this.values.splice(offset, 0, ...Array.from(operation.insert, (rune) => ({ rune, attributes: { ...attributes } })));
        offset += Array.from(operation.insert).length;
      } else {
        throw new Error("invalid operation");
      }
    }
    return new TextEncoder().encode(JSON.stringify(operations));
  }

  applyDelta(frame) {
    this.applyEditorDelta(JSON.parse(new TextDecoder().decode(frame)));
  }
}

function applyChanges(attributes, changes) {
  for (const change of changes) {
    if (change.remove) {
      delete attributes[change.key];
    } else {
      attributes[change.key] = change.value;
    }
  }
}

function sameAttributes(left, right) {
  const keys = Object.keys(left);
  return keys.length === Object.keys(right).length && keys.every((key) => left[key] === right[key]);
}

function isRecord(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
