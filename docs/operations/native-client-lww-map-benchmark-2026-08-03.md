# Native LWW-Map controlled validation — 2026-08-03

## Scope

This is a local regression baseline, not a production capacity or device-SLA
claim. The workload performs 128 local workboard writes, relays each TypeID 10
delta to another native replica, encodes a TypeID 9 state, and recovers a third
replica. The C++ case calls the same Rust runtime through the C ABI.

The behavioral simulation separately delivers writes, a delete tombstone, and
duplicates in different orders to three replicas, then restores a same-ID
replica from `{state, HLC}`. Rust also decodes/re-encodes Go's TypeID 9/10
vectors byte-for-byte; Python, Swift, and C++ exercise their real bindings.

## Environment and results

| Item | Value |
| --- | --- |
| Host | macOS 26.5.2, arm64 |
| Rust | `rustc 1.97.1` |
| Commands | `make rust-benchmark`; `make cpp-benchmark` |
| Inner samples | 8 operations per process |
| Rust LWW-Map set → relay → state → recovery | 1.687 ms/op |
| C++20 facade LWW-Map set → relay → state → recovery | 1.856 ms/op |

The process also measured the existing 1,536-scalar RGA path at 6.777 ms/op
in Rust and 7.010 ms/op through C++. These figures exclude network auth/TLS,
encrypted persistence, mobile battery, large values/tombstones, allocator
variation, and contention.

## Validation boundary

`make rust-test`, `make python-test`, `make swift-test`, and `make cpp-test`
cover vectors, malformed-frame atomicity, duplicate/reorder, tombstones,
snapshot/HLC recovery, and foreign-language FFI calls. They do not prove wheel
or XCFramework packaging, device behavior, remote CI, or production traffic.
Re-run with target limits, value sizes, storage latency, identity, TLS, and
fan-out before setting a product SLO.
