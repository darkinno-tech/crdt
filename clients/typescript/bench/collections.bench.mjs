import { performance } from "node:perf_hooks";

import { encodeNativeUpdate, NativeCollectionsDocument } from "../dist/index.js";

const SAMPLES = 5;
const EDITS_PER_REPLICA = 96;
const ACTORS = ["warehouse-tablet", "dispatcher-web", "driver-phone"];

console.log(`runtime=node-${process.version} workload=native-ts-collections-v1 controlled=true replicas=${ACTORS.length} edits_per_replica=${EDITS_PER_REPLICA}`);
for (let sample = 0; sample < SAMPLES; sample += 1) {
  const beforeHeap = process.memoryUsage().heapUsed;
  const started = performance.now();
  const result = runOfflineWorkboard();
  const elapsedMs = performance.now() - started;
  const heapDelta = process.memoryUsage().heapUsed - beforeHeap;
  console.log(
    `sample=${sample + 1} updates=${result.updates} bytes=${result.bytes} elapsed_ms=${elapsedMs.toFixed(2)} ms_per_update=${(elapsedMs / result.updates).toFixed(3)} heap_delta_b=${heapDelta}`,
  );
}

function runOfflineWorkboard() {
  const documents = ACTORS.map((actor) => new NativeCollectionsDocument(actor));
  const counters = documents.map((document) => document.getCounter("inspections"));
  const sets = documents.map((document) => document.getORSet("open-tasks"));
  const registers = documents.map((document) => document.getLWWRegister("shift-note"));
  const trees = documents.map((document) => document.getORTree("workboard"));
  const updates = [];
  for (const document of documents) {
    document.onUpdate(({ update, local }) => {
      if (local) updates.push(update);
    });
  }

  for (let turn = 0; turn < EDITS_PER_REPLICA; turn += 1) {
    for (let index = 0; index < documents.length; index += 1) {
      const task = `${ACTORS[index]}-${turn}`;
      documents[index].transact(() => {
        counters[index].increment(1n);
        if ((turn % 13) === 0) counters[index].decrement(1n);
        sets[index].add({ task, priority: turn % 5 });
        if ((turn % 7) === 0) sets[index].remove({ task, priority: turn % 5 });
        registers[index].set(`${ACTORS[index]}:${turn}`);
        const root = trees[index].add(null, { kind: "task", task });
        trees[index].add(root, { kind: "note", text: `inspection ${turn}` });
      }, "offline-workboard");
    }
  }

  let bytes = 0;
  for (let index = updates.length - 1; index >= 0; index -= 1) {
    const update = updates[index];
    bytes += encodeNativeUpdate(update).byteLength;
    for (const document of documents) {
      document.applyUpdate(update, "shuffled-transport");
      if ((index % 17) === 0) document.applyUpdate(update, "duplicate-transport");
    }
  }
  const expectedCounter = counters[0].value();
  const expectedTasks = JSON.stringify(sets[0].values());
  const expectedTrees = JSON.stringify(trees[0].roots());
  for (let index = 1; index < documents.length; index += 1) {
    if (counters[index].value() !== expectedCounter || JSON.stringify(sets[index].values()) !== expectedTasks || JSON.stringify(trees[index].roots()) !== expectedTrees) {
      throw new Error("offline workboard replicas did not converge");
    }
  }
  return { updates: updates.length, bytes };
}
