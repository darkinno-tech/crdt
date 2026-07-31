import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { performance } from "node:perf_hooks";

import { bindCodeMirrorPlainText } from "../dist/bindings.js";

const wasmDirectory = process.env.CRDT_WASM_DIR;
if (typeof wasmDirectory !== "string" || wasmDirectory === "") {
  throw new Error("CRDT_WASM_DIR must point to artifacts built by make wasm");
}

const SAMPLES = 5;
const EDITS = 256;
// Keep the initial import below the negotiated 16,384-rune local-edit cap.
const INITIAL_TEXT = "a".repeat(12 * 1024);

await import(pathToFileURL(join(wasmDirectory, "wasm_exec.js")).href);
const go = new globalThis.Go();
const bytes = await readFile(join(wasmDirectory, "crdt-rga.wasm"));
const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
void Promise.resolve(go.run(instance));
const api = await waitForRuntime();
const protocol = protocolFromRaw(unwrap(api.protocol()));

function runSample() {
  const sourceHandle = handle(unwrap(api.create("bindings-bench-source")));
  const targetHandle = handle(unwrap(api.create("bindings-bench-target")));
  const initialFrame = unwrap(api.insert(sourceHandle, 0, INITIAL_TEXT));
  unwrap(api.applyDelta(targetHandle, initialFrame));
  const source = new WasmRGA(sourceHandle);
  const target = new WasmRGA(targetHandle);
  const view = new CodeMirrorPort(source.text());
  let frameBytes = 0;
  const binding = bindCodeMirrorPlainText(source, view, {
    onLocalFrame(frame) {
      frameBytes += frame.byteLength;
      target.applyDelta(frame);
    },
  });

  const beforeHeap = process.memoryUsage().heapUsed;
  const started = performance.now();
  for (let index = 0; index < EDITS; index += 1) {
    const offset = (index * 131) % view.state.doc.length;
    view.userReplace(offset, 1, String.fromCharCode(65 + (index % 26)));
    binding.applyViewUpdate({ docChanged: true });
  }
  const elapsedMs = performance.now() - started;
  const heapDelta = process.memoryUsage().heapUsed - beforeHeap;
  if (source.text() !== target.text() || source.text() !== view.state.doc.toString()) {
    throw new Error("Go Wasm binding benchmark did not converge");
  }
  binding.destroy();
  unwrap(api.drop(sourceHandle));
  unwrap(api.drop(targetHandle));
  return { elapsedMs, frameBytes, heapDelta };
}

class WasmRGA {
  constructor(handle) {
    this.handle = handle;
    this.protocol = protocol;
  }

  text() {
    return unwrap(api.text(this.handle));
  }

  replace(offset, count, value) {
    return unwrap(api.replace(this.handle, offset, count, value));
  }

  applyDelta(frame) {
    unwrap(api.applyDelta(this.handle, frame));
  }
}

class CodeMirrorPort {
  constructor(value) {
    this.value = value;
  }

  get state() {
    return {
      doc: {
        length: this.value.length,
        toString: () => this.value,
      },
    };
  }

  dispatch({ changes }) {
    this.value = `${this.value.slice(0, changes.from)}${changes.insert}${this.value.slice(changes.to)}`;
  }

  userReplace(from, count, value) {
    this.value = `${this.value.slice(0, from)}${value}${this.value.slice(from + count)}`;
  }
}

function protocolFromRaw(value) {
  return {
    maxLocalEditBytes: requiredPositiveInteger(value.maxLocalEditBytes, "maxLocalEditBytes"),
    maxLocalEditRunes: requiredPositiveInteger(value.maxLocalEditRunes, "maxLocalEditRunes"),
  };
}

function requiredPositiveInteger(value, name) {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value <= 0) {
    throw new Error(`Wasm protocol has invalid ${name}`);
  }
  return value;
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

console.log(`runtime=node-${process.version} workload=codemirror-port-go-wasm-rga controlled=true initial_runes=${Array.from(INITIAL_TEXT).length} local_edits=${EDITS}`);
for (let sample = 0; sample < SAMPLES; sample += 1) {
  const result = runSample();
  console.log(
    `sample=${sample + 1} elapsed_ms=${result.elapsedMs.toFixed(2)} ms_per_local_merge=${(result.elapsedMs / EDITS).toFixed(3)} frame_bytes=${result.frameBytes} heap_delta_b=${result.heapDelta}`,
  );
}
