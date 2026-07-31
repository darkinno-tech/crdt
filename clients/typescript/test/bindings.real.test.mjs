import assert from "node:assert/strict";
import test, { after } from "node:test";

import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { createHeadlessEditor } from "@lexical/headless";
import { $createParagraphNode, $createTextNode, $getRoot } from "lexical";
import { JSDOM } from "jsdom";
import { Editor } from "@tiptap/core";
import TiptapDocument from "@tiptap/extension-document";
import TiptapParagraph from "@tiptap/extension-paragraph";
import TiptapText from "@tiptap/extension-text";

import {
  bindCodeMirrorPlainText,
  bindLexicalPlainText,
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
