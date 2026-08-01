import { performance } from "node:perf_hooks";

import { bindCodeMirrorPlainText } from "../dist/bindings.js";

const SAMPLES = 5;
const LOCAL_EDITS = 512;
const REMOTE_EDITS = 256;
const INITIAL_TEXT = "a".repeat(readPositiveInteger(process.env.CRDT_BINDINGS_INITIAL_RUNES, 32 * 1024));

function runSample(mode) {
  const document = new BenchmarkRGA(INITIAL_TEXT);
  const view = new CodeMirrorPort(INITIAL_TEXT);
  let emittedFrames = 0;
  const binding = bindCodeMirrorPlainText(document, view, {
    onLocalFrame() {
      emittedFrames += 1;
    },
  });
  const beforeHeap = process.memoryUsage().heapUsed;
  const localStarted = performance.now();
  for (let index = 0; index < LOCAL_EDITS; index += 1) {
    const offset = (index * 61) % view.state.doc.length;
    const update = view.userReplace(offset, 1, String.fromCharCode(65 + (index % 26)));
    binding.applyViewUpdate(mode === "native_incremental" ? update : { docChanged: true });
  }
  const localMs = performance.now() - localStarted;
  if (document.text() !== view.state.doc.toString() || emittedFrames !== LOCAL_EDITS) {
    throw new Error("local binding benchmark diverged");
  }

  const remoteStarted = performance.now();
  for (let index = 0; index < REMOTE_EDITS; index += 1) {
    const offset = (index * 89) % document.text().length;
    const frame = encodeReplacement(offset, 1, String.fromCharCode(97 + (index % 26)));
    binding.applyRemote(frame);
  }
  const remoteMs = performance.now() - remoteStarted;
  if (document.text() !== view.state.doc.toString() || emittedFrames !== LOCAL_EDITS) {
    throw new Error("remote binding benchmark echoed or diverged");
  }
  const heapDelta = process.memoryUsage().heapUsed - beforeHeap;
  binding.destroy();
  return { localMs, remoteMs, emittedFrames, heapDelta };
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
    return {
      docChanged: true,
      changes: {
        iterChanges(listener) {
          listener(from, from + count, from, from + value.length, { toString: () => value });
        },
      },
    };
  }
}

class BenchmarkRGA {
  constructor(value) {
    this.value = value;
    this.protocol = {
      maxLocalEditBytes: 64 * 1024,
      maxLocalEditRunes: 16 * 1024,
    };
  }

  text() {
    return this.value;
  }

  replace(offset, count, value) {
    this.value = replaceRunes(this.value, offset, count, value);
    return encodeReplacement(offset, count, value);
  }

  applyDelta(frame) {
    const { offset, count, value } = JSON.parse(new TextDecoder().decode(frame));
    this.value = replaceRunes(this.value, offset, count, value);
  }
}

function replaceRunes(value, offset, count, replacement) {
  const runes = Array.from(value);
  runes.splice(offset, count, ...Array.from(replacement));
  return runes.join("");
}

function encodeReplacement(offset, count, value) {
  return new TextEncoder().encode(JSON.stringify({ offset, count, value }));
}

function readPositiveInteger(value, fallback) {
  if (value === undefined) return fallback;
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed <= 0) {
    throw new Error("CRDT_BINDINGS_INITIAL_RUNES must be a positive safe integer");
  }
  return parsed;
}

console.log(`runtime=node-${process.version} workload=codemirror-port-rga-simulation controlled=true initial_runes=${Array.from(INITIAL_TEXT).length} local_edits=${LOCAL_EDITS} remote_edits=${REMOTE_EDITS}`);
for (const mode of ["native_incremental", "full_projection_fallback"]) {
  for (let sample = 0; sample < SAMPLES; sample += 1) {
    const result = runSample(mode);
    console.log(
      `scenario=${mode} sample=${sample + 1} local_ms=${result.localMs.toFixed(2)} remote_ms=${result.remoteMs.toFixed(2)} local_ms_per_edit=${(result.localMs / LOCAL_EDITS).toFixed(3)} remote_ms_per_edit=${(result.remoteMs / REMOTE_EDITS).toFixed(3)} emitted_frames=${result.emittedFrames} heap_delta_b=${result.heapDelta}`,
    );
  }
}
