# Durable state-vector sessions

## Decision

`crdt-durable-v1` remains the stable cursor-resume protocol. The durable
relay now offers `crdt-durable-v2` only when its configured `durable.Log`
implements `durable.StateVectorLog`. The bundled bbolt `durable.Store` does;
the existing Redis, PostgreSQL, and host `Log` implementations remain v1 until
they can provide the same complete bounded query.

The v2 state vector is a `replica.Frontier`: actor → greatest **contiguous**
Dot counter durably installed by the receiver. It is a delivery-progress hint
for selecting missing log entries. It is not an authenticated identity,
membership assertion, receiver receipt, anti-entropy commitment, or permission
to compact tombstones.

## V2 session sequence

```text
client checkpoint frontier ── state-vector hello ──> durable relay
                                                    │
                                            authenticate + subscribe authorize
                                                    │
                                      atomically snapshot high-water H
                                      select all Dots not covered by frontier
                                      register peer before releasing group lock
                                                    │
client <── v2 welcome(H), missing events, synced(H) ┘
  │
  ├─ install each event through Inbox and persist concrete state/frontier
  ├─ atomically persist state/frontier plus H in OnCatchUp(H)
  └─ receive ordered live events H+1 … and advance the cursor normally
```

Registering the peer under the group lock closes the catch-up-to-live gap. An
event committed after `H` enters the peer's bounded queue and is sent only
after `synced(H)`. The client rejects an unexpected live sequence and never
advances its cursor until its callback succeeds.

An out-of-order durable event can be retained by `replica.Inbox` while a
smaller Dot is missing. That pending frame is deliberately absent from the
state vector. After a reconnect the server sends it again, so the durable
checkpoint only needs to claim the installed contiguous Frontier; it must not
claim buffered data as installed.

## Resource and integrity boundaries

- State-vector JSON is bounded by the 16 KiB control-frame cap, a configured
  maximum actor count, actor-byte limit, UTF-8 validation, non-zero counters,
  and strictly sorted unique actors.
- `CatchUp` returns every missing stored event in durable sequence order or
  fails with `ErrReplayUnavailable`; it never truncates to fit event or byte
  budgets.
- The bbolt store maintains a validated actor-to-maximum-counter index. Stores
  written before this index existed rebuild it atomically on first append or
  catch-up; a corrupt record fails closed.
- A frontier that exceeds retained log knowledge fails closed. A valid frame,
  vector, checksum, or high-water still does not authenticate a peer or prove
  application-level persistence.
- The base `durable.Log` API was not widened. v1 cursor replay remains the
  compatibility and fallback path for stores without complete vector queries.

## Long-lived connection policy

Both sides perform bounded WebSocket Ping/Pong heartbeats (30 s interval,
10 s timeout by default). `durable.Config.RevalidateSubscription`, when
provided, runs before each server heartbeat and closes the connection if the
host reports an expired or revoked subscription. The application continues to
own token refresh, TLS, rate limits, tenant permissions, and audit policy.

The WebSocket library handles Ping/Pong only while reads continue. Every relay
client and server peer therefore retains its read loop for the connection
lifetime; a heartbeat timeout closes the connection and the reconnect loop
uses its bounded exponential backoff.

## Required application checkpoint contract

For v2, pass both callbacks to `durable.ClientConfig`:

```go
client, err := durable.NewReconnectClient(endpoint, manifest, durable.ClientConfig{
	StateVector: loadDurableFrontier,
	OnEvent: func(event durable.Event) error {
		return installEventAndPersistState(event) // concrete state + Frontier
	},
	OnCatchUp: func(highWater uint64) error {
		return persistStateFrontierAndCursor(highWater)
	},
})
```

`StateVector` must read the contiguous Frontier from the same durable
application checkpoint as concrete CRDT state. `OnCatchUp` must atomically
persist that state/frontier and `highWater`. If either callback fails, the
client leaves its cursor unchanged, reconnects, and asks again from the
persisted Frontier.

## Verification matrix

| Scenario | Evidence |
| --- | --- |
| Correctness | Vector filtering, actor-index rebuild, empty/ahead/over-budget rejection, and out-of-order Dot convergence tests. |
| Recovery | Real loopback v2 catch-up, forced disconnect/reconnect, and failed `OnCatchUp` cursor-retention tests. |
| Long session security | Periodic subscription revalidation closes a revoked connection before a heartbeat Ping. |
| Malformed input | Strict control decoding plus `FuzzWire` coverage for v1 and v2 controls. |
| Contention/performance | `BenchmarkDurableSameHostFanout` compares 1, 4, and 16 real same-host receivers that decode and install every event. |

Run the local gates with:

```sh
(cd durable && go test .)
(cd durable && go test -race .)
(cd durable && go test -run='^$' -fuzz=FuzzWire -fuzztime=250000x -parallel=1 .)
(cd durable && go test -run='^$' -bench='BenchmarkDurableSameHostFanout' -benchmem -count=3 -cpu=1,4 .)
```

These prove the bounded reference contract on loopback. They do not prove a
TLS deployment, browser/mobile transport, external identity service, real
network loss, storage durability configuration, or production capacity.
