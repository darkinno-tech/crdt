import assert from "node:assert/strict";
import test from "node:test";

import {
  NativeNestedArray,
  NativeNestedDocument,
  NativeNestedMap,
} from "../dist/nested.js";
import { NativeCRDTError } from "../dist/native.js";

function assertCode(callback, code) {
  assert.throws(callback, (error) => error instanceof NativeCRDTError && error.code === code);
}

function recordLocalUpdates(document) {
  const updates = [];
  document.onUpdate(({ update, local }) => {
    if (local) {
      updates.push(update);
    }
  });
  return updates;
}

test("nested maps and arrays preserve independent recursive CRDT mutations", () => {
  const document = new NativeNestedDocument("alice");
  const updates = recordLocalUpdates(document);
  const root = document.getMap("workspace");

  document.transact(() => {
    root.set("title", "Roadmap");
    const board = root.createMap("board");
    const cards = board.createArray("cards");
    const card = cards.pushMap();
    card.set("id", "card-1");
    card.set("done", false);
    card.createArray("labels").push(["planning", "shared"]);
  }, "editor");

  assert.equal(updates.length, 1);
  assert.equal(updates[0].operations.length, 8);
  const board = root.get("board");
  assert.ok(board instanceof NativeNestedMap);
  const cards = board.get("cards");
  assert.ok(cards instanceof NativeNestedArray);
  const card = cards.get(0);
  assert.ok(card instanceof NativeNestedMap);
  assert.deepEqual(card.toJSON(), { done: false, id: "card-1", labels: ["planning", "shared"] });

  const copy = root.toJSON();
  copy.title = "changed outside";
  assert.equal(root.get("title"), "Roadmap");
});

test("nested child updates wait for their parent, then converge with duplicate reverse delivery", () => {
  const source = new NativeNestedDocument("source");
  const updates = recordLocalUpdates(source);
  const root = source.getMap("root");
  const child = root.createMap("child");
  child.set("status", "draft");
  const list = child.createArray("items");
  list.push(["a", "b"]);

  const target = new NativeNestedDocument("target");
  target.getMap("root");
  assert.equal(target.applyUpdate(updates[3]), false);
  assertCode(() => target.snapshot(), "invalid_update");
  target.applyUpdate(updates[0]);
  target.applyUpdate(updates[2]);
  target.applyUpdate(updates[1]);
  target.applyUpdate(updates[3]);
  target.applyUpdate(updates[0]);

  const targetChild = target.getMap("root").get("child");
  assert.ok(targetChild instanceof NativeNestedMap);
  assert.equal(targetChild.get("status"), "draft");
  const targetItems = targetChild.get("items");
  assert.ok(targetItems instanceof NativeNestedArray);
  assert.deepEqual(targetItems.toArray(), ["a", "b"]);
});

test("nested references reject ID mismatch, aliasing, and malformed marker values before mutation", () => {
  const document = new NativeNestedDocument("target");
  const root = document.getMap("root");
  const mismatched = {
    version: 1,
    actor: "attacker",
    operations: [{
      kind: "map-set",
      target: "root",
      key: "child",
      id: { actor: "attacker", counter: 1 },
      value: { $crdt: "native-ts-nested-v1", id: { actor: "attacker", counter: 2 }, type: "map" },
    }],
  };
  assertCode(() => document.applyUpdate(mismatched), "state_conflict");
  assert.equal(root.size, 0);

  const markerInsideJSON = {
    version: 1,
    actor: "attacker",
    operations: [{
      kind: "map-set",
      target: "root",
      key: "payload",
      id: { actor: "attacker", counter: 3 },
      value: { metadata: { $crdt: "native-ts-nested-v1", id: { actor: "attacker", counter: 3 }, type: "map" } },
    }],
  };
  assertCode(() => document.applyUpdate(markerInsideJSON), "invalid_update");
  assert.equal(root.size, 0);

  const source = new NativeNestedDocument("source");
  const updates = recordLocalUpdates(source);
  source.getMap("root").createMap("first");
  const valid = updates[0];
  document.applyUpdate(valid);
  const alias = {
    version: 1,
    actor: "attacker",
    operations: [{ ...valid.operations[0], key: "second" }],
  };
  assertCode(() => document.applyUpdate(alias), "state_conflict");
  assert.equal(root.has("second"), false);
});

test("nested update admission fuzzes malformed bytes without non-domain failures", () => {
  const document = new NativeNestedDocument("fuzz-target");
  document.getMap("root");
  let random = 0x7f4a7c15;
  for (let sample = 0; sample < 600; sample += 1) {
    random = (Math.imul(random, 1664525) + 1013904223) >>> 0;
    const bytes = new Uint8Array(random % 384);
    for (let index = 0; index < bytes.length; index += 1) {
      random = (Math.imul(random, 1664525) + 1013904223) >>> 0;
      bytes[index] = random & 0xff;
    }
    try {
      document.applyEncodedUpdate(bytes);
    } catch (error) {
      assert.ok(error instanceof NativeCRDTError, `sample ${sample} threw ${String(error)}`);
    }
  }
  assert.equal(document.getMap("root").size, 0);
});

test("nested snapshots restore container declarations and never reuse local IDs", () => {
  const source = new NativeNestedDocument("writer");
  const root = source.getMap("root");
  root.createMap("empty");
  const snapshot = source.snapshot();
  const restored = NativeNestedDocument.restore("writer", snapshot);
  const updates = recordLocalUpdates(restored);
  const newChild = restored.getMap("root").createArray("later");
  newChild.push(["recovered"]);

  const firstOperation = updates[0].operations[0];
  assert.equal(firstOperation.kind, "map-set");
  assert.ok(firstOperation.id.counter > snapshot.native.counter);
  assert.ok(restored.getMap("root").get("empty") instanceof NativeNestedMap);
  assert.deepEqual(restored.getMap("root").get("later").toArray(), ["recovered"]);
});

test("three offline nested editors converge across shuffled duplicate updates", () => {
  const replicas = ["alice", "bob", "carol"].map((id) => new NativeNestedDocument(id));
  const updates = [];
  for (const replica of replicas) {
    replica.getMap("workspace");
    replica.onUpdate(({ update, local }) => {
      if (local) {
        updates.push(update);
      }
    });
  }
  for (const [index, replica] of replicas.entries()) {
    const root = replica.getMap("workspace");
    const profile = root.createMap(`profile-${index}`);
    profile.set("editor", replica.replicaID);
    profile.createArray("recent").push([`draft-${index}`, `review-${index}`]);
  }
  for (let index = updates.length - 1; index >= 0; index -= 1) {
    for (const replica of replicas) {
      replica.applyUpdate(updates[index]);
      if ((index % 2) === 0) {
        replica.applyUpdate(updates[index]);
      }
    }
  }
  const expected = replicas[0].getMap("workspace").toJSON();
  for (const replica of replicas.slice(1)) {
    assert.deepEqual(replica.getMap("workspace").toJSON(), expected);
  }
});
