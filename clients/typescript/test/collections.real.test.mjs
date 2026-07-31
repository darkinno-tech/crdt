import assert from "node:assert/strict";
import test from "node:test";

import { NativeCollectionsDocument } from "../dist/index.js";

test("real BroadcastChannel transport converges a multi-type offline workboard without echo", async () => {
  const suffix = `${process.pid}-${Date.now()}-${Math.random().toString(16).slice(2)}`;
  const replicas = ["warehouse", "dispatcher", "driver"].map((actor) => new NativeCollectionsDocument(actor));
  const counters = replicas.map((document) => document.getCounter("inspections"));
  const sets = replicas.map((document) => document.getORSet("open-tasks"));
  const registers = replicas.map((document) => document.getLWWRegister("status"));
  const trees = replicas.map((document) => document.getORTree("workboard"));
  const channels = replicas.map(() => new BroadcastChannel(`darkinno-crdt-collections-${suffix}`));

  try {
    for (let index = 0; index < replicas.length; index += 1) {
      channels[index].onmessage = ({ data }) => replicas[index].applyUpdate(data, "BroadcastChannel");
      replicas[index].onUpdate(({ update, local }) => {
        if (!local) return;
        channels[index].postMessage(update);
      });
    }

    replicas[0].transact(() => {
      counters[0].increment(2n);
      sets[0].add({ id: "task-9", source: "warehouse" });
      registers[0].set("inspection-started");
      const root = trees[0].add(null, { kind: "report" });
      trees[0].add(root, { kind: "finding" });
    }, "real-channel-workflow");
    counters[1].increment(3n);
    sets[2].add({ id: "task-10", source: "driver" });

    await eventually(() => counters.every((counter) => counter.value() === 5n)
      && sets.every((set) => set.size === 2)
      && registers.every((register) => register.get() === "inspection-started")
      && trees.every((tree) => tree.roots().length === 1 && tree.roots()[0].children.length === 1));
    assert.deepEqual(sets.map((set) => set.values()), [
      [{ id: "task-10", source: "driver" }, { id: "task-9", source: "warehouse" }],
      [{ id: "task-10", source: "driver" }, { id: "task-9", source: "warehouse" }],
      [{ id: "task-10", source: "driver" }, { id: "task-9", source: "warehouse" }],
    ]);
  } finally {
    for (const channel of channels) channel.close();
  }
});

async function eventually(predicate) {
  const deadline = Date.now() + 2_000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  assert.fail("BroadcastChannel replicas did not converge before timeout");
}
