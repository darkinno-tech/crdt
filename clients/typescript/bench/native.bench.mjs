import { performance } from "node:perf_hooks";

import { encodeNativeUpdate, NativeDocument } from "../dist/native.js";

const SAMPLES = 5;

console.log(`runtime=node-${process.version} workload=native-ts-v1 controlled=true`);
benchmark("cold_append_and_encoded_merge_4096", 6, runAppendAndMerge);
benchmark("cold_middle_insert_and_encoded_merge_4096", 8, runMiddleInsertAndMerge);
benchmark("shuffled_duplicate_three_editor_session", 6, runShuffledThreeEditorSession);
benchmark("cold_state_encode_and_restore_4096", 6, runStateEncodeAndRestore);

function benchmark(name, iterations, operation) {
  for (let warmup = 0; warmup < 2; warmup += 1) {
    operation();
  }
  for (let sample = 0; sample < SAMPLES; sample += 1) {
    const beforeHeap = process.memoryUsage().heapUsed;
    const started = performance.now();
    let bytes = 0;
    for (let index = 0; index < iterations; index += 1) {
      bytes = operation();
    }
    const elapsedMs = performance.now() - started;
    const afterHeap = process.memoryUsage().heapUsed;
    console.log(
      `workload=${name} sample=${sample + 1} iterations=${iterations} bytes=${bytes} elapsed_ms=${elapsedMs.toFixed(2)} ms_per_operation=${(elapsedMs / iterations).toFixed(3)} heap_delta_b=${afterHeap - beforeHeap}`,
    );
  }
}

function runAppendAndMerge() {
  const source = new NativeDocument("append-source");
  const target = new NativeDocument("append-target");
  const updates = recordUpdates(source);
  const values = Array.from({ length: 4096 }, (_, index) => `card-${index}`);
  source.getArray("cards").push(values);
  const encoded = encodeNativeUpdate(updates[0]);
  target.applyEncodedUpdate(encoded);
  if (target.getArray("cards").length !== values.length) {
    throw new Error("append benchmark did not converge");
  }
  return encoded.byteLength;
}

function runMiddleInsertAndMerge() {
  const source = new NativeDocument("middle-source");
  const target = new NativeDocument("middle-target");
  const updates = recordUpdates(source);
  const cards = source.getArray("cards");
  const values = Array.from({ length: 4096 }, (_, index) => `card-${index}`);
  cards.push(values);
  target.applyUpdate(updates.shift());
  cards.insert(2048, ["middle"]);
  const encoded = encodeNativeUpdate(updates[0]);
  target.applyEncodedUpdate(encoded);
  const merged = target.getArray("cards");
  if (merged.length !== values.length + 1 || merged.get(2048) !== "middle") {
    throw new Error("middle-insert benchmark did not converge");
  }
  return encoded.byteLength;
}

function runShuffledThreeEditorSession() {
  const documents = [new NativeDocument("alice"), new NativeDocument("bob"), new NativeDocument("carol")];
  const arrays = documents.map((document) => document.getArray("cards"));
  const updates = [];
  for (const document of documents) {
    document.onUpdate(({ update }) => updates.push(update));
  }
  for (let turn = 0; turn < 192; turn += 1) {
    const index = turn % documents.length;
    const array = arrays[index];
    if ((turn % 5) === 0 && array.length !== 0) {
      array.delete((turn * 17) % array.length);
    } else {
      array.insert((turn * 13) % (array.length + 1), [`${documents[index].replicaID}-${turn}`]);
    }
  }
  let bytes = 0;
  for (let index = updates.length - 1; index >= 0; index -= 1) {
    const encoded = encodeNativeUpdate(updates[index]);
    bytes += encoded.byteLength;
    for (const document of documents) {
      document.applyEncodedUpdate(encoded);
      if ((index % 11) === 0) {
        document.applyEncodedUpdate(encoded);
      }
    }
  }
  const expected = arrays[0].toArray();
  for (const array of arrays.slice(1)) {
    if (JSON.stringify(array.toArray()) !== JSON.stringify(expected) || array.pendingCount !== 0) {
      throw new Error("shuffled three-editor benchmark did not converge");
    }
  }
  return bytes;
}

function runStateEncodeAndRestore() {
  const source = new NativeDocument("state-source");
  source.getArray("cards").push(Array.from({ length: 4096 }, (_, index) => ({ id: index, done: (index & 1) === 0 })));
  source.getMap("metadata").set("title", "4096-card state");
  const snapshot = source.snapshot();
  const restored = NativeDocument.restore("state-source", snapshot);
  if (restored.getArray("cards").length !== 4096 || restored.getMap("metadata").get("title") !== "4096-card state") {
    throw new Error("state recovery benchmark did not converge");
  }
  return snapshot.updates.reduce((total, update) => total + encodeNativeUpdate(update).byteLength, 0);
}

function recordUpdates(document) {
  const updates = [];
  document.onUpdate(({ update }) => updates.push(update));
  return updates;
}
