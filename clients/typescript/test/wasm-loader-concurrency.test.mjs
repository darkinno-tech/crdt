import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createServer } from "node:http";
import { join } from "node:path";
import test, { after } from "node:test";
import { pathToFileURL } from "node:url";

import {
  CRDTRuntimeError,
  RGA_PROTOCOL_PACKED_V3,
  RGA_PROTOCOL_PACKED_V3_V2,
  RGA_PROTOCOL_RUN_V2,
  RGA_PROTOCOL_V1,
  initRGAWasm,
} from "../dist/wasm.js";

const wasmDirectory = process.env.CRDT_WASM_DIR;
if (typeof wasmDirectory !== "string" || wasmDirectory === "") {
  throw new Error("CRDT_WASM_DIR must point to artifacts built by make wasm");
}
const artifactProtocol = protocolForArtifact(process.env.CRDT_RGA_PROTOCOL);
const incompatibleProtocol = artifactProtocol === RGA_PROTOCOL_RUN_V2 ? RGA_PROTOCOL_V1 : RGA_PROTOCOL_RUN_V2;

await import(pathToFileURL(join(wasmDirectory, "wasm_exec.js")).href);
const assets = await startAssetServer(wasmDirectory);
after(async () => {
  await new Promise((resolve, reject) => {
    assets.server.close((error) => (error === undefined ? resolve() : reject(error)));
  });
});

test("concurrent Wasm startup shares one real module and rejects a conflicting protocol", async () => {
  const runtimes = await Promise.all(Array.from(
    { length: 24 },
    () => initRGAWasm({ wasmURL: `${assets.url}/crdt-rga.wasm`, expectedProtocol: artifactProtocol }),
  ));
  assert.equal(assets.requests(), 1);
  for (const runtime of runtimes) {
    assert.deepEqual(runtime.protocol, runtimes[0].protocol);
  }

  await assert.rejects(
    () => initRGAWasm({ wasmURL: `${assets.url}/crdt-rga.wasm`, expectedProtocol: incompatibleProtocol }),
    (error) => error instanceof CRDTRuntimeError && error.code === "protocol_mismatch",
  );
  assert.equal(assets.requests(), 1);
});

function protocolForArtifact(value) {
  if (value === undefined || value === "run-v2") return RGA_PROTOCOL_RUN_V2;
  if (value === "v1") return RGA_PROTOCOL_V1;
  if (value === "packed-v3") return RGA_PROTOCOL_PACKED_V3;
  if (value === "packed-v3-v2") return RGA_PROTOCOL_PACKED_V3_V2;
  throw new Error("CRDT_RGA_PROTOCOL must be run-v2, packed-v3, packed-v3-v2, or v1");
}

async function startAssetServer(directory) {
  const wasm = await readFile(join(directory, "crdt-rga.wasm"));
  let requests = 0;
  const server = createServer((request, response) => {
    if (request.url !== "/crdt-rga.wasm") {
      response.writeHead(404).end();
      return;
    }
    requests += 1;
    response.writeHead(200, { "content-length": wasm.length, "content-type": "application/wasm" });
    response.end(wasm);
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  if (address === null || typeof address === "string") throw new Error("Wasm test server did not expose a TCP port");
  return { server, requests: () => requests, url: `http://127.0.0.1:${address.port}` };
}
