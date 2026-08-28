# darkinno-tech Swift CRDT client

`CRDTRGA.RGA` is a Swift wrapper around the bounded Rust `rga-run-v2` runtime.
Pass the same manifest-supplied `RGALimits` to initial construction and
`RGAClockState` recovery, so a restart cannot widen the frame, graph,
tombstone, or pending-parent policy. `CRDTRGA.LWWMap` exposes Go-compatible
LWW-Map TypeID `9/10` frames, opaque `Data` values, ordered `keys()`, and
manifest-supplied `LWWMapLimits`. It is not compatible with `native-ts-v1` or
scalar RGA v1.

The Swift package deliberately includes no bundled binary. Build the Rust
library for each deployment target, then point SwiftPM at it:

```sh
cargo build --manifest-path clients/rust/Cargo.toml
CRDT_RGA_LIBRARY_DIR="$PWD/clients/rust/target/debug" \
DYLD_LIBRARY_PATH="$PWD/clients/rust/target/debug" \
swift run --package-path clients/swift crdt-rga-swift-conformance
```

The runner uses Go-published RGA and LWW-Map vectors, duplicate/reordered
three-replica sessions, tombstones, and snapshot/HLC recovery. It avoids
XCTest because the current local Swift distribution does not ship it;
production Apple CI should run the same runner and the platform's XCTest/Xcode
test suite once packaged.

Authenticate and authorize the manifest before `applyFrame`. Persist `state()`
atomically with your clock/frontier/outbox record before reusing a replica ID.
CRC-32C does not authenticate, encrypt, or authorize a peer.
