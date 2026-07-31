# DarkInno C++ RGA client

`darkinno::crdt::Rga` is a C++20 RAII facade over the bounded Rust
[`rga-run-v2`](../../docs/protocol/rga-run-v2.md) runtime. It owns its opaque
handle, frees every returned C ABI buffer after copying it into standard C++
containers, and throws `darkinno::crdt::Error` for a non-zero ABI status.

It is a supported native client binding, not an independent C++ wire
implementation. Rust remains the single source of truth for canonical frame
decoding, merging, pending-parent bounds, and HLC semantics.

```cpp
darkinno::crdt::Rga document("cpp-device-7");
const auto delta = document.Insert(0, "Draft");  // already applied locally
document.Apply(std::span<const std::uint8_t>(delta));  // duplicate delivery is a no-op
```

Build the Rust dynamic library for the exact target architecture first. The
checked-in conformance executable uses a Go-published frame, corrupt-frame
atomicity, a duplicate/reordered three-replica session, and same-ID snapshot
recovery:

```sh
make cpp-test
make cpp-benchmark
```

`State()` only returns a complete RGA snapshot. Persist it atomically with
`Clock()` plus the application-owned frontier/outbox before reusing a replica
ID. Authenticate and authorize the selected manifest before `Apply`; CRC-32C
is not authentication, encryption, or replay protection.
