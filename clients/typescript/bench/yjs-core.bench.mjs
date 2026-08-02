import { performance } from "node:perf_hooks";

import * as Y from "yjs";

import {
  observeYjsDeep,
  YjsTextBinding,
} from "../dist/yjs.js";

const SAMPLES = 5;
const INITIAL_TEXT = "a".repeat(16 * 1024);
const ROUNDS = 256;
const limits = Object.freeze({
  maxUpdateBytes: 1 << 20,
  maxAwarenessBytes: 1 << 16,
  maxTextUTF16: 1 << 20,
  maxCursorBytes: 256,
});

function runStateVectorSyncSample() {
  const source = new Y.Doc();
  const target = new Y.Doc();
  const sourceText = source.getText("content");
  const targetText = target.getText("content");
  sourceText.insert(0, INITIAL_TEXT);
  const sourceBinding = new YjsTextBinding(source, sourceText, limits);
  const targetBinding = new YjsTextBinding(target, targetText, limits);
  const sourceSync = sourceBinding.createSyncProtocol({ maxMessageBytes: (1 << 20) + 16 });
  const targetSync = targetBinding.createSyncProtocol({ maxMessageBytes: (1 << 20) + 16 });
  const initial = sourceSync.receive(targetSync.encodeSyncStep1());
  if (initial === undefined || targetSync.receive(initial) !== undefined) {
    throw new Error("initial Yjs SyncStep1/2 exchange failed");
  }

  let totalDiffBytes = 0;
  const started = performance.now();
  for (let index = 0; index < ROUNDS; index += 1) {
    sourceText.insert(sourceText.length, String.fromCharCode(97 + (index % 26)));
    const response = sourceSync.receive(targetSync.encodeSyncStep1());
    if (response === undefined || targetSync.receive(response) !== undefined) {
      throw new Error("incremental Yjs SyncStep1/2 exchange failed");
    }
    totalDiffBytes += response.byteLength;
  }
  const elapsedMs = performance.now() - started;
  if (sourceText.toString() !== targetText.toString()) {
    throw new Error("state-vector benchmark did not converge");
  }
  sourceBinding.destroy();
  targetBinding.destroy();
  source.destroy();
  target.destroy();
  return { elapsedMs, averageDiffBytes: totalDiffBytes / ROUNDS };
}

function runDeepObserverSample() {
  const document = new Y.Doc();
  const board = document.getMap("board");
  const card = new Y.Map();
  board.set("card", card);
  let delivered = 0;
  const observer = observeYjsDeep(board, {
    maxEventsPerTransaction: 4,
    maxPathDepth: 2,
    onChanges(changes) {
      delivered += changes.length;
    },
  });
  const started = performance.now();
  for (let index = 0; index < ROUNDS; index += 1) {
    card.set("title", `revision-${index}`);
  }
  const elapsedMs = performance.now() - started;
  if (delivered !== ROUNDS) {
    throw new Error(`deep observer delivered ${delivered} events, expected ${ROUNDS}`);
  }
  observer.destroy();
  document.destroy();
  return { elapsedMs, delivered };
}

function runUndoRedoSample() {
  const document = new Y.Doc();
  const text = document.getText("content");
  text.insert(0, INITIAL_TEXT);
  const binding = new YjsTextBinding(document, text, limits);
  const undo = binding.createUndoManager({ captureTimeout: 0 });
  const started = performance.now();
  for (let index = 0; index < ROUNDS; index += 1) {
    binding.applyLocalReplacement({ from: text.length, to: text.length, insert: "x" });
    undo.stopCapturing();
  }
  for (let index = 0; index < ROUNDS; index += 1) {
    if (!undo.undo()) {
      throw new Error("undo stack ended before the benchmark completed");
    }
  }
  for (let index = 0; index < ROUNDS; index += 1) {
    if (!undo.redo()) {
      throw new Error("redo stack ended before the benchmark completed");
    }
  }
  const elapsedMs = performance.now() - started;
  if (text.length !== INITIAL_TEXT.length + ROUNDS) {
    throw new Error("undo/redo benchmark did not restore its final text length");
  }
  undo.destroy();
  binding.destroy();
  document.destroy();
  return { elapsedMs };
}

console.log(`runtime=node-${process.version} workload=yjs-core-controlled initial_utf16=${INITIAL_TEXT.length} rounds=${ROUNDS}`);
for (let sample = 1; sample <= SAMPLES; sample += 1) {
  const sync = runStateVectorSyncSample();
  const deep = runDeepObserverSample();
  const undo = runUndoRedoSample();
  console.log(
    `sample=${sample} sync_elapsed_ms=${sync.elapsedMs.toFixed(2)} sync_ms_per_round=${(sync.elapsedMs / ROUNDS).toFixed(3)} sync_avg_reply_bytes=${sync.averageDiffBytes.toFixed(1)} deep_elapsed_ms=${deep.elapsedMs.toFixed(2)} deep_ms_per_event=${(deep.elapsedMs / deep.delivered).toFixed(3)} undo_redo_elapsed_ms=${undo.elapsedMs.toFixed(2)} undo_redo_ms_per_operation=${(undo.elapsedMs / (ROUNDS * 3)).toFixed(3)}`,
  );
}
