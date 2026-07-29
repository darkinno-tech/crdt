import { performance } from "node:perf_hooks";

import { decodeFrame, FrameType } from "../dist/frame.js";

const payload = new Uint8Array(512 * 1024);
for (let index = 0; index < payload.length; index += 1) {
  payload[index] = index & 0xff;
}
const frame = makeFrame(FrameType.RGADelta, payload);
const samples = [];

for (let warmup = 0; warmup < 10; warmup += 1) {
  decodeFrame(frame);
}
for (let sample = 0; sample < 5; sample += 1) {
  const iterations = 100;
  const beforeHeap = process.memoryUsage().heapUsed;
  const started = performance.now();
  for (let index = 0; index < iterations; index += 1) {
    const decoded = decodeFrame(frame);
    if (decoded.payload.length !== payload.length) {
      throw new Error("unexpected decoded payload length");
    }
  }
  const elapsedMs = performance.now() - started;
  const afterHeap = process.memoryUsage().heapUsed;
  const mibPerSecond = (frame.length * iterations) / (1024 * 1024) / (elapsedMs / 1000);
  samples.push({ elapsedMs, mibPerSecond, heapDelta: afterHeap - beforeHeap });
}

for (const [index, sample] of samples.entries()) {
  console.log(
    `sample=${index + 1} elapsed_ms=${sample.elapsedMs.toFixed(2)} throughput_mib_s=${sample.mibPerSecond.toFixed(2)} heap_delta_b=${sample.heapDelta}`,
  );
}

function makeFrame(typeID, payloadBytes) {
  const header = Uint8Array.from([
    ...encodeUvarint(1n),
    ...encodeUvarint(typeID),
    0,
    ...encodeUvarint(BigInt(payloadBytes.length)),
  ]);
  const body = new Uint8Array(header.length + payloadBytes.length);
  body.set(header);
  body.set(payloadBytes, header.length);
  const checksum = crc32cSlow(body);
  const frame = new Uint8Array(4 + body.length + 4);
  frame.set([0x43, 0x52, 0x44, 0x54]);
  frame.set(body, 4);
  frame[frame.length - 4] = (checksum >>> 24) & 0xff;
  frame[frame.length - 3] = (checksum >>> 16) & 0xff;
  frame[frame.length - 2] = (checksum >>> 8) & 0xff;
  frame[frame.length - 1] = checksum & 0xff;
  return frame;
}

function encodeUvarint(value) {
  const result = [];
  let remaining = value;
  do {
    let byte = Number(remaining & 0x7fn);
    remaining >>= 7n;
    if (remaining !== 0n) {
      byte |= 0x80;
    }
    result.push(byte);
  } while (remaining !== 0n);
  return result;
}

function crc32cSlow(data) {
  let checksum = 0xffff_ffff;
  for (const byte of data) {
    checksum ^= byte;
    for (let bit = 0; bit < 8; bit += 1) {
      checksum = (checksum & 1) === 0 ? checksum >>> 1 : (checksum >>> 1) ^ 0x82f6_3b78;
    }
  }
  return (checksum ^ 0xffff_ffff) >>> 0;
}
