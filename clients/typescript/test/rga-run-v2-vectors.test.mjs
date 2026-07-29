import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { decodeFrame, FrameType } from "../dist/frame.js";

const vectors = JSON.parse(
  await readFile(new URL("../../../docs/protocol/testdata/rga-run-v2-vectors.json", import.meta.url), "utf8"),
);

test("published RGA run-v2 vectors retain their canonical outer frames", () => {
  assert.equal(vectors.protocol, "rga-run-v2");
  assert.equal(vectors.semantics_version, 2);
  assert.equal(vectors.frame_types.state, Number(FrameType.RGARunState));
  assert.equal(vectors.frame_types.delta, Number(FrameType.RGARunDelta));
  for (const vector of vectors.vectors) {
    const frame = decodeFrame(hexBytes(vector.hex));
    assert.equal(frame.typeID, BigInt(vector.frame_type), vector.name);
    assert.equal(frame.codecID.length, 0, vector.name);
    assert.ok(frame.payload.length > 0, vector.name);
  }
});

function hexBytes(value) {
  assert.match(value, /^(?:[0-9a-f]{2})+$/i);
  const bytes = new Uint8Array(value.length / 2);
  for (let index = 0; index < bytes.length; index += 1) {
    bytes[index] = Number.parseInt(value.slice(index * 2, index * 2 + 2), 16);
  }
  return bytes;
}
