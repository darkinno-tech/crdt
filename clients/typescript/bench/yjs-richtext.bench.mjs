import { performance } from "node:perf_hooks";

import * as Y from "yjs";

import { YjsRichTextBinding } from "../dist/index.js";

const SAMPLES = 5;
const REMOTE_FORMAT_EDITS = 512;
const INITIAL_TEXT = `${"a".repeat(16 * 1024)}\n`;
const limits = Object.freeze({
  maxUpdateBytes: 1 << 20,
  maxTextUTF16: 1 << 20,
  maxDeltaOperations: 64,
  maxAttributesPerOperation: 8,
  maxAttributeKeyBytes: 64,
  maxAttributeValueBytes: 1024,
  maxEmbedBytes: 4096,
  allowedAttributes: ["bold"],
  allowedEmbeds: [],
});

function runIncrementalDeltaSample() {
  const author = new Y.Doc();
  const authorText = author.getText("content");
  authorText.insert(0, INITIAL_TEXT);
  const replica = new Y.Doc();
  Y.applyUpdate(replica, Y.encodeStateAsUpdate(author));
  const replicaText = replica.getText("content");
  const editor = new DeltaPort(deltaFrom(replicaText));
  const binding = new YjsRichTextBinding(replica, replicaText, {
    ...limits,
    onRemoteDelta(delta) {
      editor.updateContents(delta);
    },
  });
  author.on("update", (update) => binding.applyRemoteUpdate(update));
  const result = runRemoteFormatting(author, authorText, replicaText, editor);
  binding.destroy();
  author.destroy();
  replica.destroy();
  editor.destroy();
  return result;
}

function runFullProjectionSample() {
  const author = new Y.Doc();
  const authorText = author.getText("content");
  authorText.insert(0, INITIAL_TEXT);
  const replica = new Y.Doc();
  Y.applyUpdate(replica, Y.encodeStateAsUpdate(author));
  const replicaText = replica.getText("content");
  const editor = new DeltaPort(deltaFrom(replicaText));
  replicaText.observe(() => editor.setContents(deltaFrom(replicaText)));
  author.on("update", (update) => Y.applyUpdate(replica, update));
  const result = runRemoteFormatting(author, authorText, replicaText, editor);
  author.destroy();
  replica.destroy();
  editor.destroy();
  return result;
}

function runRemoteFormatting(author, authorText, replicaText, editor) {
  const beforeHeap = process.memoryUsage().heapUsed;
  const started = performance.now();
  for (let index = 0; index < REMOTE_FORMAT_EDITS; index += 1) {
    const offset = (index * 31) % (authorText.length - 1);
    authorText.format(offset, 1, { bold: true });
  }
  const elapsedMs = performance.now() - started;
  const heapDelta = process.memoryUsage().heapUsed - beforeHeap;
  if (JSON.stringify(deltaFrom(authorText)) !== JSON.stringify(deltaFrom(replicaText)) ||
    JSON.stringify(deltaFrom(replicaText)) !== JSON.stringify(editor.getContents())) {
    throw new Error("Yjs rich-text binding benchmark did not converge");
  }
  return {
    elapsedMs,
    heapDelta,
    fullWrites: editor.fullWrites,
    incrementalWrites: editor.incrementalWrites,
  };
}

function deltaFrom(text) {
  return { ops: text.toDelta().map((operation) => structuredClone(operation)) };
}

class DeltaPort {
  constructor(initial) {
    this.document = new Y.Doc();
    this.text = this.document.getText("content");
    this.fullWrites = 0;
    this.incrementalWrites = 0;
    this.setContents(initial, false);
  }

  getContents() {
    return deltaFrom(this.text);
  }

  setContents(delta, measured = true) {
    this.document.transact(() => {
      if (this.text.length > 0) this.text.delete(0, this.text.length);
      this.text.applyDelta(delta.ops);
    });
    if (measured) this.fullWrites += 1;
  }

  updateContents(delta) {
    this.text.applyDelta(delta.ops);
    this.incrementalWrites += 1;
  }

  destroy() {
    this.document.destroy();
  }
}

console.log(`runtime=node-${process.version} workload=yjs-native-richtext-delta controlled=true initial_utf16=${INITIAL_TEXT.length} remote_format_edits=${REMOTE_FORMAT_EDITS}`);
for (const [scenario, run] of [
  ["incremental_ytext_delta", runIncrementalDeltaSample],
  ["full_delta_projection_baseline", runFullProjectionSample],
]) {
  for (let sample = 1; sample <= SAMPLES; sample += 1) {
    const result = run();
    console.log(
      `scenario=${scenario} sample=${sample} elapsed_ms=${result.elapsedMs.toFixed(2)} ms_per_remote_merge=${(result.elapsedMs / REMOTE_FORMAT_EDITS).toFixed(3)} full_writes=${result.fullWrites} incremental_writes=${result.incrementalWrites} heap_delta_b=${result.heapDelta}`,
    );
  }
}
