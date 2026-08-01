import { performance } from "node:perf_hooks";

import { MemoryNativeBrowserPersistence, openNativeBrowserDocument } from "../dist/browser.js";

const SAMPLES = 5;
const MUTATIONS = 512;

console.log(`runtime=node-${process.version} workload=native-browser-memory-persistence controlled=true`);
for (let sample = 0; sample < SAMPLES; sample += 1) {
  const persistence = new MemoryNativeBrowserPersistence();
  const started = performance.now();
  const document = await openNativeBrowserDocument({
    documentID: `benchmark-${sample}`,
    replicaID: `writer-${sample}`,
    persistence,
    persistenceLimits: {
      compactAfterUpdates: MUTATIONS + 1,
      compactAfterBytes: 32 << 20,
      maxUpdates: MUTATIONS + 1,
      maxBytes: 32 << 20,
    },
  });
  const metadata = document.getMap("metadata");
  for (let index = 0; index < MUTATIONS; index += 1) {
    metadata.set(`field-${index}`, index);
    await document.flush();
  }
  const persistedMs = performance.now() - started;
  await document.close();

  const recoverStarted = performance.now();
  const restored = await openNativeBrowserDocument({
    documentID: `benchmark-${sample}`,
    replicaID: `reader-${sample}`,
    persistence,
  });
  if (restored.getMap("metadata").size !== MUTATIONS) {
    throw new Error("browser persistence benchmark did not recover every field");
  }
  await restored.close();
  const recoveredMs = performance.now() - recoverStarted;
  const stored = await persistence.load(`benchmark-${sample}`);
  if (stored?.updates.length !== MUTATIONS) {
    throw new Error("browser persistence benchmark did not retain the expected append log");
  }
  console.log(
    `workload=append_flush_restore sample=${sample + 1} mutations=${MUTATIONS} retained_bytes=${stored.logBytes} append_ms=${persistedMs.toFixed(3)} append_ms_per_mutation=${(persistedMs / MUTATIONS).toFixed(4)} restore_ms=${recoveredMs.toFixed(3)}`,
  );

  const livePersistence = [new MemoryNativeBrowserPersistence(), new MemoryNativeBrowserPersistence()];
  const hub = createLiveHub();
  const left = await openNativeBrowserDocument({
    documentID: `live-benchmark-${sample}`,
    replicaID: `left-${sample}`,
    persistence: livePersistence[0],
    liveTransport: hub.createTransport(),
    persistenceLimits: {
      compactAfterUpdates: MUTATIONS + 1,
      compactAfterBytes: 32 << 20,
      maxUpdates: MUTATIONS + 1,
      maxBytes: 32 << 20,
    },
  });
  const right = await openNativeBrowserDocument({
    documentID: `live-benchmark-${sample}`,
    replicaID: `right-${sample}`,
    persistence: livePersistence[1],
    liveTransport: hub.createTransport(),
  });
  const liveMetadata = left.getMap("metadata");
  const liveStarted = performance.now();
  for (let index = 0; index < MUTATIONS; index += 1) {
    liveMetadata.set(`field-${index}`, index);
    await left.flush();
  }
  await waitFor(() => right.getMap("metadata").size === MUTATIONS);
  await right.flush();
  const liveMs = performance.now() - liveStarted;
  if (left.pendingOutbox !== MUTATIONS || right.pendingOutbox !== 0) {
    throw new Error("browser live benchmark crossed the durable receipt boundary");
  }
  console.log(
    `workload=append_flush_live sample=${sample + 1} mutations=${MUTATIONS} append_live_ms=${liveMs.toFixed(3)} append_live_ms_per_mutation=${(liveMs / MUTATIONS).toFixed(4)} sender_outbox=${left.pendingOutbox}`,
  );
  await left.close();
  await right.close();
}

function createLiveHub() {
  const receivers = new Map();
  return {
    createTransport() {
      const source = Symbol("live-benchmark-tab");
      return {
        publish(encoded) {
          const copy = encoded.slice();
          queueMicrotask(() => {
            for (const [target, receiver] of receivers) {
              if (target !== source) receiver(copy.slice());
            }
          });
        },
        subscribe(receiver) {
          receivers.set(source, receiver);
          return () => receivers.delete(source);
        },
      };
    },
  };
}

async function waitFor(predicate) {
  const deadline = performance.now() + 5_000;
  while (!predicate()) {
    if (performance.now() >= deadline) throw new Error("browser live benchmark delivery timed out");
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
}
