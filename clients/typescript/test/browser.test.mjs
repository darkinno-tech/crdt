import assert from "node:assert/strict";
import test from "node:test";

import {
  MemoryNativeBrowserPersistence,
  NativeBrowserError,
  openNativeBrowserDocument,
} from "../dist/browser.js";
import { decodeNativeUpdate, encodeNativeUpdate } from "../dist/native.js";

test("browser document atomically restores a local update and retries it from the durable outbox", async () => {
  const persistence = new MemoryNativeBrowserPersistence();
  const initial = await openNativeBrowserDocument({
    documentID: "roadmap",
    replicaID: "alice",
    persistence,
  });
  initial.getMap("metadata").set("title", "Offline roadmap");
  await initial.flush();
  assert.equal(initial.pendingOutbox, 1);
  await initial.close();

  const delivered = [];
  const resumed = await openNativeBrowserDocument({
    documentID: "roadmap",
    replicaID: "alice",
    persistence,
    transport: {
      async send(encoded) {
        delivered.push(encoded.slice());
      },
      subscribe() {
        return () => {};
      },
    },
  });
  assert.equal(resumed.getMap("metadata").get("title"), "Offline roadmap");
  await resumed.flush();
  assert.equal(resumed.pendingOutbox, 0);
  assert.equal(delivered.length, 1);

  const remotePersistence = new MemoryNativeBrowserPersistence();
  const remote = await openNativeBrowserDocument({
    documentID: "roadmap",
    replicaID: "bob",
    persistence: remotePersistence,
  });
  assert.equal(remote.applyEncodedUpdate(delivered[0]), true);
  await remote.flush();
  assert.equal(remote.getMap("metadata").get("title"), "Offline roadmap");
  await resumed.close();
  await remote.close();
});

test("browser document compacts only after receipt and restores the compacted state", async () => {
  const persistence = new MemoryNativeBrowserPersistence();
  const document = await openNativeBrowserDocument({
    documentID: "compact-me",
    replicaID: "writer",
    persistence,
    persistenceLimits: {
      compactAfterUpdates: 2,
      compactAfterBytes: 256,
      maxUpdates: 100,
      maxBytes: 1 << 20,
    },
    transport: {
      send() {},
      subscribe() {
        return () => {};
      },
    },
  });
  const metadata = document.getMap("metadata");
  metadata.set("title", "first");
  await document.flush();
  metadata.set("title", "second");
  await document.flush();

  const stored = await persistence.load("compact-me");
  assert.ok(stored);
  assert.equal(stored.updates.length, 0);
  assert.equal(stored.baseUpdates.length, 1);
  await document.close();

  const restored = await openNativeBrowserDocument({
    documentID: "compact-me",
    replicaID: "writer",
    persistence,
  });
  assert.equal(restored.getMap("metadata").get("title"), "second");
  await restored.close();
});

test("browser document leaves an outbox entry intact when a receipt transport fails", async () => {
  const persistence = new MemoryNativeBrowserPersistence();
  const document = await openNativeBrowserDocument({
    documentID: "retry-me",
    replicaID: "writer",
    persistence,
    transport: {
      send() {
        throw new Error("temporary network outage");
      },
      subscribe() {
        return () => {};
      },
    },
  });
  document.getMap("metadata").set("title", "keep retrying");
  await assert.rejects(
    () => document.flush(),
    (error) => error instanceof NativeBrowserError && error.code === "transport_failed",
  );
  assert.equal(document.pendingOutbox, 1);
  document.disconnect();
  await document.close();
});

test("three offline browser editors converge after reverse, duplicate delivery and recovery", async () => {
  const clients = await Promise.all(
    ["alice", "bob", "carol"].map((replicaID) =>
      openNativeBrowserDocument({
        documentID: "three-editors",
        replicaID,
        persistence: new MemoryNativeBrowserPersistence(),
      }),
    ),
  );
  const arrays = clients.map((client) => client.getArray("cards"));
  const maps = clients.map((client) => client.getMap("metadata"));
  const updates = [];
  for (const client of clients) {
    client.onUpdate((event) => {
      if (event.local) {
        updates.push(event.encoded);
      }
    });
  }

  let random = 0x91e10da5;
  const next = () => {
    random = (Math.imul(random, 1_664_525) + 1_013_904_223) >>> 0;
    return random;
  };
  for (let turn = 0; turn < 120; turn += 1) {
    const index = next() % clients.length;
    if ((next() % 5) === 0 && arrays[index].length > 0) {
      arrays[index].delete(next() % arrays[index].length);
    } else if ((next() % 4) === 0) {
      maps[index].set(`field-${next() % 8}`, { turn, actor: index });
    } else {
      arrays[index].insert(next() % (arrays[index].length + 1), [`${index}-${turn}`]);
    }
  }

  for (let index = updates.length - 1; index >= 0; index -= 1) {
    for (const client of clients) {
      client.applyEncodedUpdate(updates[index]);
      if ((index % 7) === 0) {
        client.applyEncodedUpdate(updates[index]);
      }
    }
  }
  await Promise.all(clients.map((client) => client.flush()));
  const expectedCards = arrays[0].toArray();
  const expectedMetadata = maps[0].toJSON();
  for (let index = 1; index < clients.length; index += 1) {
    assert.deepEqual(arrays[index].toArray(), expectedCards);
    assert.deepEqual(maps[index].toJSON(), expectedMetadata);
    assert.equal(arrays[index].pendingCount, 0);
  }

  const persisted = clients.map((client) => client.close());
  await Promise.all(persisted);
});

test("browser persistence errors before an unbounded offline log is accepted", async () => {
  const persistence = new MemoryNativeBrowserPersistence();
  const document = await openNativeBrowserDocument({
    documentID: "bounded-log",
    replicaID: "writer",
    persistence,
    persistenceLimits: {
      compactAfterUpdates: 1,
      compactAfterBytes: 1,
      maxUpdates: 1,
      maxBytes: 1 << 20,
    },
  });
  const metadata = document.getMap("metadata");
  metadata.set("one", 1);
  await document.flush();
  metadata.set("two", 2);
  await assert.rejects(
    () => document.flush(),
    (error) => error instanceof NativeBrowserError && error.code === "persistence_limit",
  );
  await assert.rejects(
    () => document.close(),
    (error) => error instanceof NativeBrowserError && error.code === "persistence_limit",
  );
});

test("a browser log retains an incomplete receive graph and compacts only after its parent arrives", async () => {
  const sourcePersistence = new MemoryNativeBrowserPersistence();
  const source = await openNativeBrowserDocument({
    documentID: "pending-source",
    replicaID: "source",
    persistence: sourcePersistence,
  });
  const sourceUpdates = [];
  source.onUpdate((event) => {
    if (event.local) {
      sourceUpdates.push(event.encoded);
    }
  });
  source.getArray("cards").push(["parent", "child"]);
  await source.flush();
  const update = decodeNativeUpdate(sourceUpdates[0]);
  const operation = update.operations[0];
  assert.equal(operation.kind, "array-insert");
  const [parent, child] = operation.entries;
  assert.ok(parent && child);

  const persistence = new MemoryNativeBrowserPersistence();
  const target = await openNativeBrowserDocument({
    documentID: "pending-target",
    replicaID: "target",
    persistence,
    persistenceLimits: {
      compactAfterUpdates: 1,
      compactAfterBytes: 1,
      maxUpdates: 100,
      maxBytes: 1 << 20,
    },
  });
  const childUpdate = encodeNativeUpdate({
    version: 1,
    actor: update.actor,
    operations: [{ kind: "array-insert", target: operation.target, entries: [child] }],
  });
  assert.equal(target.applyEncodedUpdate(childUpdate), true);
  await target.flush();
  assert.equal(target.getArray("cards").pendingCount, 1);
  assert.equal((await persistence.load("pending-target"))?.updates.length, 1);

  const parentUpdate = encodeNativeUpdate({
    version: 1,
    actor: update.actor,
    operations: [{ kind: "array-insert", target: operation.target, entries: [parent] }],
  });
  assert.equal(target.applyEncodedUpdate(parentUpdate), true);
  await target.flush();
  assert.deepEqual(target.getArray("cards").toArray(), ["parent", "child"]);
  assert.equal((await persistence.load("pending-target"))?.updates.length, 0);
  await source.close();
  await target.close();
});
