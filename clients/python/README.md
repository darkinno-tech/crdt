# DarkInno Python RGA client

`crdt_rga.RGA` is a Python ownership wrapper for the bounded Rust
`rga-run-v2` runtime. It can locally insert/delete text, apply Go/Rust state
and delta frames, emit canonical frames, and recover a state snapshot.

It is not an independent Python wire implementation. This intentionally keeps
Python, Rust, and Swift on the same audited merge, canonicalization, resource,
and HLC behavior while the independent-port conformance suite grows.

```python
from crdt_rga import RGA

with RGA("python-device-7") as document:
    delta = document.insert(0, "Draft")  # already applied locally
    # Send delta only after transport-level authentication/authorization.
    document.apply_frame(delta)           # duplicate delivery is a no-op
    assert document.text == "Draft"
```

Build the dynamic Rust library first and set `CRDT_RGA_LIBRARY` to its absolute
path for packaged deployments. Development tests find
`clients/rust/target/debug` automatically:

```sh
make python-test
```

`state()` is only a complete RGA snapshot. Persist it atomically with the
runtime clock/frontier/outbox state owned by your application before reusing a
replica ID. Authenticate the manifest before `apply_frame`; CRC is not
authentication, replay protection, or encryption.
