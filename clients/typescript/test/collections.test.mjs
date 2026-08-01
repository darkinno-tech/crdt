import assert from "node:assert/strict";
import test from "node:test";

import {
  NativeCRDTError,
  NativeCollectionsDocument,
} from "../dist/index.js";

function assertCode(callback, code) {
  assert.throws(callback, (error) => error instanceof NativeCRDTError && error.code === code);
}

function recordUpdates(document) {
  const updates = [];
  document.onUpdate(({ update, local }) => {
    if (local) {
      updates.push(update);
    }
  });
  return updates;
}

function deliver(replicas, updates) {
  for (const update of updates) {
    for (const replica of replicas) {
      replica.applyUpdate(update, "test-transport");
    }
  }
}

test("PN-Counter converges after shuffled duplicate offline updates", () => {
  const alice = new NativeCollectionsDocument("alice");
  const bob = new NativeCollectionsDocument("bob");
  const carol = new NativeCollectionsDocument("carol");
  const counters = [alice, bob, carol].map((document) => document.getCounter("inventory"));
  const aliceUpdates = recordUpdates(alice);
  const bobUpdates = recordUpdates(bob);

  alice.transact(() => {
    counters[0].increment(12n);
    counters[0].decrement(3n);
  }, "warehouse-scan");
  bob.transact(() => {
    counters[1].increment(8n);
    counters[1].decrement(2n);
  }, "warehouse-scan");

  deliver([alice, bob, carol], [...bobUpdates, ...aliceUpdates].reverse());
  deliver([alice, bob, carol], [...aliceUpdates, ...bobUpdates]);
  assert.deepEqual(counters.map((counter) => counter.value()), [15n, 15n, 15n]);
  assert.deepEqual(counters.map((counter) => counter.componentCount()), [2, 2, 2]);
});

test("counter rejects a lower newer component without mutating accepted state", () => {
  const alice = new NativeCollectionsDocument("alice");
  const bob = new NativeCollectionsDocument("bob");
  const aliceCounter = alice.getCounter("inventory");
  const bobCounter = bob.getCounter("inventory");
  const updates = recordUpdates(alice);
  aliceCounter.increment(4n);
  bob.applyUpdate(updates[0]);
  aliceCounter.increment(3n);
  const forged = structuredClone(updates[1]);
  forged.operations[0].value.positive = "1";

  assertCode(() => bob.applyUpdate(forged), "state_conflict");
  assert.equal(bobCounter.value(), 4n);
});

test("OR-Set preserves remove-before-add tombstones and concurrent values", () => {
  const alice = new NativeCollectionsDocument("alice");
  const bob = new NativeCollectionsDocument("bob");
  const carol = new NativeCollectionsDocument("carol");
  const sets = [alice, bob, carol].map((document) => document.getORSet("open-tasks"));
  const aliceUpdates = recordUpdates(alice);
  const bobUpdates = recordUpdates(bob);

  sets[0].add({ id: "task-7", labels: ["urgent"] });
  assert.equal(sets[0].remove({ id: "task-7", labels: ["urgent"] }), true);
  sets[1].add({ id: "task-8", labels: ["offline"] });

  deliver([alice, bob, carol], [aliceUpdates[1], bobUpdates[0], aliceUpdates[0]]);
  deliver([alice, bob, carol], [aliceUpdates[1], bobUpdates[0], aliceUpdates[0]]);
  for (const set of sets) {
    assert.deepEqual(set.values(), [{ id: "task-8", labels: ["offline"] }]);
    assert.equal(set.tombstoneCount(), 1);
  }
});

test("LWW register selects a deterministic concurrent winner and retains a clear", () => {
  const alice = new NativeCollectionsDocument("alice");
  const bob = new NativeCollectionsDocument("bob");
  const target = new NativeCollectionsDocument("target");
  const registers = [alice, bob, target].map((document) => document.getLWWRegister("title"));
  const aliceUpdates = recordUpdates(alice);
  const bobUpdates = recordUpdates(bob);
  registers[0].set("alice draft");
  registers[1].set("bob draft");

  target.applyUpdate(aliceUpdates[0]);
  target.applyUpdate(bobUpdates[0]);
  assert.equal(registers[2].get(), "bob draft");
  const targetUpdates = recordUpdates(target);
  assert.equal(registers[2].clear(), true);
  assert.equal(targetUpdates.length, 1);
  alice.applyUpdate(targetUpdates[0]);
  bob.applyUpdate(targetUpdates[0]);
  assert.deepEqual(registers.map((register) => register.get()), [undefined, undefined, undefined]);
});

test("OR-Tree hides missing/deleted ancestors and converges deterministic siblings", () => {
  const alice = new NativeCollectionsDocument("alice");
  const bob = new NativeCollectionsDocument("bob");
  const aliceTree = alice.getORTree("outline");
  const bobTree = bob.getORTree("outline");
  const updates = recordUpdates(alice);
  const root = aliceTree.add(null, { kind: "document" });
  aliceTree.add(root, { kind: "paragraph", text: "first" });

  bob.applyUpdate(updates[1]);
  assert.equal(bobTree.pendingCount(), 1);
  assert.deepEqual(bobTree.roots(), []);
  bob.applyUpdate(updates[0]);
  assert.deepEqual(bobTree.roots(), [{
    id: root,
    value: { kind: "document" },
    children: [{ id: { actor: "alice", counter: 2 }, value: { kind: "paragraph", text: "first" }, children: [] }],
  }]);

  assert.equal(aliceTree.remove(root), true);
  bob.applyUpdate(updates[2]);
  assert.deepEqual(bobTree.roots(), []);
  assert.equal(bobTree.nodeCount(), 2);
  assert.equal(bobTree.tombstoneCount(), 1);
});

test("tree rejects a self-parenting forged node and local depth overflow without partial admission", () => {
  const alice = new NativeCollectionsDocument("alice", { collections: { maxTreeDepth: 2 } });
  const bob = new NativeCollectionsDocument("bob", { collections: { maxTreeDepth: 2 } });
  const aliceTree = alice.getORTree("outline");
  const bobTree = bob.getORTree("outline");
  const updates = recordUpdates(alice);
  const root = aliceTree.add(null, "root");
  const forged = structuredClone(updates[0]);
  forged.operations[0].value.parent = structuredClone(forged.operations[0].id);

  assertCode(() => bob.applyUpdate(forged), "state_conflict");
  assert.equal(bobTree.nodeCount(), 0);
  const child = aliceTree.add(root, "child");
  assertCode(() => aliceTree.add(child, "too-deep"), "resource_limit");
  assert.equal(aliceTree.nodeCount(), 2);
});

test("collection update listeners observe the committed cached tree projection", () => {
  const alice = new NativeCollectionsDocument("alice");
  const bob = new NativeCollectionsDocument("bob");
  const aliceTree = alice.getORTree("outline");
  const bobTree = bob.getORTree("outline");
  const aliceUpdates = recordUpdates(alice);
  const projections = [];
  bob.onUpdate(() => projections.push(bobTree.roots().map((node) => node.value)));
  aliceTree.add(null, "remote-root");

  bob.applyUpdate(aliceUpdates[0]);
  assert.deepEqual(projections, [["remote-root"]]);
});

test("OR-Set rejects a mismatched tombstone value before either map changes", () => {
  const alice = new NativeCollectionsDocument("alice");
  const bob = new NativeCollectionsDocument("bob");
  const aliceSet = alice.getORSet("tasks");
  const bobSet = bob.getORSet("tasks");
  const updates = recordUpdates(alice);
  aliceSet.add("approved");
  bob.applyUpdate(updates[0]);
  aliceSet.remove("approved");
  const forged = structuredClone(updates[1]);
  forged.operations[0].value.value = "other";

  assertCode(() => bob.applyUpdate(forged), "state_conflict");
  assert.deepEqual(bobSet.values(), ["approved"]);
  assert.equal(bobSet.tombstoneCount(), 0);
});

test("collection snapshots restore counters, set tombstones, LWW values, and tree IDs", () => {
  const source = new NativeCollectionsDocument("source");
  const counter = source.getCounter("visits");
  const set = source.getORSet("tags");
  const register = source.getLWWRegister("title");
  const tree = source.getORTree("outline");
  counter.increment(9n);
  set.add("remove-me");
  set.remove("remove-me");
  register.set("Recovered");
  const root = tree.add(null, "root");
  tree.add(root, "child");

  const restored = NativeCollectionsDocument.restore("source", source.snapshot());
  assert.equal(restored.getCounter("visits").value(), 9n);
  assert.deepEqual(restored.getORSet("tags").values(), []);
  assert.equal(restored.getLWWRegister("title").get(), "Recovered");
  assert.equal(restored.getORTree("outline").roots()[0].children[0].value, "child");

  restored.getCounter("visits").increment(1n);
  assert.equal(restored.getCounter("visits").value(), 10n);
});

test("unknown roots and wrong collection types fail closed before native map admission", () => {
  const alice = new NativeCollectionsDocument("alice");
  const bob = new NativeCollectionsDocument("bob");
  const updates = recordUpdates(alice);
  alice.getCounter("inventory").increment(1n);
  bob.getCounter("inventory");
  const unknownRoot = structuredClone(updates[0]);
  unknownRoot.operations[0].target = "metadata";
  assertCode(() => bob.applyUpdate(unknownRoot), "invalid_update");
  assert.equal(bob.getCounter("inventory").value(), 0n);
  assertCode(() => bob.getORSet("inventory"), "type_conflict");
});
