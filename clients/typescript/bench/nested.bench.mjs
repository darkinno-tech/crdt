import { performance } from "node:perf_hooks";

import { encodeNativeUpdate, NativeNestedDocument } from "../dist/index.js";

const SAMPLES = 5;
const NESTED_CARDS = 64;

console.log(`runtime=node-${process.version} workload=native-ts-nested-v1 controlled=true`);
benchmark("build_merge_64_nested_cards", 3, runBuildAndMerge);
benchmark("snapshot_restore_64_nested_cards", 3, runSnapshotRestore);
benchmark("three_editor_shuffled_nested_session", 4, runThreeEditorSession);

function benchmark(name, iterations, operation) {
  for (let warmup = 0; warmup < 2; warmup += 1) operation();
  for (let sample = 0; sample < SAMPLES; sample += 1) {
    const beforeHeap = process.memoryUsage().heapUsed;
    const started = performance.now();
    let bytes = 0;
    for (let index = 0; index < iterations; index += 1) bytes = operation();
    const elapsedMs = performance.now() - started;
    const afterHeap = process.memoryUsage().heapUsed;
    console.log(`workload=${name} sample=${sample + 1} iterations=${iterations} bytes=${bytes} elapsed_ms=${elapsedMs.toFixed(2)} ms_per_operation=${(elapsedMs / iterations).toFixed(3)} heap_delta_b=${afterHeap - beforeHeap}`);
  }
}

function runBuildAndMerge() {
  const source = new NativeNestedDocument("source", { maxNestedTypes: 4_096 });
  const updates = recordLocalUpdates(source);
  const root = source.getMap("workspace");
  source.transact(() => {
    const cards = root.createArray("cards");
    for (let index = 0; index < NESTED_CARDS; index += 1) {
      const card = cards.pushMap();
      card.set("id", `card-${index}`);
      card.set("done", (index & 1) === 0);
      card.createArray("labels").push(["planning", "shared"]);
    }
  });
  const target = new NativeNestedDocument("target", { maxNestedTypes: 4_096 });
  target.getMap("workspace");
  target.applyUpdate(updates[0]);
  const cards = target.getMap("workspace").get("cards");
  if (cards.length !== NESTED_CARDS || cards.get(32).get("id") !== "card-32") throw new Error("nested merge did not converge");
  return encodeNativeUpdate(updates[0]).byteLength;
}

function runSnapshotRestore() {
  const source = populatedDocument("snapshot", NESTED_CARDS);
  const snapshot = source.snapshot();
  const restored = NativeNestedDocument.restore("snapshot", snapshot, { maxNestedTypes: 4_096 });
  const cards = restored.getMap("workspace").get("cards");
  if (cards.length !== NESTED_CARDS || cards.get(NESTED_CARDS - 1).get("id") !== `card-${NESTED_CARDS - 1}`) throw new Error("nested snapshot did not recover");
  return snapshot.native.updates.reduce((total, update) => total + encodeNativeUpdate(update).byteLength, 0);
}

function runThreeEditorSession() {
  const replicas = ["alice", "bob", "carol"].map((id) => new NativeNestedDocument(id, { maxNestedTypes: 2_048 }));
  const updates = [];
  for (const replica of replicas) {
    replica.getMap("workspace");
    replica.onUpdate(({ update, local }) => { if (local) updates.push(update); });
  }
  for (let turn = 0; turn < 96; turn += 1) {
    const replica = replicas[turn % replicas.length];
    const profile = replica.getMap("workspace").createMap(`${replica.replicaID}-${turn}`);
    profile.set("turn", turn);
    profile.createArray("events").push(["created", `turn-${turn}`]);
  }
  let bytes = 0;
  for (let index = updates.length - 1; index >= 0; index -= 1) {
    bytes += encodeNativeUpdate(updates[index]).byteLength;
    for (const replica of replicas) {
      replica.applyUpdate(updates[index]);
      if ((index % 11) === 0) replica.applyUpdate(updates[index]);
    }
  }
  const expected = JSON.stringify(replicas[0].getMap("workspace").toJSON());
  for (const replica of replicas.slice(1)) {
    if (JSON.stringify(replica.getMap("workspace").toJSON()) !== expected) throw new Error("nested simulation did not converge");
  }
  return bytes;
}

function populatedDocument(replicaID, cards) {
  const document = new NativeNestedDocument(replicaID, { maxNestedTypes: 4_096 });
  const root = document.getMap("workspace");
  document.transact(() => {
    const list = root.createArray("cards");
    for (let index = 0; index < cards; index += 1) {
      const card = list.pushMap();
      card.set("id", `card-${index}`);
      card.createArray("labels").push(["benchmark"]);
    }
  });
  return document;
}

function recordLocalUpdates(document) {
  const updates = [];
  document.onUpdate(({ update, local }) => { if (local) updates.push(update); });
  return updates;
}
