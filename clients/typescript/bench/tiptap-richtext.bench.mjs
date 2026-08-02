import { performance } from "node:perf_hooks";

import { Editor, Mark, Node } from "@tiptap/core";
import TiptapDocument from "@tiptap/extension-document";
import TiptapParagraph from "@tiptap/extension-paragraph";
import TiptapText from "@tiptap/extension-text";

import { bindTiptapRichText } from "../dist/bindings.js";

const LOCAL_EDITS = 128;
const REMOTE_MERGES = 64;

function main() {
  const editor = createEditor(initialDocument());
  const document = new BenchmarkRichText();
  let frames = 0;
  const binding = bindTiptapRichText(document, editor, {
    initialContent: "editor",
    embeds: [mentionCodec],
    onLocalFrame() {
      frames++;
    },
  });
  const localStart = performance.now();
  for (let index = 0; index < LOCAL_EDITS; index++) {
    const next = structuredClone(editor.getJSON());
    const paragraph = next.content[31];
    paragraph.content[0].text = `review-${String(index).padStart(3, "0")} ${"x".repeat(120)}`;
    editor.commands.setContent(next);
  }
  const localMilliseconds = performance.now()-localStart;
  if (frames !== LOCAL_EDITS+1) {
    throw new Error(`expected ${LOCAL_EDITS+1} local frames, got ${frames}`);
  }

  const remote = new BenchmarkRichText(document.spans());
  const remoteStart = performance.now();
  for (let index = 0; index < REMOTE_MERGES; index++) {
    const frame = remote.applyEditorDelta([
      { retain: 32 },
      { insert: ".", changes: [{ key: "rt.block", value: "paragraph" }] },
      { retain: Array.from(remote.text()).length-32 },
    ]);
    binding.applyRemote(frame);
  }
  const remoteMilliseconds = performance.now()-remoteStart;
  if (frames !== LOCAL_EDITS+1 || document.text() !== remote.text()) {
    throw new Error("Tiptap rich-text benchmark diverged or echoed a remote frame");
  }

  console.log(JSON.stringify({
    scenario: "actual_tiptap_core_64_blocks_marked_text_and_atomic_embeds",
    localEdits: LOCAL_EDITS,
    localMilliseconds,
    localMillisecondsPerEdit: localMilliseconds/LOCAL_EDITS,
    remoteMerges: REMOTE_MERGES,
    remoteMilliseconds,
    remoteMillisecondsPerMerge: remoteMilliseconds/REMOTE_MERGES,
    frames,
    visibleRunes: Array.from(document.text()).length,
  }));

  binding.destroy();
  editor.destroy();
}

function createEditor(content) {
  const Bold = Mark.create({ name: "bold" });
  const Mention = Node.create({
    name: "mention",
    group: "inline",
    inline: true,
    atom: true,
    addAttributes() {
      return { id: { default: null }, label: { default: null } };
    },
  });
  return new Editor({ extensions: [TiptapDocument, TiptapParagraph, TiptapText, Bold, Mention], content });
}

function initialDocument() {
  return {
    type: "doc",
    content: Array.from({ length: 64 }, (_, index) => ({
      type: "paragraph",
      content: [
        { type: "text", text: `paragraph-${String(index).padStart(2, "0")} ${"x".repeat(120)}`, marks: index % 2 === 0 ? [{ type: "bold" }] : undefined },
        ...(index % 8 === 0 ? [{ type: "mention", attrs: { id: `u-${index}`, label: `User ${index}` } }] : []),
      ],
    })),
  };
}

const mentionCodec = {
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

class BenchmarkRichText {
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
        for (let index = offset; index < offset+operation.retain; index++) {
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

main();
