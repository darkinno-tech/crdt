# im10furry native Rust CRDT runtime

`im10furry-crdt-rga` is the bounded native implementation of two stable Go-wire
protocols: [`rga-run-v2`](../../docs/protocol/rga-run-v2.md) TypeIDs `19/20`
(semantic version `2`) and [`lww-map-v1`](../../docs/protocol/lww-v1.md)
TypeIDs `9/10` (semantic version `1`). Each is independently negotiated.

It is deliberately not compatible with TypeScript's separately negotiated
`native-ts-v1` JSON updates or the legacy scalar RGA TypeIDs `11/12`.

## Security and correctness boundary

- Authenticate and authorize the replication-group manifest before calling
  `apply_frame`; CRC-32C detects accidental corruption, not a malicious peer.
- Select compatible receiver limits. The defaults are mobile-oriented: 1 MiB
  frame/payload, 64 KiB identifiers, 100,000 retained nodes/tags/tombstones,
  and 10,000 pending nodes / 512 KiB pending charge.
- The decoder rejects non-shortest varints, length overflow, checksum/type/
  codec errors, invalid Unicode scalars, cycles, duplicate IDs, incomplete
  state frames, and non-canonical block partitions before it changes state.
- Deltas may have an external missing parent only inside the bounded pending
  queue. `encode_state()` fails until every parent is integrated.
- Persist the state frame, `clock_state()`, and application frontier/outbox in
  one transaction before restarting with the same replica ID. Tombstones are
  structural anchors; this SDK does not make tombstone GC safe.

## Local API

```rust
use im10furry_crdt_rga::{Limits, Rga};

let mut alice = Rga::new("alice-device-7", Limits::default())?;
let outgoing_delta = alice.insert(0, "Draft")?; // already applied locally

let mut bob = Rga::new("bob-device-4", Limits::default())?;
bob.apply_frame(&outgoing_delta)?;
assert_eq!(bob.text(), "Draft");
# Ok::<(), im10furry_crdt_rga::Error>(())
```

`insert` and `delete` use Unicode-scalar offsets and return the exact
already-applied canonical delta. `insert_at`, `delete_at`, and
`apply_frame_at` additionally accept a physical millisecond clock for
deterministic hosts and tests.

`LwwMap` stores opaque byte values under UTF-8 keys. It exposes `set`,
`delete`, `get`, canonical visible `keys`, `state`, and `clock_state`; use
`LwwMapLimits` from the authenticated manifest for frame/payload/key/value/
entry/tombstone bounds. It is not a generic fallback for tree, rich-text,
attachment, or other TypeIDs; see the
[type-coverage decision](../../docs/design/native-client-type-coverage.md).

## FFI and language bindings

The crate also emits a `cdylib` and `staticlib`. Its small, ownership-explicit
C ABI is in [`include/crdt_rga.h`](include/crdt_rga.h): returned bytes are
owned `crdt_buffer` values and must be released with `crdt_buffer_free`.
`crdt_rga` and `crdt_lww_map` serialize opaque handles with a mutex; callers
must still enforce their own lifecycle and avoid use after free.

The checked-in Python and Swift clients use this ABI. They are supported native
runtime bindings, not claims of independent Python/Swift wire implementations:
the Rust core remains their single semantic source of truth. See the
[multilanguage design](../../docs/design/native-multilanguage-rga.md) for the
decision and the explicit path to independently maintained ports.

## Verification

```sh
make rust-test
make python-test
make swift-test # macOS: uses the no-framework Swift conformance executable
make rust-benchmark
```

The conformance suites consume Go-published fixtures and check byte-for-byte
re-encoding, atomic invalid-frame rejection, duplicated/reordered
three-replica convergence, tombstones, and snapshot/HLC recovery.
