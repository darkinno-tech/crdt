import assert from "node:assert/strict";
import test from "node:test";

import {
  decodeNativeUpdate,
  encodeNativeUpdate,
  NativeCRDTError,
  NativeDocument,
} from "../dist/native.js";

function assertCode(callback, code) {
  assert.throws(callback, (error) => error instanceof NativeCRDTError && error.code === code);
}

function recordUpdates(document) {
  const updates = [];
  document.onUpdate(({ update }) => updates.push(update));
  return updates;
}

function applyEverywhere(replicas, updates) {
  for (const update of updates) {
    for (const replica of replicas) {
      replica.applyUpdate(update);
    }
  }
}

test("NativeMap copies values, batches transaction observers, and resolves concurrent writes", () => {
  const alice = new NativeDocument("alice");
  const bob = new NativeDocument("bob");
  const aliceMap = alice.getMap("metadata");
  const bobMap = bob.getMap("metadata");
  const updates = recordUpdates(alice);
  const observerEvents = [];
  aliceMap.observe(({ update, origin }) => observerEvents.push({ update, origin }));

  const source = { nested: ["draft"] };
  alice.transact(() => {
    aliceMap.set("title", source);
    aliceMap.set("owner", "alice");
  }, "editor");
  source.nested[0] = "mutated outside";

  assert.equal(updates.length, 1);
  assert.equal(updates[0].operations.length, 2);
  assert.equal(observerEvents.length, 1);
  assert.equal(observerEvents[0].origin, "editor");
  assert.deepEqual(aliceMap.get("title"), { nested: ["draft"] });
  const copy = aliceMap.get("title");
  copy.nested[0] = "mutated returned copy";
  assert.deepEqual(aliceMap.get("title"), { nested: ["draft"] });
  const protoValue = JSON.parse('{"__proto__":{"polluted":true}}');
  aliceMap.set("__proto__", protoValue);
  assert.deepEqual(aliceMap.get("__proto__"), protoValue);
  assert.equal(Object.prototype.hasOwnProperty.call(aliceMap.toJSON(), "__proto__"), true);
  assert.equal({}.polluted, undefined);

  const bobUpdates = recordUpdates(bob);
  bobMap.set("title", "bob wins same counter by actor tie-break");
  bob.applyUpdate(updates[0]);
  alice.applyUpdate(bobUpdates[0]);
  assert.equal(aliceMap.get("title"), "bob wins same counter by actor tie-break");
});

test("NativeArray keeps tombstones and resolves a reversed parent chain", () => {
  const source = new NativeDocument("source");
  const updates = recordUpdates(source);
  source.getArray("tasks").push(["a", "b", "c"]);
  const insert = updates[0];
  const original = insert.operations[0];
  assert.equal(original.kind, "array-insert");
  const reversed = {
    version: 1,
    actor: "relay",
    operations: [{ ...original, entries: [...original.entries].reverse() }],
  };

  const target = new NativeDocument("target");
  const tasks = target.getArray("tasks");
  target.applyUpdate(reversed);
  assert.equal(tasks.pendingCount, 0);
  assert.deepEqual(tasks.toArray(), ["a", "b", "c"]);

  const deleteUpdates = recordUpdates(source);
  source.getArray("tasks").delete(1, 1);
  const deletion = deleteUpdates[0];
  const late = new NativeDocument("late");
  late.getArray("tasks");
  late.applyUpdate(deletion);
  late.applyUpdate(insert);
  assert.deepEqual(late.getArray("tasks").toArray(), ["a", "c"]);
});

test("native updates are canonical, bounded, and reject conflicts atomically", () => {
  const document = new NativeDocument("local", { maxPendingItems: 1 });
  const map = document.getMap("metadata");
  map.set("status", "safe");
  const [existing] = document.encodeStateAsUpdates();
  const operation = existing.operations[0];
  assert.equal(operation.kind, "map-set");
  const conflict = { ...operation, value: "tampered" };
  assertCode(
    () => document.applyUpdate({ version: 1, actor: "attacker", operations: [conflict] }),
    "state_conflict",
  );
  const reusedTag = { ...operation, key: "another-key" };
  assertCode(
    () => document.applyUpdate({ version: 1, actor: "attacker", operations: [reusedTag] }),
    "state_conflict",
  );
  assert.equal(map.get("status"), "safe");

  const cycle = {
    version: 1,
    actor: "attacker",
    operations: [
      {
        kind: "array-insert",
        target: "items",
        entries: [
          { id: { actor: "attacker", counter: 1 }, after: { actor: "attacker", counter: 2 }, value: "a" },
          { id: { actor: "attacker", counter: 2 }, after: { actor: "attacker", counter: 1 }, value: "b" },
        ],
      },
    ],
  };
  assertCode(() => document.applyUpdate(cycle), "state_conflict");
  assert.equal(document.getArray("items").length, 0);

  const excessPending = {
    version: 1,
    actor: "attacker",
    operations: [
      {
        kind: "array-insert",
        target: "pending",
        entries: [
          { id: { actor: "attacker", counter: 3 }, after: { actor: "missing", counter: 1 }, value: "a" },
          { id: { actor: "attacker", counter: 4 }, after: { actor: "missing", counter: 2 }, value: "b" },
        ],
      },
    ],
  };
  assertCode(() => document.applyUpdate(excessPending), "resource_limit");
  assert.equal(document.getArray("pending").pendingCount, 0);

  const encoded = encodeNativeUpdate(existing);
  assert.deepEqual(decodeNativeUpdate(encoded), existing);
  assertCode(() => decodeNativeUpdate(new TextEncoder().encode(JSON.stringify(existing))), "invalid_update");
});

test("native update decoder rejects malformed bytes with domain errors", () => {
  let random = 0x9e3779b9;
  for (let sample = 0; sample < 600; sample += 1) {
    random = (Math.imul(random, 1664525) + 1013904223) >>> 0;
    const bytes = new Uint8Array(random % 384);
    for (let index = 0; index < bytes.length; index += 1) {
      random = (Math.imul(random, 1664525) + 1013904223) >>> 0;
      bytes[index] = random & 0xff;
    }
    try {
      decodeNativeUpdate(bytes);
    } catch (error) {
      assert.ok(error instanceof NativeCRDTError, `sample ${sample} threw ${String(error)}`);
    }
  }
});

test("snapshots retain empty root type declarations", () => {
  const source = new NativeDocument("source");
  source.getMap("empty-map");
  source.getArray("empty-array");
  const restored = NativeDocument.restore("source", source.snapshot());
  assert.equal(restored.getMap("empty-map").size, 0);
  assert.equal(restored.getArray("empty-array").length, 0);
  assertCode(() => restored.getArray("empty-map"), "type_conflict");
  assertCode(() => restored.getMap("empty-array"), "type_conflict");
});

test("three offline editors converge after shuffled duplicate delivery and state recovery", () => {
  const replicas = [new NativeDocument("alice"), new NativeDocument("bob"), new NativeDocument("carol")];
  const maps = replicas.map((document) => document.getMap("metadata"));
  const arrays = replicas.map((document) => document.getArray("cards"));
  const queue = [];
  for (const replica of replicas) {
    replica.onUpdate(({ update }) => queue.push(update));
  }

  let random = 0x12345678;
  const next = () => {
    random = (Math.imul(random, 1664525) + 1013904223) >>> 0;
    return random;
  };
  for (let turn = 0; turn < 180; turn += 1) {
    const index = next() % replicas.length;
    const array = arrays[index];
    const map = maps[index];
    if ((next() % 3) === 0 && array.length !== 0) {
      array.delete(next() % array.length);
    } else if ((next() % 4) === 0) {
      map.set(`field-${next() % 7}`, { turn, actor: replicas[index].replicaID });
    } else {
      array.insert(next() % (array.length + 1), [`${replicas[index].replicaID}-${turn}`]);
    }
  }

  for (let index = queue.length - 1; index >= 0; index -= 1) {
    const update = queue[index];
    applyEverywhere(replicas, [update]);
    if ((index % 9) === 0) {
      applyEverywhere(replicas, [update]);
    }
  }

  const expectedCards = arrays[0].toArray();
  const expectedMetadata = maps[0].toJSON();
  for (let index = 1; index < replicas.length; index += 1) {
    assert.deepEqual(arrays[index].toArray(), expectedCards);
    assert.deepEqual(maps[index].toJSON(), expectedMetadata);
    assert.equal(arrays[index].pendingCount, 0);
  }

  const snapshot = replicas[0].snapshot();
  const restored = NativeDocument.restore("alice", snapshot);
  assert.deepEqual(restored.getArray("cards").toArray(), expectedCards);
  assert.deepEqual(restored.getMap("metadata").toJSON(), expectedMetadata);
  const recoveredUpdates = recordUpdates(restored);
  restored.getMap("metadata").set("after-recovery", true);
  const recoveredOperation = recoveredUpdates[0].operations[0];
  assert.equal(recoveredOperation.kind, "map-set");
  assert.ok(recoveredOperation.id.counter > snapshot.counter);
});
