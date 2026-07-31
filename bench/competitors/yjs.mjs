import { createRequire } from "node:module";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import * as Y from "yjs";

const require = createRequire(import.meta.url);
const yjsVersion = require("yjs/package.json").version;

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const options = parseArguments(process.argv.slice(2));
  writeReports(makeReports(options), options.output);
}

export function main(argumentsList) {
  const options = parseArguments(argumentsList);
  return makeReports(options);
}

function makeReports(options) {
  return options.sizes.map((runes) => measure(options.scenario, runes, options.samples, options.warmups, options.iterations, options.revision));
}

function writeReports(reports, output) {
  const encoded = `${JSON.stringify(reports, null, 2)}\n`;
  if (output === "-") {
    process.stdout.write(encoded);
    return;
  }
  mkdirSync(dirname(output), { recursive: true, mode: 0o755 });
  writeFileSync(output, encoded, { encoding: "utf8", mode: 0o644 });
}

export function measure(scenario, runes, samples, warmups, iterations, revision) {
	const payload = "x".repeat(runes);
	for (let index = 0; index < warmups; index += 1) {
		runBatch(scenario, payload, iterations);
  }
  const durations = [];
  let updateBytes = 0;
  let stateBytes = 0;
  for (let index = 0; index < samples; index += 1) {
    if (global.gc !== undefined) {
      global.gc();
    }
		const result = runBatch(scenario, payload, iterations);
    durations.push(result.elapsedMS);
    updateBytes = result.updateBytes;
    stateBytes = result.stateBytes;
  }
  const sorted = [...durations].sort((left, right) => left - right);
  return {
    implementation: `Yjs ${yjsVersion}`,
    runtime: process.version,
    scenario: scenarioDescription(scenario),
    runes,
    samples_ms: durations,
    median_ms: sorted[Math.floor(sorted.length / 2)],
    update_bytes: updateBytes,
    state_bytes: stateBytes,
    revision,
  };
}

function runBatch(scenario, payload, iterations) {
	const started = process.hrtime.bigint();
	let updateBytes = 0;
	let stateBytes = 0;
	for (let index = 0; index < iterations; index += 1) {
		const result = runOnce(scenario, payload);
		updateBytes = result.updateBytes;
		stateBytes = result.stateBytes;
	}
	return { elapsedMS: Number(process.hrtime.bigint() - started) / 1e6 / iterations, updateBytes, stateBytes };
}

function runOnce(scenario, payload) {
  if (scenario === "initial") {
    return runInitial(payload);
  }
  if (scenario === "offline-concurrent") {
    return runOfflineConcurrent(payload);
  }
  throw new Error(`unsupported scenario: ${scenario}`);
}

function runInitial(payload) {
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

function runOfflineConcurrent(payload) {
  const seed = new Y.Doc();
  seed.getText("text").insert(0, payload);
  const base = Y.encodeStateAsUpdate(seed);

  const left = new Y.Doc();
  const right = new Y.Doc();
  const observer = new Y.Doc();
  Y.applyUpdate(left, base);
  Y.applyUpdate(right, base);
  const baseVector = Y.encodeStateVector(left);
  const offset = Math.floor(payload.length / 2); // The comparison payload is ASCII, so UTF-16 and rune offsets match.

  left.transact(() => {
    const text = left.getText("text");
    text.delete(offset, 1);
    text.insert(offset, "A");
  });
  right.transact(() => {
    const text = right.getText("text");
    text.delete(offset, 1);
    text.insert(offset, "B");
  });
  const leftUpdate = Y.encodeStateAsUpdate(left, baseVector);
  const rightUpdate = Y.encodeStateAsUpdate(right, baseVector);

  for (const update of [leftUpdate, leftUpdate]) {
    Y.applyUpdate(right, update);
  }
  for (const update of [rightUpdate, rightUpdate]) {
    Y.applyUpdate(left, update);
  }
  for (const update of [rightUpdate, rightUpdate, leftUpdate, leftUpdate, base, base]) {
    Y.applyUpdate(observer, update);
  }

  const expected = left.getText("text").toString();
  for (const doc of [right, observer]) {
    if (doc.getText("text").toString() !== expected) {
      throw new Error("replicas did not converge");
    }
  }
  return {
    updateBytes: base.byteLength + leftUpdate.byteLength + rightUpdate.byteLength,
    stateBytes: Y.encodeStateAsUpdate(left).byteLength,
  };
}

function scenarioDescription(scenario) {
  if (scenario === "initial") {
    return "two-replica initial plain-text sync; create, encode update, decode, apply, and verify";
  }
  if (scenario === "offline-concurrent") {
    return "three replicas; two offline writers concurrently replace one shared rune, then decode duplicate and reordered updates before verifying convergence";
  }
  throw new Error(`unsupported scenario: ${scenario}`);
}

export function parseArguments(argumentsList) {
	const values = { scenario: "initial", sizes: [4096, 16384], samples: 5, warmups: 2, iterations: 20, revision: "unknown", output: "-" };
  for (let index = 0; index < argumentsList.length; index += 1) {
    const rawArgument = argumentsList[index];
    const separator = rawArgument.indexOf("=");
    const argument = separator === -1 ? rawArgument : rawArgument.slice(0, separator);
    const hasInlineValue = separator !== -1;
    const value = hasInlineValue ? rawArgument.slice(separator + 1) : argumentsList[index + 1];
    if (argument === "--scenario") {
      values.scenario = parseScenario(value);
    } else if (argument === "--sizes") {
      values.sizes = value.split(",").map((item) => positiveInteger(item, "size"));
    } else if (argument === "--samples") {
      values.samples = positiveInteger(value, "samples");
	} else if (argument === "--warmups") {
		values.warmups = nonNegativeInteger(value, "warmups");
	} else if (argument === "--iterations") {
		values.iterations = positiveInteger(value, "iterations");
    } else if (argument === "--revision") {
      values.revision = value;
	} else if (argument === "--report") {
		values.output = value;
    } else {
      throw new Error(`unknown argument: ${argument}`);
    }
    if (!hasInlineValue) {
      index += 1;
    }
  }
  return values;
}

function parseScenario(value) {
  if (value === "initial" || value === "offline-concurrent") {
    return value;
  }
  throw new Error(`unsupported scenario: ${value}`);
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
