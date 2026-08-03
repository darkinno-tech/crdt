import assert from "node:assert/strict";
import test, { after } from "node:test";

import { Editor, Node } from "@tiptap/core";
import TiptapDocument from "@tiptap/extension-document";
import TiptapParagraph from "@tiptap/extension-paragraph";
import TiptapText from "@tiptap/extension-text";
import { JSDOM } from "jsdom";

import { NativeCollectionsDocument } from "../dist/collections.js";

const dom = new JSDOM("<!doctype html><html><body></body></html>");
dom.window.requestAnimationFrame = (callback) => setTimeout(() => callback(Date.now()), 0);
dom.window.cancelAnimationFrame = (handle) => clearTimeout(handle);
const previousDOMGlobals = new Map();
for (const [key, value] of Object.entries({
  document: dom.window.document,
  window: dom.window,
  MutationObserver: dom.window.MutationObserver,
  navigator: dom.window.navigator,
  requestAnimationFrame: dom.window.requestAnimationFrame,
  cancelAnimationFrame: dom.window.cancelAnimationFrame,
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

const OutlineHeading = Node.create({
  name: "outlineHeading",
  group: "block",
  content: "text*",
  addAttributes() {
    return { level: { default: 1 } };
  },
  renderHTML({ node }) {
    return [`h${node.attrs.level}`, 0];
  },
});

test("actual Tiptap projects a reordered native OR-Tree outline without a remote editor echo", () => {
  const alice = new NativeCollectionsDocument("alice");
  const bob = new NativeCollectionsDocument("bob");
  const aliceTree = alice.getORTree("outline");
  const bobTree = bob.getORTree("outline");
  const updates = [];
  let editorUpdates = 0;
  const editor = new Editor({
    element: dom.window.document.createElement("div"),
    extensions: [TiptapDocument, TiptapParagraph, TiptapText, OutlineHeading],
    content: emptyDocument(),
    onUpdate: () => { editorUpdates += 1; },
  });
  // NativeCollectionsDocument publishes after its collection caches commit.
  // Observe the host boundary rather than an internal map so this projection
  // never sees a half-committed remote tree update.
  const stop = bob.onUpdate(() => {
    const roots = bobTree.roots();
    if (roots.length !== 0) {
      assert.equal(editor.commands.setContent(outlineDocument(roots), { emitUpdate: false }), true);
    } else {
      assert.equal(editor.commands.setContent(emptyDocument(), { emitUpdate: false }), true);
    }
  });
  alice.onUpdate(({ update, local }) => {
    if (local) updates.push(update);
  });

  try {
    const section = aliceTree.add(null, { kind: "section", title: "Safety" });
    aliceTree.add(section, { kind: "paragraph", text: "Reject malformed remote values." });

    bob.applyUpdate(updates[1], "authenticated-test-peer");
    assert.equal(bobTree.pendingCount(), 1);
    assert.deepEqual(bobTree.roots(), []);
    bob.applyUpdate(updates[0], "authenticated-test-peer");
    bob.applyUpdate(updates[0], "authenticated-test-peer");
    bob.applyUpdate(updates[1], "authenticated-test-peer");

    const rendered = editor.getJSON();
    assert.equal(rendered.type, "doc");
    assert.equal(rendered.content?.[0]?.type, "outlineHeading");
    assert.equal(rendered.content?.[0]?.attrs?.level, 1);
    assert.equal(rendered.content?.[0]?.content?.[0]?.text, "Safety");
    assert.equal(rendered.content?.[1]?.type, "paragraph");
    assert.equal(rendered.content?.[1]?.content?.[0]?.text, "Reject malformed remote values.");
    assert.equal(editorUpdates, 0, "remote OR-Tree projections must not enter an editor-local update path");

    assert.equal(aliceTree.remove(section), true);
    bob.applyUpdate(updates[2], "authenticated-test-peer");
    assert.deepEqual(bobTree.roots(), []);
    assert.deepEqual(editor.getJSON(), emptyDocument());
    assert.equal(editorUpdates, 0);
  } finally {
    stop();
    editor.destroy();
  }
});

function emptyDocument() {
  return { type: "doc", content: [{ type: "paragraph" }] };
}

function outlineDocument(roots) {
  const content = [];
  for (const root of roots) appendOutlineNode(content, root, 1);
  return { type: "doc", content };
}

function appendOutlineNode(content, node, depth) {
  const value = node.value;
  if (value?.kind === "section" && typeof value.title === "string") {
    content.push({ type: "outlineHeading", attrs: { level: Math.min(depth, 6) }, content: textContent(value.title) });
  } else if (value?.kind === "paragraph" && typeof value.text === "string") {
    content.push({ type: "paragraph", content: textContent(value.text) });
  } else {
    throw new TypeError("unsupported outline value");
  }
  for (const child of node.children) appendOutlineNode(content, child, depth + 1);
}

function textContent(value) {
  return value === "" ? [] : [{ type: "text", text: value }];
}
