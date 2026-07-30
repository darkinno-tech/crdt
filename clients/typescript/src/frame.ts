/**
 * Canonical CRDT v1 frame decoding shared by browser and JavaScript mobile
 * runtimes. This module only authenticates frame shape and accidental
 * corruption (CRC-32C); callers still must authenticate the sender and route
 * the payload to a type-specific decoder such as the RGA Wasm runtime.
 */

export const FORMAT_VERSION = 1n;

export { FrameType } from "./type_ids.generated.js";

export interface FrameDecoderLimits {
  /** Maximum total bytes accepted before copying a payload. */
  readonly maxFrameBytes: number;
  /** Maximum payload bytes accepted after the envelope is parsed. */
  readonly maxPayloadBytes: number;
  /** Maximum opaque codec-ID bytes accepted in the envelope. */
  readonly maxCodecBytes: number;
}

/**
 * Conservative client-side defaults. They intentionally cap input below the
 * Go library default. Transport code should enforce an equal or smaller body
 * cap before allocating a Uint8Array.
 */
export const DEFAULT_FRAME_LIMITS: Readonly<FrameDecoderLimits> = Object.freeze({
  maxFrameBytes: 1 << 20,
  maxPayloadBytes: (1 << 20) - 4096,
  maxCodecBytes: 256,
});

export interface Frame {
  readonly version: bigint;
  readonly typeID: bigint;
  /** Opaque canonical bytes; do not assume this is UTF-8. */
  readonly codecID: Uint8Array;
  /** A defensive copy of the framed payload. */
  readonly payload: Uint8Array;
}

export type FrameDecodeErrorCode = "invalid_frame" | "frame_limit";

export class FrameDecodeError extends Error {
  readonly code: FrameDecodeErrorCode;

  constructor(code: FrameDecodeErrorCode) {
    super(code);
    this.name = "FrameDecodeError";
    this.code = code;
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

const CRC32C_POLYNOMIAL = 0x82f63b78;
const MAX_UVARINT_BYTES = 10;
const CRC32C_TABLE = makeCRC32CTable();

/**
 * decodeFrame validates one complete canonical Go CRDT v1 frame. It rejects
 * unknown envelope versions, non-shortest varints, length inconsistencies,
 * CRC failures, and configured resource-limit violations before returning a
 * copied payload.
 */
export function decodeFrame(
  input: Uint8Array,
  limits: Readonly<FrameDecoderLimits> = DEFAULT_FRAME_LIMITS,
): Frame {
  assertLimits(limits);
  if (input.length < 9 || input.length > limits.maxFrameBytes) {
    throw frameLimit();
  }
  if (input[0] !== 0x43 || input[1] !== 0x52 || input[2] !== 0x44 || input[3] !== 0x54) {
    throw invalidFrame();
  }

  const checksumOffset = input.length - 4;
  const storedChecksum = readUint32BE(input, checksumOffset);
  if (crc32c(input, 4, checksumOffset) !== storedChecksum) {
    throw invalidFrame();
  }

  let position = 4;
  const version = readUvarint(input, position, checksumOffset);
  if (version.value !== FORMAT_VERSION) {
    throw invalidFrame();
  }
  position = version.next;

  const typeID = readUvarint(input, position, checksumOffset);
  if (typeID.value === 0n) {
    throw invalidFrame();
  }
  position = typeID.next;

  const codecLength = readUvarint(input, position, checksumOffset);
  position = codecLength.next;
  if (
    codecLength.value > BigInt(limits.maxCodecBytes) ||
    codecLength.value > BigInt(checksumOffset - position)
  ) {
    throw frameLimit();
  }
  const codecEnd = position + Number(codecLength.value);
  const codecID = input.slice(position, codecEnd);
  position = codecEnd;

  const payloadLength = readUvarint(input, position, checksumOffset);
  position = payloadLength.next;
  if (
    payloadLength.value > BigInt(limits.maxPayloadBytes) ||
    payloadLength.value !== BigInt(checksumOffset - position)
  ) {
    throw frameLimit();
  }

  return {
    version: version.value,
    typeID: typeID.value,
    codecID,
    payload: input.slice(position, checksumOffset),
  };
}

/** Returns true when two opaque byte sequences match exactly. */
export function bytesEqual(left: Uint8Array, right: Uint8Array): boolean {
  if (left.length !== right.length) {
    return false;
  }
  for (let index = 0; index < left.length; index += 1) {
    if (left[index] !== right[index]) {
      return false;
    }
  }
  return true;
}

/**
 * assertFrameType validates the type and opaque codec contract after
 * decodeFrame. Type IDs alone are not an authenticated manifest handshake.
 */
export function assertFrameType(
  frame: Frame,
  typeID: bigint,
  codecID: Uint8Array = new Uint8Array(),
): void {
  if (frame.typeID !== typeID || !bytesEqual(frame.codecID, codecID)) {
    throw invalidFrame();
  }
}

interface VarintResult {
  readonly value: bigint;
  readonly next: number;
}

function readUvarint(data: Uint8Array, position: number, end: number): VarintResult {
  if (position < 0 || position >= end) {
    throw invalidFrame();
  }
  let value = 0n;
  for (let count = 0; count < MAX_UVARINT_BYTES; count += 1) {
    const index = position + count;
    if (index >= end) {
      throw invalidFrame();
    }
    const byte = data[index]!;
    if (count === MAX_UVARINT_BYTES - 1 && (byte & 0xfe) !== 0) {
      throw invalidFrame();
    }
    value |= BigInt(byte & 0x7f) << BigInt(count * 7);
    if ((byte & 0x80) === 0) {
      if (uvarintSize(value) !== count + 1) {
        throw invalidFrame();
      }
      return { value, next: index + 1 };
    }
  }
  throw invalidFrame();
}

function uvarintSize(value: bigint): number {
  let size = 1;
  let remaining = value;
  while (remaining >= 0x80n) {
    remaining >>= 7n;
    size += 1;
  }
  return size;
}

function readUint32BE(data: Uint8Array, position: number): number {
  return (
    ((data[position]! << 24) |
      (data[position + 1]! << 16) |
      (data[position + 2]! << 8) |
      data[position + 3]!) >>>
    0
  );
}

function crc32c(data: Uint8Array, start: number, end: number): number {
  let checksum = 0xffffffff;
  for (let index = start; index < end; index += 1) {
    checksum = CRC32C_TABLE[(checksum ^ data[index]!) & 0xff]! ^ (checksum >>> 8);
  }
  return (checksum ^ 0xffffffff) >>> 0;
}

function makeCRC32CTable(): Uint32Array {
  const table = new Uint32Array(256);
  for (let index = 0; index < table.length; index += 1) {
    let value = index;
    for (let bit = 0; bit < 8; bit += 1) {
      value = (value & 1) === 0 ? value >>> 1 : (value >>> 1) ^ CRC32C_POLYNOMIAL;
    }
    table[index] = value >>> 0;
  }
  return table;
}

function assertLimits(limits: Readonly<FrameDecoderLimits>): void {
  if (
    !isPositiveSafeInteger(limits.maxFrameBytes) ||
    !isPositiveSafeInteger(limits.maxPayloadBytes) ||
    !isPositiveSafeInteger(limits.maxCodecBytes) ||
    limits.maxPayloadBytes > limits.maxFrameBytes ||
    limits.maxCodecBytes > limits.maxFrameBytes
  ) {
    throw frameLimit();
  }
}

function isPositiveSafeInteger(value: number): boolean {
  return Number.isSafeInteger(value) && value > 0;
}

function invalidFrame(): FrameDecodeError {
  return new FrameDecodeError("invalid_frame");
}

function frameLimit(): FrameDecodeError {
  return new FrameDecodeError("frame_limit");
}
