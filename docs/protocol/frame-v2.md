# Compression-aware CRDT frame v2

This specification defines the optional **outer frame v2** representation. It
is deliberately independent of CRDT semantics and TypeIDs: it can carry a
G-Counter, an RGA delta, a run-v2 RGA state, or another implemented canonical
payload without changing the underlying merge contract.

It must not be confused with the RGA **run-v2** protocol (TypeIDs 19/20). Run
v2 compacts an RGA's internal same-replica chains; outer frame v2 compresses a
complete canonical frame payload. A group may use neither, either, or both,
but it must negotiate the exact combination.

## When to use it

Outer v2 is most useful for snapshots, large pastes, and batched updates with
repeated actor IDs, keys, or values. Small interactive deltas can be larger
than v1 after v2's two length fields; the encoder keeps their bytes raw when
DEFLATE would not make the complete envelope smaller. Measure on the target
workload before enabling it.

The library continues to emit v1 from existing `MarshalBinary` methods. A
provider or store can opt in explicitly at its boundary:

```go
v1, err := delta.MarshalRunBinary()
if err != nil { /* handle */ }

v2, err := frame.ConvertFrameV1ToV2(v1, receiveLimits)
if err != nil { /* handle */ }
```

For a run-v2 RGA group that has already negotiated outer v2, prefer the direct
RGA APIs. They preflight the final v2 frame rather than constructing and then
validating an intermediate v1 envelope:

```go
update, err := document.InsertRunFrameV2WithLimits(0, pastedText, receiveLimits)
if err != nil { /* reject the local edit before mutation */ }

checkpoint, err := document.SnapshotRunFrameV2CurrentStateWithLimits(receiveLimits)
if err != nil { /* handle */ }
```

`Delta.MarshalRunFrameV2`, `RGA.MarshalRunFrameV2`, and the matching
anti-entropy methods emit the same canonical run-v2 payload as their v1
counterparts. This is an outer-representation optimization only: it does not
combine scalar identities, change RGA ordering, or make an outer-v2 group
compatible with a v1 peer. The direct path can use a compressed final-frame
budget smaller than the equivalent v1 envelope, but its decoded payload still
must fit `MaxPayload`.

For raw interactive payloads, the direct APIs write the canonical payload into
the final v2 envelope after first checking its exact `MaxFrameBytes` budget.
They therefore avoid a separate payload allocation and copy. Payloads at the
compression threshold or above still use one `MaxPayload`-bounded temporary
buffer: the encoder must inspect the complete payload before choosing raw or
DEFLATE mode. This is a local allocation optimization, not a relaxation of
output limits or canonicality checks.

For a v2 group, bind `WireFormatVersion: frame.FormatVersionV2` in its
`replica.Protocol`. `NewChange`, `Inbox`, snapshots, recovery plans, and
checkpoints then reject an otherwise valid v1 frame rather than silently
downgrading. `WireFormatVersion: 0` retains the legacy v1 default for existing
Go literals and decoded old JSON manifests.

```go
manifest, err := replica.NewManifest("notes/42", "example.com/note/v1", 1, replica.Protocol{
    StateID:           crdt.TypeIDRGARunState,
    DeltaID:           crdt.TypeIDRGARunDelta,
    SemanticsVersion:  2,
    WireFormatVersion: frame.FormatVersionV2,
}, crdt.ProtocolPolicy{})
```

The negotiated frame version is included in manifest hashes and checkpoint
digests. Authenticate the exact manifest before accepting bytes; a TypeID,
CRC-32C checksum, or compression mode does not authenticate a peer.

## Wire grammar

All integers are shortest-form unsigned LEB128 `uvarint` values. `bytes` is
one `uvarint` length followed by exactly that many bytes.

```text
frame             = "CRDT" version type-id codec-id payload-mode raw-length encoded-length encoded-payload checksum
version           = uvarint(2)
type-id           = uvarint ; non-zero
codec-id          = bytes
payload-mode      = uvarint(0 raw / 1 raw-DEFLATE)
raw-length        = uvarint
encoded-length    = uvarint
encoded-payload   = exactly encoded-length bytes
checksum          = four-byte big-endian CRC-32C (Castagnoli)
```

The checksum covers every byte after `"CRDT"` through `encoded-payload`; it
does not authenticate or encrypt data. Mode `0` requires
`raw-length == encoded-length`. Mode `1` is a complete RFC 1951 raw-DEFLATE
stream that expands to exactly `raw-length` bytes. A decoder must reject an
unknown mode, a non-canonical varint, mismatched lengths, an invalid stream,
or a stream whose output is shorter or longer than `raw-length`.

The exact compressed bit stream is not canonical: a sender may use a different
DEFLATE level while preserving the same decoded canonical CRDT payload. The Go
encoder uses `flate.BestSpeed` to favor interactive CPU cost; it chooses mode
`1` only when that full v2 frame is smaller than mode `0`.

## Correctness and resource bounds

1. Check the transport body cap and `MaxFrameBytes` before parsing.
2. Verify CRC-32C and parse the outer fields before allocating decoded output.
3. Reject `raw-length > MaxPayload` before DEFLATE expansion; read at most
   `raw-length + 1` decoded bytes and require exact completion.
4. Pass the decoded payload to the unchanged type-specific decoder. Canonical
   RGA run and rich-text checks canonicalize the **decoded payload**, not an
   outer v1 envelope, so outer-v2 does not weaken their invariants.
5. Apply the frame only after manifest, type, codec, semantic version, and
   outer frame version all match. Conversion APIs are explicit
   (`ConvertFrameV1ToV2` and `ConvertFrameV2ToV1`); decoding never downgrades.

Run-v2 canonicality checks rebuild and compare the decoded payload, never a
temporary v1 envelope. This keeps canonical validation independent from the
compressed transport budget while preserving the same per-scalar HLC tags and
parent links.

The payload limit bounds memory amplification, but compression is not
confidentiality. Use TLS and application-layer authorization for live traffic;
avoid accepting an Internet-sized compressed body merely because it can expand
within a process-wide default limit.

## Browser and native interoperability

The Go/Wasm RGA runtime uses the same Go frame decoder and therefore can apply
outer-v2 frames once the host authenticates a v2 manifest. The dependency-free
TypeScript `decodeFrame` helper currently validates v1 envelopes only; a pure
TypeScript consumer must not advertise outer v2 until it has a bounded raw
DEFLATE decoder with the same limits. Do not pretend that an in-memory TypeID
or a browser's ability to pass bytes to Wasm is a complete provider
negotiation.

The Go tests cover conversion, corrupt or over-expanding inputs, fuzzed outer
frames, final-frame versus decoded-payload bounds, manifest/checkpoint/recovery
version binding, and a three-editor RGA simulation with duplicate and shuffled
v2 frames. Run controlled measurements with:

```sh
go test -run='^$' -bench='BenchmarkFrameUpdateFormats|BenchmarkRGADeltaWireProtocols|BenchmarkRGASmallDeltaFrameV2Encoders' -benchmem ./encoding ./text
```
