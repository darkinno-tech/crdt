# darkinno-tech C++ CRDT client

`darkinno-tech::crdt::Rga` and `darkinno-tech::crdt::LwwMap` are C++20 RAII facades over
the bounded Rust [`rga-run-v2`](../../docs/protocol/rga-run-v2.md) and
[`lww-map-v1`](../../docs/protocol/lww-v1.md) runtimes. They own opaque
handles, free every returned C ABI buffer after copying it into standard C++
containers, and throw `darkinno-tech::crdt::Error` for a non-zero ABI status.

It is a supported native client binding, not an independent C++ wire
implementation. Rust remains the single source of truth for canonical frame
decoding, merging, pending-parent bounds, and HLC semantics.

```cpp
darkinno-tech::crdt::Rga document("cpp-device-7");
const auto delta = document.Insert(0, "Draft");  // already applied locally
document.Apply(std::span<const std::uint8_t>(delta));  // duplicate delivery is a no-op

darkinno-tech::crdt::LwwMap metadata("cpp-device-7");
const auto map_delta = metadata.Set("status", std::span<const std::uint8_t>{});
metadata.Apply(std::span<const std::uint8_t>(map_delta));
```

Build the Rust dynamic library for the exact target architecture first. The
checked-in conformance executable uses Go-published RGA and LWW-Map frames,
corrupt-frame atomicity, duplicate/reordered three-replica sessions,
tombstones, and same-ID snapshot recovery:

```sh
make cpp-test
make cpp-benchmark
```

`State()` only returns a complete RGA or LWW-Map snapshot. Persist it
atomically with `Clock()` plus the application-owned frontier/outbox before
reusing a replica ID. Pass the same manifest-supplied `crdt_limits` to the
`Rga` replica-ID and `ClockState` constructors; recovery must not widen frame,
graph, tombstone, or pending-parent limits. Authenticate and authorize the
selected manifest before `Apply`; CRC-32C is not authentication, encryption,
or replay protection.
