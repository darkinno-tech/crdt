# Native multilingual RGA delivery decision

## Decision

Build one complete bounded native Rust implementation of the stable
[`rga-run-v2`](../protocol/rga-run-v2.md) contract, then expose it through a
small owned-buffer C ABI to Python and Swift. Keep an explicit path for later
independent Python/Swift implementations, but do not call a thin frame decoder
or a language wrapper an independent CRDT implementation.

This is deliberately separate from `native-ts-v1`/`native-ts-nested-v1`.
Those are useful TypeScript-only JSON contracts but are not Go RGA frames,
TypeIDs `19/20`, or a safe upgrade path for a manifest-bound RGA group.

## Facts and constraints

| Fact | Evidence | Consequence |
| --- | --- | --- |
| New Go text groups use run-v2 state/delta TypeIDs `19/20`, semantic version `2`, empty codec. | [run-v2 protocol](../protocol/rga-run-v2.md) | A port must reject scalar-v1 and mismatched manifests; no auto fallback. |
| The protocol already has Go-generated canonical vectors. | [`rga-run-v2-vectors.json`](../protocol/testdata/rga-run-v2-vectors.json) | Every new runtime can test actual Go frames byte-for-byte before it advertises interoperability. |
| RGA merge includes out-of-order parents, tombstones, deterministic descending sibling traversal, and HLC recovery. | [protocol sections 3–8](../protocol/rga-run-v2.md) | Frame decode alone is not a usable local-first client. |
| TypeScript has a native contract and Go/Wasm RGA path. | [TypeScript client guide](../../clients/typescript/README.md) | Reimplementing or relabelling TS native updates would create a third incompatible protocol. |

## Architecture options

| Option | Correctness / security | Performance / cost | Decision |
| --- | --- | --- | --- |
| Independent Go-wire implementation in Rust, Python, and Swift immediately | Three merge/HLC/canonical decoders must stay identical; widest attack surface | Best language-native integration, but three expensive audits and divergent fixes | Defer after conformance maturity. |
| Rust core plus FFI wrappers | One bounded decoder/merge engine and one wire test surface; ABI ownership must be audited | Native speed and one implementation; FFI crossing costs are tiny versus a frame merge | **Adopt now.** |
| Go/Wasm everywhere | Reuses Go semantics, but desktop/mobile package and startup restrictions remain | Good browser portability; unsuitable as the only Python/Swift native story | Retain for browser groups only. |

The chosen Rust core is a real local client: local insert/delete, canonical
state/delta encode/decode, set-union merge, bounded pending parents,
tombstones, deterministic projection, and HLC state are all implemented. The
Python and Swift packages call that native core through `crdt_rga.h`; therefore
they provide supported client capability today but are not independent source
ports. This distinction is part of the public contract.

## Correctness model

1. A document contains immutable `(position, parent, Unicode scalar)` nodes
   plus tombstone positions. Merge is set union; conflicting payload for one
   position is rejected.
2. Decode validates the complete envelope, CRC-32C, shortest uvarints,
   lengths, type/codec, scalar values, duplicate positions/tombstones, graph,
   complete-state parent closure, and canonical payload re-encoding before
   mutation.
3. A delta may retain an external parent only in bounded pending state. State
   encoding fails while pending exists, which prevents an unrecoverable
   snapshot.
4. Visibility is iterative depth-first traversal; sibling identifiers sort
   descending at display time, while canonical encoder blocks sort ascending.
5. `state + HLC + frontier/outbox` is one durability unit. HLC is not optional
   metadata because same-ID recovery otherwise can repeat a mutation tag.

## Security model

The library's limits are a second line of defense, never admission control.
Hosts must cap transport bodies before copying, authenticate the user/peer,
authorize the exact document/schema/epoch/TypeID pair, apply replay policy,
and use TLS/encryption as appropriate. A checksum detects accidental
corruption only.

The Rust C ABI has opaque mutex-protected handles and transferred buffers with
one `crdt_buffer_free` release path. FFI callers receive status codes rather
than unwinding Rust panics across language boundaries. Python validates UTF-8
for local text; Swift owns `Data` copies rather than borrowing Rust state.

## Performance model and limits

Canonical serialization sorts at the wire boundary; visibility builds an
ephemeral deterministic projection. Current admission commits are deliberately
copy-on-validate, so a failed frame leaves a document, pending graph,
tombstones, and HLC untouched. That favors correctness and straightforward
auditability in this first native release but creates `O(retained state)` copy
cost per non-duplicate merge. Benchmark before changing it; a future
copy-on-write map/transaction plan is justified only if the realistic edit
benchmarks show it dominates the frame workload.

The included benchmark covers a 1,536-scalar local insert, native frame relay,
complete snapshot encoding, and recovery. It is a controlled regression signal
only: product limits require target CPU, memory, battery, transport, document,
tombstone, and concurrent-editor measurements.

## Conformance and rollout

| Gate | What it proves | What it does not prove |
| --- | --- | --- |
| `make rust-test` | Go vector decode/re-encode; malformed/limit atomicity; pending/reorder/duplicate/recovery behavior | Network auth, storage durability, Python/Swift dynamic loader packaging |
| `make python-test` | Real Python → C ABI → Rust core state/merge/recovery path | Independent Python semantics or wheel packaging |
| `make swift-test` | Real Swift → C ABI → Rust core vector/session/recovery path on macOS | iOS binary signing/XCTest/device performance |
| `make rust-benchmark` | Controlled native insert/relay/recovery regression number | Production capacity or latency SLA |

The next independent-port milestone is not “decode a frame.” It requires a
language implementation to pass the published vectors, hostile-frame atomicity
tests, randomized duplicate/reorder/partition convergence against Rust/Go,
HLC recovery, and the same benchmark workload. Only then may it be advertised
as an independent Python or Swift implementation.
