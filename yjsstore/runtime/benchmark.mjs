import { performance } from "node:perf_hooks";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createRequire } from "node:module";

import * as Y from "yjs";

import { createYJSStoreServer, loadConfig } from "./server.mjs";

const require = createRequire(import.meta.url);
const yjsVersion = require("yjs/package.json").version;

export async function main(argumentsList = process.argv.slice(2)) {
  const options = parseArguments(argumentsList);
  const dataDir = await mkdtemp(join(tmpdir(), "darkinno-yjs-store-benchmark-"));
  const token = "yjs-store-benchmark-token-0123456789";
  const config = await loadConfig({
    YJS_STORE_DATA_DIR: dataDir,
    YJS_STORE_TOKEN: token,
    YJS_STORE_PORT: "0",
    YJS_STORE_MAX_UPDATE_BYTES: `${options.maxUpdateBytes}`,
    YJS_STORE_MAX_STATE_VECTOR_BYTES: "65536",
    YJS_STORE_MAX_SNAPSHOT_BYTES: `${options.maxSnapshotBytes}`,
    YJS_STORE_MAX_MERGE_UPDATES: "256",
  });
  const service = createYJSStoreServer(config);
  const endpoint = await service.listen();
  try {
    const document = { tenant: "benchmark", room: "notes", epoch: "1", schema: "plain-text-v1", format: "v1" };
    const author = new Y.Doc();
    const initial = captureUpdate(author, () => author.getText("shared").insert(0, "x".repeat(options.initialBytes)));
    await call(endpoint, token, "/v1/yjs/apply", { document, update: toBase64(initial) });
    const replicas = Array.from({ length: options.receivers }, () => {
      const replica = new Y.Doc();
      Y.applyUpdate(replica, initial);
      return replica;
    });
    const measurements = { apply: [], diff: [], snapshot: [] };
    let deltaBytes = 0;
    for (let index = 0; index < options.warmups + options.iterations; index++) {
      const update = captureUpdate(author, () => author.getText("shared").insert(author.getText("shared").length, `-${index}-`));
      const applyStarted = performance.now();
      await call(endpoint, token, "/v1/yjs/apply", { document, update: toBase64(update) });
      const applyDuration = performance.now() - applyStarted;
      for (const replica of replicas) {
        const vector = Y.encodeStateVector(replica);
        const diffStarted = performance.now();
        const result = await call(endpoint, token, "/v1/yjs/diff", { document, stateVector: toBase64(vector) });
        const diffDuration = performance.now() - diffStarted;
        const delta = fromBase64(result.update);
        Y.applyUpdate(replica, delta);
        if (index >= options.warmups) {
          measurements.diff.push(diffDuration);
          deltaBytes += delta.length;
        }
      }
      const snapshotStarted = performance.now();
      await call(endpoint, token, "/v1/yjs/snapshot", { document });
      const snapshotDuration = performance.now() - snapshotStarted;
      if (index >= options.warmups) {
        measurements.apply.push(applyDuration);
        measurements.snapshot.push(snapshotDuration);
      }
    }
    const expected = author.getText("shared").toString();
    if (!replicas.every((replica) => replica.getText("shared").toString() === expected)) {
      throw new Error("benchmark replicas did not converge");
    }
    return {
      implementation: `YJSStore sidecar with yjs@${yjsVersion}`,
      node: process.version,
      scenario: "loopback durable apply, state-vector diff, and snapshot",
      limits: { maxUpdateBytes: options.maxUpdateBytes, maxSnapshotBytes: options.maxSnapshotBytes },
      workload: { initialBytes: options.initialBytes, iterations: options.iterations, warmups: options.warmups, receivers: options.receivers },
      latencyMs: {
        apply: summarize(measurements.apply),
        diff: summarize(measurements.diff),
        snapshot: summarize(measurements.snapshot),
      },
      meanDiffBytes: deltaBytes / Math.max(1, measurements.diff.length),
    };
  } finally {
    await service.close();
    await rm(dataDir, { recursive: true, force: true });
  }
}

export function parseArguments(argumentsList) {
  const options = { initialBytes: 4096, iterations: 40, warmups: 5, receivers: 4, maxUpdateBytes: 1 << 20, maxSnapshotBytes: 16 << 20 };
  for (let index = 0; index < argumentsList.length; index += 2) {
    const name = argumentsList[index];
    const value = argumentsList[index + 1];
    if (value === undefined || !["--initial-bytes", "--iterations", "--warmups", "--receivers", "--max-update-bytes", "--max-snapshot-bytes"].includes(name) || !/^[1-9][0-9]*$/.test(value)) {
      throw new Error(`invalid benchmark argument ${name ?? ""}`);
    }
    const parsed = Number(value);
    if (!Number.isSafeInteger(parsed)) {
      throw new Error(`invalid benchmark value for ${name}`);
    }
    switch (name) {
      case "--initial-bytes": options.initialBytes = parsed; break;
      case "--iterations": options.iterations = parsed; break;
      case "--warmups": options.warmups = parsed; break;
      case "--receivers": options.receivers = parsed; break;
      case "--max-update-bytes": options.maxUpdateBytes = parsed; break;
      case "--max-snapshot-bytes": options.maxSnapshotBytes = parsed; break;
    }
  }
  if (options.initialBytes > options.maxSnapshotBytes || options.maxSnapshotBytes < options.maxUpdateBytes || options.receivers > 64 || options.iterations > 10000 || options.warmups > 1000) {
    throw new Error("benchmark options exceed the bounded workload contract");
  }
  return options;
}

function summarize(values) {
  const ordered = [...values].sort((left, right) => left - right);
  if (ordered.length === 0) {
    throw new Error("benchmark produced no samples");
  }
  return {
    p50: percentile(ordered, 0.50),
    p95: percentile(ordered, 0.95),
    p99: percentile(ordered, 0.99),
  };
}

function percentile(ordered, quantile) {
  return Number(ordered[Math.min(ordered.length - 1, Math.ceil(ordered.length * quantile) - 1)].toFixed(3));
}

async function call(endpoint, token, path, body) {
  const response = await fetch(`${endpoint}${path}`, {
    method: "POST",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  const parsed = await response.json();
  if (!response.ok) {
    throw new Error(`YJSStore ${path} failed with ${parsed.code}`);
  }
  return parsed;
}

function captureUpdate(document, operation) {
  let update;
  document.once("update", (candidate) => { update = candidate; });
  operation();
  if (!(update instanceof Uint8Array) || update.length === 0) {
    throw new Error("Yjs did not emit an update");
  }
  return update;
}

function toBase64(value) {
  return Buffer.from(value).toString("base64");
}

function fromBase64(value) {
  return new Uint8Array(Buffer.from(value, "base64"));
}

if (process.argv[1] === new URL(import.meta.url).pathname) {
  try {
    process.stdout.write(`${JSON.stringify(await main())}\n`);
  } catch (error) {
    process.stderr.write(`YJSStore benchmark failed: ${error instanceof Error ? error.message : "unknown error"}\n`);
    process.exitCode = 1;
  }
}
