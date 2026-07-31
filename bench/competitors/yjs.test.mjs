import assert from "node:assert/strict";
import { mkdtempSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { execFileSync } from "node:child_process";
import test from "node:test";

import { main, parseArguments } from "./yjs.mjs";

test("offline-concurrent scenario converges and reports emitted bytes", () => {
  const [report] = main(["--scenario", "offline-concurrent", "--sizes", "1", "--samples", "1", "--warmups", "0", "--iterations", "1", "--revision", "test"]);
  assert.equal(report.runes, 1);
  assert.equal(report.revision, "test");
  assert.match(report.scenario, /three replicas/);
  assert.ok(report.update_bytes > 0);
  assert.ok(report.state_bytes > 0);
});

test("argument parser rejects an unknown comparison scenario", () => {
  assert.equal(parseArguments(["--scenario", "initial"]).scenario, "initial");
  assert.throws(() => parseArguments(["--scenario", "unknown"]), /unsupported scenario/);
});

test("CLI writes JSON to a requested nested output path", () => {
  const directory = mkdtempSync(join(tmpdir(), "darkinno-yjs-"));
  const output = join(directory, "reports", "comparison.json");
  execFileSync(process.execPath, ["--no-experimental-webstorage", "--expose-gc", "yjs.mjs", "--sizes", "1", "--samples", "1", "--warmups", "0", "--iterations", "1", "--report", output], { cwd: new URL(".", import.meta.url) });
  const [report] = JSON.parse(readFileSync(output, "utf8"));
  assert.equal(report.runes, 1);
});
