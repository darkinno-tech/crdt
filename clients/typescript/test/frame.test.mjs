import assert from "node:assert/strict";
import test from "node:test";

import {
  assertFrameType,
  bytesEqual,
  decodeFrame,
  FrameDecodeError,
  FrameType,
} from "../dist/frame.js";

test("decodeFrame accepts a canonical opaque-codec frame and copies input", () => {
  const codec = Uint8Array.of(0xff, 0x00, 0x61);
  const input = makeFrame({ typeID: FrameType.RGADelta, codec, payload: Uint8Array.of(1, 2, 3) });
  const decoded = decodeFrame(input);

  assert.equal(decoded.version, 1n);
  assert.equal(decoded.typeID, FrameType.RGADelta);
  assert.deepEqual(decoded.codecID, codec);
  assert.deepEqual(decoded.payload, Uint8Array.of(1, 2, 3));
  assert.doesNotThrow(() => assertFrameType(decoded, FrameType.RGADelta, codec));
  assert.throws(
    () => assertFrameType(decoded, FrameType.RGAState),
    (error) => error instanceof FrameDecodeError && error.code === "invalid_frame",
  );

  input[input.length - 5] = 99;
  assert.deepEqual(decoded.payload, Uint8Array.of(1, 2, 3));
  assert.equal(bytesEqual(decoded.codecID, codec), true);
});

test("FrameType includes every generated stable protocol pair", () => {
  assert.equal(FrameType.ListRGAState, 21n);
  assert.equal(FrameType.ListRGADelta, 22n);
  assert.equal(FrameType.RichTextState, 23n);
  assert.equal(FrameType.RichTextDelta, 24n);
});

test("decodeFrame rejects checksum, canonicality, type, and resource failures", () => {
  const valid = makeFrame({ typeID: 7n, codec: new Uint8Array(), payload: Uint8Array.of(7, 8, 9) });
  const tampered = valid.slice();
  tampered[5] ^= 1;
  assertFrameError(() => decodeFrame(tampered), "invalid_frame");

  const nonCanonical = seal(Uint8Array.of(0x81, 0x00, 0x07, 0x00, 0x00));
  assertFrameError(() => decodeFrame(nonCanonical), "invalid_frame");

  const zeroType = makeFrame({ typeID: 0n, codec: new Uint8Array(), payload: new Uint8Array() });
  assertFrameError(() => decodeFrame(zeroType), "invalid_frame");
  assertFrameError(
    () => decodeFrame(valid, { maxFrameBytes: 128, maxPayloadBytes: 2, maxCodecBytes: 8 }),
    "frame_limit",
  );
  assertFrameError(
    () => decodeFrame(valid, { maxFrameBytes: 0, maxPayloadBytes: 1, maxCodecBytes: 1 }),
    "frame_limit",
  );
  assertFrameError(
    () => decodeFrame(valid, { maxFrameBytes: 8, maxPayloadBytes: 9, maxCodecBytes: 1 }),
    "frame_limit",
  );
});

test("decodeFrame fuzzes arbitrary truncated and malformed input without non-domain failures", () => {
  let state = 0x12_34_56_78;
  for (let iteration = 0; iteration < 10_000; iteration += 1) {
    state = nextRandom(state);
    const length = state & 0x3ff;
    const input = new Uint8Array(length);
    for (let index = 0; index < input.length; index += 1) {
      state = nextRandom(state);
      input[index] = state & 0xff;
    }
    try {
      decodeFrame(input);
    } catch (error) {
      assert.ok(error instanceof FrameDecodeError, `unexpected error: ${String(error)}`);
    }
  }
});

function assertFrameError(operation, code) {
  assert.throws(
    operation,
    (error) => error instanceof FrameDecodeError && error.code === code,
  );
}

function makeFrame({ typeID, codec, payload }) {
  const body = Uint8Array.from([
    ...encodeUvarint(1n),
    ...encodeUvarint(typeID),
    ...encodeUvarint(BigInt(codec.length)),
    ...codec,
    ...encodeUvarint(BigInt(payload.length)),
    ...payload,
  ]);
  return seal(body);
}

function seal(body) {
  const checksum = crc32cSlow(body);
  return Uint8Array.from([
    0x43,
    0x52,
    0x44,
    0x54,
    ...body,
    (checksum >>> 24) & 0xff,
    (checksum >>> 16) & 0xff,
    (checksum >>> 8) & 0xff,
    checksum & 0xff,
  ]);
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

function nextRandom(value) {
  return (Math.imul(value, 1_664_525) + 1_013_904_223) >>> 0;
}
