import { createRequire } from "node:module";
import process from "node:process";

import * as Y from "yjs";

const require = createRequire(import.meta.url);
const yjsVersion = require("yjs/package.json").version;
const options = parseArguments(process.argv.slice(2));
const reports = options.sizes.map((runes) => measure(runes, options.samples, options.warmups, options.iterations, options.revision));
process.stdout.write(`${JSON.stringify(reports, null, 2)}\n`);

function measure(runes, samples, warmups, iterations, revision) {
	const payload = "x".repeat(runes);
	for (let index = 0; index < warmups; index += 1) {
		runBatch(payload, iterations);
  }
  const durations = [];
  let updateBytes = 0;
  let stateBytes = 0;
  for (let index = 0; index < samples; index += 1) {
    if (global.gc !== undefined) {
      global.gc();
    }
		const result = runBatch(payload, iterations);
    durations.push(result.elapsedMS);
    updateBytes = result.updateBytes;
    stateBytes = result.stateBytes;
  }
  const sorted = [...durations].sort((left, right) => left - right);
  return {
    implementation: `Yjs ${yjsVersion}`,
    runtime: process.version,
    scenario: "two-replica initial plain-text sync; create, encode update, decode, apply, and verify",
    runes,
    samples_ms: durations,
    median_ms: sorted[Math.floor(sorted.length / 2)],
    update_bytes: updateBytes,
    state_bytes: stateBytes,
    revision,
  };
}

function runBatch(payload, iterations) {
	const started = process.hrtime.bigint();
	let updateBytes = 0;
	let stateBytes = 0;
	for (let index = 0; index < iterations; index += 1) {
		const result = run(payload);
		updateBytes = result.updateBytes;
		stateBytes = result.stateBytes;
	}
	return { elapsedMS: Number(process.hrtime.bigint() - started) / 1e6 / iterations, updateBytes, stateBytes };
}

function run(payload) {
  const source = new Y.Doc();
  const target = new Y.Doc();
	source.getText("text").insert(0, payload);
	const update = Y.encodeStateAsUpdate(source);
	Y.applyUpdate(target, update);
  if (target.getText("text").toString() !== payload) {
    throw new Error("target did not converge");
  }
	return { updateBytes: update.byteLength, stateBytes: Y.encodeStateAsUpdate(source).byteLength };
}

function parseArguments(argumentsList) {
	const values = { sizes: [4096, 16384], samples: 5, warmups: 2, iterations: 20, revision: "unknown" };
  for (let index = 0; index < argumentsList.length; index += 1) {
    const argument = argumentsList[index];
    const value = argumentsList[index + 1];
    if (argument === "--sizes") {
      values.sizes = value.split(",").map((item) => positiveInteger(item, "size"));
    } else if (argument === "--samples") {
      values.samples = positiveInteger(value, "samples");
	} else if (argument === "--warmups") {
		values.warmups = nonNegativeInteger(value, "warmups");
	} else if (argument === "--iterations") {
		values.iterations = positiveInteger(value, "iterations");
    } else if (argument === "--revision") {
      values.revision = value;
    } else {
      throw new Error(`unknown argument: ${argument}`);
    }
    index += 1;
  }
  return values;
}

function positiveInteger(value, label) {
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed <= 0) {
    throw new Error(`${label} must be a positive integer`);
  }
  return parsed;
}

function nonNegativeInteger(value, label) {
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed < 0) {
    throw new Error(`${label} must be a non-negative integer`);
  }
  return parsed;
}
