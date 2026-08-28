# darkinno-tech Python CRDT client

`crdt_rga.RGA` is a Python ownership wrapper for the bounded Rust
`rga-run-v2` runtime. Supply `RGALimits` at initial construction and
`ClockState` recovery, so a restart cannot widen a replication group's frame,
graph, tombstone, or pending-parent policy. `crdt_rga.LWWMap` is the matching
wrapper for Go-wire `lww-map-v1` TypeIDs `9/10`, with opaque byte values and
optional `LWWMapLimits` at construction/recovery. Each protocol has its own
manifest.

It is not an independent Python wire implementation. This intentionally keeps
Python, Rust, and Swift on the same audited merge, canonicalization, resource,
and HLC behavior while the independent-port conformance suite grows.

```python
from crdt_rga import LWWMap, RGA

with RGA("python-device-7") as document:
    delta = document.insert(0, "Draft")  # already applied locally
    # Send delta only after transport-level authentication/authorization.
    document.apply_frame(delta)           # duplicate delivery is a no-op
    assert document.text == "Draft"

with LWWMap("python-device-7") as document:
    delta = document.set("status", b"review")
    document.apply_frame(delta)  # duplicate delivery is a no-op
    assert document.get("status") == b"review"
```

Build the dynamic Rust library first and set `CRDT_RGA_LIBRARY` to its absolute
path for packaged deployments. Development tests find
`clients/rust/target/debug` automatically:

```sh
make python-test
```

`state()` is only a complete snapshot. Persist it atomically with the runtime
clock/frontier/outbox state owned by your application before reusing a replica
ID. Authenticate the manifest and validate opaque Map values before
`apply_frame`; CRC is not authentication, replay protection, or encryption.
