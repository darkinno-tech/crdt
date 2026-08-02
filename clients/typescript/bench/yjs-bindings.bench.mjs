import { performance } from "node:perf_hooks";

import * as Y from "yjs";
import { YjsTextBinding } from "../dist/yjs.js";

const SAMPLES = 5;
const EDITS = 512;
const INITIAL_TEXT = "a".repeat(48 * 1024);
const limits = Object.freeze({
  maxUpdateBytes: 1 << 20,
  maxAwarenessBytes: 1 << 16,
  maxTextUTF16: 1 << 20,
  maxCursorBytes: 256,
});

function runIncrementalSample() {
  const source = new Y.Doc();
  const target = new Y.Doc();
  const sourceText = source.getText("content");
  sourceText.insert(0, INITIAL_TEXT);
  Y.applyUpdate(target, Y.encodeStateAsUpdate(source));
  const targetText = target.getText("content");
  const view = new TextPort(targetText.toString());
  const binding = new YjsTextBinding(target, targetText, {
    ...limits,
    onTextChanges(changes) {
      view.apply(changes);
    },
  });
  return runRemoteEdits(source, sourceText, target, targetText, view, binding);
}

function runFullProjectionSample() {
  const source = new Y.Doc();
  const target = new Y.Doc();
  const sourceText = source.getText("content");
  sourceText.insert(0, INITIAL_TEXT);
  Y.applyUpdate(target, Y.encodeStateAsUpdate(source));
  const targetText = target.getText("content");
  const view = new TextPort(targetText.toString());
  targetText.observe(() => {
    view.replaceAll(targetText.toString());
  });
  return runRemoteEdits(source, sourceText, target, targetText, view, undefined);
}

function runRemoteEdits(source, sourceText, target, targetText, view, binding) {
  const updates = [];
  source.on("update", (update) => updates.push(update.slice()));
  const beforeHeap = process.memoryUsage().heapUsed;
  const started = performance.now();
  for (let index = 0; index < EDITS; index += 1) {
    const offset = (index * 131) % sourceText.length;
    source.transact(() => {
      sourceText.delete(offset, 1);
      sourceText.insert(offset, String.fromCharCode(65 + (index % 26)));
    });
    const update = updates.shift();
    if (update === undefined) throw new Error("Yjs source did not emit an update");
    if (binding === undefined) {
      Y.applyUpdate(target, update);
    } else {
      binding.applyRemoteUpdate(update);
    }
  }
  const elapsedMs = performance.now() - started;
  const heapDelta = process.memoryUsage().heapUsed - beforeHeap;
  if (sourceText.toString() !== targetText.toString() || targetText.toString() !== view.value) {
    throw new Error("Yjs binding benchmark did not converge");
  }
  binding?.destroy();
  source.destroy();
  target.destroy();
  return { elapsedMs, heapDelta, fullWrites: view.fullWrites, incrementalWrites: view.incrementalWrites };
}

class TextPort {
  constructor(value) {
    this.value = value;
    this.fullWrites = 0;
    this.incrementalWrites = 0;
  }

  replaceAll(value) {
    this.value = value;
    this.fullWrites += 1;
  }

  apply(changes) {
    for (const change of [...changes].sort((left, right) => right.from - left.from)) {
      this.value = `${this.value.slice(0, change.from)}${change.insert}${this.value.slice(change.to)}`;
    }
    this.incrementalWrites += 1;
  }
}

console.log(`runtime=node-${process.version} workload=yjs-native-remote-text controlled=true initial_utf16=${INITIAL_TEXT.length} remote_edits=${EDITS}`);
for (const [scenario, run] of [
  ["incremental_ytext_delta", runIncrementalSample],
  ["full_text_projection_baseline", runFullProjectionSample],
]) {
  for (let sample = 0; sample < SAMPLES; sample += 1) {
    const result = run();
    console.log(
      `scenario=${scenario} sample=${sample + 1} elapsed_ms=${result.elapsedMs.toFixed(2)} ms_per_remote_merge=${(result.elapsedMs / EDITS).toFixed(3)} full_writes=${result.fullWrites} incremental_writes=${result.incrementalWrites} heap_delta_b=${result.heapDelta}`,
    );
  }
}
