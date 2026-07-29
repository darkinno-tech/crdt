import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { performance } from "node:perf_hooks";

const wasmDirectory = process.env.CRDT_WASM_DIR;
if (typeof wasmDirectory !== "string" || wasmDirectory === "") {
  throw new Error("CRDT_WASM_DIR must point to artifacts built by make wasm");
}

await import(pathToFileURL(join(wasmDirectory, "wasm_exec.js")).href);
const go = new globalThis.Go();
const bytes = await readFile(join(wasmDirectory, "crdt-rga.wasm"));
const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
void Promise.resolve(go.run(instance));
const api = await waitForRuntime();
benchmark("single_rune", "协", 300);
benchmark("bulk_4096_runes", "协".repeat(4096), 30);
benchmark("snapshot_recovery_4096_runes", "协".repeat(4096), 20, runSnapshotRecoveryOnce);

function benchmark(name, payload, iterations, operation = runOnce) {
  for (let index = 0; index < 5; index += 1) {
    operation(payload);
  }
  for (let sample = 0; sample < 5; sample += 1) {
    const beforeHeap = process.memoryUsage().heapUsed;
    const started = performance.now();
    let frameBytes = 0;
    for (let index = 0; index < iterations; index += 1) {
      frameBytes = operation(payload);
    }
    const elapsedMs = performance.now() - started;
    const afterHeap = process.memoryUsage().heapUsed;
    console.log(
      `workload=${name} sample=${sample + 1} frame_bytes=${frameBytes} elapsed_ms=${elapsedMs.toFixed(2)} ms_per_insert_apply=${(elapsedMs / iterations).toFixed(3)} heap_delta_b=${afterHeap - beforeHeap}`,
    );
  }
}

function runSnapshotRecoveryOnce(payload) {
  const source = handle(unwrap(api.create("bench-snapshot-source")));
  unwrap(api.insert(source, 0, payload));
  const saved = unwrap(api.snapshot(source));
  const recovered = handle(unwrap(api.restore(saved)));
  if (unwrap(api.text(recovered)) !== payload) {
    throw new Error("Wasm snapshot recovery did not converge benchmark document");
  }
  unwrap(api.drop(source));
  unwrap(api.drop(recovered));
  if (!(saved.state instanceof Uint8Array)) {
    throw new Error("Wasm snapshot did not return a Uint8Array state");
  }
  return saved.state.byteLength;
}

function runOnce(payload) {
  const source = handle(unwrap(api.create("bench-source")));
  const target = handle(unwrap(api.create("bench-target")));
  const delta = unwrap(api.insert(source, 0, payload));
  unwrap(api.applyDelta(target, delta));
  if (unwrap(api.text(target)) !== payload) {
    throw new Error("Wasm runtime did not converge benchmark document");
  }
  unwrap(api.drop(source));
  unwrap(api.drop(target));
  return delta.byteLength;
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

function unwrap(result) {
  if (result.ok !== true) {
    throw new Error(`Wasm operation failed: ${result.error}`);
  }
  return result.value;
}

function handle(value) {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value <= 0) {
    throw new Error("Wasm returned an invalid document handle");
  }
  return value;
}
