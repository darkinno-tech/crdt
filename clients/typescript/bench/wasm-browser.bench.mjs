import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { performance } from "node:perf_hooks";
import { pathToFileURL } from "node:url";

import { MemoryRGAWasmBrowserPersistence, openRGAWasmBrowserDocument } from "../dist/browser.js";
import { RGAWasmRuntime } from "../dist/wasm.js";

const wasmDirectory = process.env.CRDT_WASM_DIR;
if (typeof wasmDirectory !== "string" || wasmDirectory === "") {
  throw new Error("CRDT_WASM_DIR must point to artifacts built by make wasm");
}

const SAMPLES = 5;
const MUTATIONS = 256;

await import(pathToFileURL(join(wasmDirectory, "wasm_exec.js")).href);
const go = new globalThis.Go();
const bytes = await readFile(join(wasmDirectory, "crdt-rga.wasm"));
const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
void Promise.resolve(go.run(instance));
const api = await waitForRuntime();
const runtime = new RGAWasmRuntime(api, protocolFromRaw(unwrap(api.protocol())));

console.log(`runtime=node-${process.version} workload=wasm-rga-browser-memory-persistence controlled=true mutations=${MUTATIONS}`);
for (let sample = 0; sample < SAMPLES; sample += 1) {
  const persistence = new MemoryRGAWasmBrowserPersistence();
  const documentID = `bench-${sample}`;
  const replicaID = `bench-actor-${sample}`;
  const started = performance.now();
  const document = await openRGAWasmBrowserDocument({
    documentID,
    replicaID,
    runtime,
    persistence,
    persistenceLimits: {
      compactAfterUpdates: MUTATIONS + 1,
      compactAfterBytes: 32 << 20,
      maxUpdates: MUTATIONS + 1,
      maxBytes: 32 << 20,
    },
  });
  for (let index = 0; index < MUTATIONS; index += 1) {
    document.insert(index, String.fromCharCode(65 + (index % 26)));
    await document.flush();
  }
  const persistedMs = performance.now() - started;
  await document.close();

  const recoverStarted = performance.now();
  const restored = await openRGAWasmBrowserDocument({ documentID, replicaID, runtime, persistence });
  if (Array.from(restored.text()).length !== MUTATIONS) {
    throw new Error("Wasm RGA browser persistence benchmark did not recover every mutation");
  }
  await restored.close();
  const recoveredMs = performance.now() - recoverStarted;
  const stored = await persistence.load(JSON.stringify([documentID, replicaID]));
  if (stored?.updates.length !== MUTATIONS) {
    throw new Error("Wasm RGA browser persistence benchmark did not retain the expected append log");
  }
  console.log(
    `sample=${sample + 1} retained_bytes=${stored.logBytes} append_ms=${persistedMs.toFixed(3)} append_ms_per_mutation=${(persistedMs / MUTATIONS).toFixed(4)} restore_ms=${recoveredMs.toFixed(3)}`,
  );
}

async function waitForRuntime() {
  const deadline = Date.now() + 5_000;
  while (globalThis.__darkinnoCRDTRGA === undefined) {
    if (Date.now() >= deadline) {
      throw new Error("RGA Wasm runtime did not start");
    }
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
  return globalThis.__darkinnoCRDTRGA;
}

function protocolFromRaw(value) {
  return {
    stateTypeID: requiredBigInt(value.stateTypeID, "stateTypeID"),
    deltaTypeID: requiredBigInt(value.deltaTypeID, "deltaTypeID"),
    semanticsVersion: requiredBigInt(value.semanticsVersion, "semanticsVersion"),
    maxFrameBytes: requiredPositiveInteger(value.maxFrameBytes, "maxFrameBytes"),
    maxTags: requiredPositiveInteger(value.maxTags, "maxTags"),
    maxStringBytes: requiredPositiveInteger(value.maxStringBytes, "maxStringBytes"),
    maxLocalEditBytes: requiredPositiveInteger(value.maxLocalEditBytes, "maxLocalEditBytes"),
    maxLocalEditRunes: requiredPositiveInteger(value.maxLocalEditRunes, "maxLocalEditRunes"),
  };
}

function requiredBigInt(value, name) {
  if (typeof value !== "string" || !/^(0|[1-9][0-9]*)$/.test(value)) {
    throw new Error(`Wasm protocol has invalid ${name}`);
  }
  return BigInt(value);
}

function requiredPositiveInteger(value, name) {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value <= 0) {
    throw new Error(`Wasm protocol has invalid ${name}`);
  }
  return value;
}

function unwrap(result) {
  if (result.ok !== true) {
    throw new Error(`Wasm operation failed: ${result.error}`);
  }
  return result.value;
}
