import { performance } from "node:perf_hooks";

import { BlockNoteEditor } from "@blocknote/core";

import { bindBlockNoteRichText } from "../dist/bindings.js";

const SAMPLES = 5;
const BLOCKS = 128;
const RUNES_PER_BLOCK = 256;
const LOCAL_EDITS = 256;
const REMOTE_EDITS = 128;

function runSimulatedSample() {
  const port = new FakeBlockNotePort(initialBlocks());
  const document = new BenchmarkRichText();
  let emittedFrames = 0;
  const binding = bindBlockNoteRichText(document, port, { initialContent: "editor", onLocalFrame() { emittedFrames++; } });
  emittedFrames = 0;
  const beforeHeap = process.memoryUsage().heapUsed;

  const localStarted = performance.now();
  for (let index = 0; index < LOCAL_EDITS; index++) {
    const next = structuredClone(port.document);
    const block = next[index % BLOCKS];
    const current = block.content.map((item) => item.text).join("");
    block.content = [{ type: "text", text: replaceAt(current, (index * 61) % current.length, String.fromCharCode(65 + (index % 26))), styles: {} }];
    port.userReplace(next);
  }
  const localMs = performance.now() - localStarted;
  if (document.text() !== textFromBlocks(port.document) || emittedFrames !== LOCAL_EDITS) {
    throw new Error("simulated BlockNote local workload diverged");
  }

  const remote = new BenchmarkRichText(document.spans());
  const remoteStarted = performance.now();
  for (let index = 0; index < REMOTE_EDITS; index++) {
    const offset = editableOffset(remote.values, (index * 89) % remote.values.length);
    const frame = remote.replace(offset, String.fromCharCode(97 + (index % 26)));
    binding.applyRemote(frame);
  }
  const remoteMs = performance.now() - remoteStarted;
  if (document.text() !== remote.text() || document.text() !== textFromBlocks(port.document) || emittedFrames !== LOCAL_EDITS) {
    throw new Error("simulated BlockNote remote workload echoed or diverged");
  }
  const heapDelta = process.memoryUsage().heapUsed - beforeHeap;
  binding.destroy();
  return { localMs, remoteMs, emittedFrames, heapDelta };
}

function runRealCoreSample() {
  const editor = BlockNoteEditor.create({ initialContent: initialBlocks() });
  const document = new BenchmarkRichText();
  let emittedFrames = 0;
  const binding = bindBlockNoteRichText(document, editor, { initialContent: "editor", onLocalFrame() { emittedFrames++; } });
  emittedFrames = 0;
  const beforeHeap = process.memoryUsage().heapUsed;

  const localStarted = performance.now();
  for (let index = 0; index < LOCAL_EDITS; index++) {
    const block = editor.document[index % BLOCKS];
    const current = block.content.map((item) => item.text).join("");
    editor.updateBlock(block, { content: replaceAt(current, (index * 61) % current.length, String.fromCharCode(65 + (index % 26))) });
  }
  const localMs = performance.now() - localStarted;
  if (document.text() !== textFromBlocks(editor.document) || emittedFrames !== LOCAL_EDITS) {
    throw new Error("BlockNote core local workload diverged");
  }

  const remote = new BenchmarkRichText(document.spans());
  const remoteStarted = performance.now();
  for (let index = 0; index < REMOTE_EDITS; index++) {
    const offset = editableOffset(remote.values, (index * 89) % remote.values.length);
    binding.applyRemote(remote.replace(offset, String.fromCharCode(97 + (index % 26))));
  }
  const remoteMs = performance.now() - remoteStarted;
  if (document.text() !== remote.text() || document.text() !== textFromBlocks(editor.document) || emittedFrames !== LOCAL_EDITS) {
    throw new Error("BlockNote core remote workload echoed or diverged");
  }
  const heapDelta = process.memoryUsage().heapUsed - beforeHeap;
  binding.destroy();
  return { localMs, remoteMs, emittedFrames, heapDelta };
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

class BenchmarkRichText {
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
        for (let index = offset; index < offset + operation.retain; index++) applyChanges(this.values[index].attributes, operation.changes ?? []);
        offset += operation.retain;
      } else if (operation.delete !== undefined) {
        this.values.splice(offset, operation.delete);
      } else {
        const values = Array.from(operation.insert).map((rune) => ({ rune, attributes: attributesFromChanges(operation.changes ?? []) }));
        this.values.splice(offset, 0, ...values);
        offset += values.length;
      }
    }
    return new TextEncoder().encode(JSON.stringify(operations));
  }

  applyDelta(frame) {
    this.applyEditorDelta(JSON.parse(new TextDecoder().decode(frame)));
  }

  replace(offset, value) {
    const attributes = this.values[offset].attributes;
    return this.applyEditorDelta([
      ...(offset === 0 ? [] : [{ retain: offset }]),
      { delete: 1 },
      { insert: value, changes: Object.entries(attributes).map(([key, entry]) => ({ key, value: entry })) },
    ]);
  }
}

function initialBlocks() {
  return Array.from({ length: BLOCKS }, (_, index) => ({
    type: index % 8 === 0 ? "heading" : "paragraph",
    props: index % 8 === 0
      ? { backgroundColor: "default", textColor: "default", textAlignment: "left", level: 2, isToggleable: false }
      : { backgroundColor: "default", textColor: "default", textAlignment: "left" },
    content: [{ type: "text", text: String.fromCharCode(97 + (index % 26)).repeat(RUNES_PER_BLOCK), styles: {} }],
    children: [],
  }));
}

function textFromBlocks(blocks) {
  const values = [];
  const visit = (block) => {
    values.push(block.content.map((item) => item.text).join(""), "\n");
    for (const child of block.children ?? []) visit(child);
  };
  for (const block of blocks) visit(block);
  return values.join("");
}

function editableOffset(values, start) {
  for (let index = 0; index < values.length; index++) {
    const offset = (start + index) % values.length;
    if (values[offset].rune !== "\n") return offset;
  }
  throw new Error("benchmark document has no editable text");
}

function replaceAt(value, offset, replacement) {
  return `${value.slice(0, offset)}${replacement}${value.slice(offset + 1)}`;
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

console.log(`runtime=node-${process.version} workload=blocknote-richtext controlled=true blocks=${BLOCKS} initial_runes=${BLOCKS * (RUNES_PER_BLOCK + 1)} local_edits=${LOCAL_EDITS} remote_edits=${REMOTE_EDITS}`);
for (const [name, run] of [["simulated_port", runSimulatedSample], ["blocknote_core", runRealCoreSample]]) {
  for (let sample = 0; sample < SAMPLES; sample++) {
    const result = run();
    console.log(`scenario=${name} sample=${sample + 1} local_ms=${result.localMs.toFixed(2)} remote_ms=${result.remoteMs.toFixed(2)} local_ms_per_edit=${(result.localMs / LOCAL_EDITS).toFixed(3)} remote_ms_per_edit=${(result.remoteMs / REMOTE_EDITS).toFixed(3)} emitted_frames=${result.emittedFrames} heap_delta_b=${result.heapDelta}`);
  }
}
