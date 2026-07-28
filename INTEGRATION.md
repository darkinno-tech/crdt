# End-to-end integration tutorial

[English](INTEGRATION.md) | [简体中文](INTEGRATION.zh-CN.md)

This tutorial connects the library primitives into a verifiable flow: two HTTP
receivers accept the same encoded deltas, duplicate delivery is observed as
idempotent, and a business example covers a partition, an add-wins conflict,
and a safe OR-Set restart. The HTTP utility is a test probe, not a production
replication service.

## Scenario and data model

The examples model a field-maintenance operation.

| Business fact | CRDT | Reason |
| --- | --- | --- |
| Inspections completed by each technician | `GCounter` | Each technician only adds to its own component; the dashboard sums components. |
| Tasks open in the workboard | `ORSet[string]` | A task may be independently added or removed while a vehicle is offline. |

Do not model a decreasing value, balance, exclusive reservation, or workflow
transition with `GCounter`. Do not use `ORSet` as a last-write-wins document
store. Keep those invariants in an authoritative service and replicate only
facts whose merge policy is acceptable to the business.

## 1. Run the real business example

Requirements: Go 1.21 or later and a local checkout. From the repository root:

```sh
go test ./...
go run ./examples/collaborative-board
```

Expected output:

```text
completed-inspections=5
open-tasks=[close-shift inspect-pump replace-filter]
```

The program serializes and decodes every delta before applying it. It delivers
one counter delta and one reopened-task delta twice. While the field van is
partitioned, it removes `inspect-pump` after observing it while dispatch
independently adds that task again. The new add has a different tag, so it
survives the observed remove: this is add-wins. It then creates
`SnapshotCurrentState`, restores a same-ID field replica, and emits a new
mutation safely. See [the source](examples/collaborative-board/main.go).

## 2. Exercise local HTTP delivery

`crdt-sync-probe` is a short-lived transport check. It accepts only encoded
library deltas and exposes no business API. The following starts two receivers,
broadcasts the same mutations to both, and checks their returned states. The
temporary directory keeps the generated token and logs out of the repository.

```sh
umask 077
scenario_dir="$(mktemp -d)"
openssl rand -hex 32 > "$scenario_dir/probe.token"
go build -o "$scenario_dir/crdt-sync-probe" ./cmd/crdt-sync-probe

"$scenario_dir/crdt-sync-probe" -mode serve -listen 127.0.0.1:49511 -replica dock-a -token-file "$scenario_dir/probe.token" > "$scenario_dir/dock-a.log" 2>&1 &
pid_a=$!
"$scenario_dir/crdt-sync-probe" -mode serve -listen 127.0.0.1:49512 -replica dock-b -token-file "$scenario_dir/probe.token" > "$scenario_dir/dock-b.log" 2>&1 &
pid_b=$!

token="$(tr -d '\n' < "$scenario_dir/probe.token")"
ready=''
for attempt in 1 2 3 4 5; do
  if curl -fsS -H "X-CRDT-Probe-Token: $token" http://127.0.0.1:49511/state >/dev/null && curl -fsS -H "X-CRDT-Probe-Token: $token" http://127.0.0.1:49512/state >/dev/null; then
    ready=1
    break
  fi
  sleep 1
done
test -n "$ready" # Inspect "$scenario_dir"/*.log if this fails.

go run ./cmd/crdt-sync-probe -mode send -target http://127.0.0.1:49511,http://127.0.0.1:49512 -replica receiving-gate -token-file "$scenario_dir/probe.token" -counter-increment 4 -element pallet-042 -duplicates 7
```

Each JSON report must contain exactly one `receiving-gate: 4` component and one
`pallet-042` element, despite seven deliveries of each delta. Send one more
independent mutation and fetch the state from both receivers:

```sh
go run ./cmd/crdt-sync-probe -mode send -target http://127.0.0.1:49511,http://127.0.0.1:49512 -replica forklift-9 -token-file "$scenario_dir/probe.token" -counter-increment 2 -element pallet-043 -duplicates 3

curl -fsS -H "X-CRDT-Probe-Token: $token" http://127.0.0.1:49511/state
curl -fsS -H "X-CRDT-Probe-Token: $token" http://127.0.0.1:49512/state
```

Both final responses should agree on `counts` (`receiving-gate: 4`,
`forklift-9: 2`) and sorted elements (`pallet-042`, `pallet-043`). Stop only
the PIDs created above, then remove the exact temporary directory:

```sh
kill "$pid_a" "$pid_b"
wait "$pid_a" "$pid_b" 2>/dev/null || true
rm -rf "$scenario_dir"
```

The probe requires `X-CRDT-Probe-Token`, binds to loopback by default, and
limits request bodies to 1 MiB. It has no TLS, durable state, membership,
replay policy, or authorization model. Do not expose it to the public Internet.

## 3. Production integration contract

The probe demonstrates the boundary that a consuming application owns:

1. Assign every live logical replica a globally unique, non-blank ID. A process
   that reuses an OR-Set ID after restart must restore its HLC state; a process
   that reuses an MV-Register ID must restore its causal snapshot. Otherwise it
   needs a fresh ID.
2. On a local mutation, call `Add`, `Remove`, `Increment`, or `Set`; persist the local
   CRDT state and its encoded delta in one durable transaction/outbox. Persist
   OR-Set state with the HLC state from `SnapshotCurrentState()` or
   `MarshalBinaryWithClockState()`.
3. Authenticate and authorize a sender before accepting its payload. Enforce
   application-specific bounds for message bytes, elements, tags, and strings.
   Decode untrusted input with `Unmarshal*WithLimits`, then call `ApplyDelta`
   in the same durable transaction that records the receipt.
4. Retry outbox items until the receiver acknowledges them. CRDT joins tolerate
   duplicate and reordered delivery; they do not replace networking,
   persistence, authentication, or business authorization.
5. Periodically exchange full state or Merkle summaries to discover missing
   history, then merge state to repair it. A retry queue alone cannot repair a
   delta lost before it entered that queue.
6. Before exchanging experimental LWW-Map, RGA, or OR-Tree frames, authenticate a
   connection/setup capability advertisement built from
   `crdt.ProtocolPolicy{AllowExperimental: true}.FrameTypes()`. Both peers must
   advertise the same state/delta pair before either sends that type. Unknown,
   reserved, or not-mutually-enabled types remain a protocol error.

An OR-Set receive path has this essential shape:

```go
encoded := boundedRequestBody(request)
delta, err := set.UnmarshalORSetDeltaWithLimits(encoded, taskCodec, limits)
if err != nil {
    return badRequest(err)
}
if err := workboard.ApplyDelta(delta); err != nil {
    return internalError(err)
}
// Atomically persist workboard state, its HLC state, and receipt/outbox data.
```

`taskCodec.ID`, `Marshal`, and `Unmarshal` are wire-contract fields. Keep the
codec ID stable across replicas, make encoding deterministic, and deliberately
version it when the byte format changes. Real tasks normally use canonical IDs,
not arbitrary display text.

## 4. Stable G-Set and MV-Register integration

G-Set and MV-Register are stable framed protocols included by the zero-value
`crdt.ProtocolPolicy`. A G-Set is grow-only: use OR-Set when the product needs
removal. An MV-Register `Set` can retain multiple causally concurrent values;
read `Values()` and make the product-level choice explicit rather than assuming
a single last writer wins.

Persist an MV-Register state frame and its `Snapshot()` atomically with the
outbox/receipt transaction. Restore a same-ID replica with
`register.NewMVRegisterFromSnapshot`; state bytes alone omit its causal context.
No experimental opt-in is required for G-Set or MV-Register frames.

## 5. Experimental LWW-Map, RGA, and OR-Tree integration

LWW-Map (`lww.Map`), RGA (`text`), and OR-Tree (`tree`) are framed, HLC-backed
experimental protocols. They are suitable only after the capability check above succeeds;
the policy is local to a replication group and is not a dynamic plugin
mechanism. Use each concrete decoder after the frame type is accepted—for
example, `text.UnmarshalRGADeltaWithLimits` for RGA deltas and
`tree.UnmarshalDeltaWithLimits` for OR-Tree deltas. Do not dispatch an
untrusted frame to a type merely because it has a valid checksum.

Persist a local LWW-Map, RGA, or OR-Tree state frame and its HLC state atomically with the
outbox/receipt transaction. Restore a same-ID replica only through
`SnapshotCurrentState()` and the package's `NewFromSnapshot`; state bytes alone
cannot prove the next locally emitted tag will be unique. RGA and OR-Tree retain
delete tombstones for out-of-order delivery. Exact acknowledgement-based
compaction for them is not implemented, so an experimental integration must
budget, monitor, and retain those tombstones rather than calling a generic GC.

## 6. Recovery, anti-entropy, and tombstones

Bootstrap a new or recovering replica from a complete state snapshot. For an
OR-Set, never restore a same-ID replica from `MarshalBinary()` bytes alone: its
next HLC tag might collide with an earlier local tag. Instead persist the state
frame and its HLC state atomically, for example with `SnapshotCurrentState()`
and `NewORSetFromSnapshot()`.

Do not compact tombstones merely because a peer's maximum tag is newer.
Out-of-order delivery means a maximum tag does not prove a gap-free history.
Use `tombstonegc.Coordinator` only with an authenticated, authoritative active
membership view and exact `TombstoneTags()` acknowledgements for the current
membership epoch. A retired member must bootstrap from a post-compaction
snapshot before returning.

The repository integration test is another executable reference: it checks
three-replica delta delivery, batching, recovery, and a Merkle anti-entropy
difference before convergence.

```sh
make test-integration
```

## Acceptance checklist

| Check | Required evidence |
| --- | --- |
| Stable identity | Replica-ID lifecycle, OR-Set HLC persistence, and MV-Register causal-snapshot persistence are documented and tested. |
| Duplicate/reordered delivery | The same encoded delta is delivered more than once and final state is unchanged. |
| Partition repair | A replica is bootstrapped from a snapshot or repaired through state/Merkle exchange, then converges. |
| Input safety | Authentication precedes decode; bounded decoders reject malformed, oversized, and type/codec-mismatched frames. |
| Business semantics | Product owners have accepted add-wins, grow-only G-Set and counter limits, and concurrent MV-Register value semantics. |
| Experimental protocol agreement | LWW-Map/RGA/OR-Tree are enabled only after authenticated bilateral `ProtocolPolicy.FrameTypes()` comparison; HLC state is persisted and their tombstones are retained. |
| Operations | Outbox retry, monitoring, backups, member retirement, and tombstone policy have a clear owner. |

Passing `go test` proves the library and examples at this revision. It does not
prove a browser, mobile client, production network, identity provider,
database transaction, or real member lifecycle; verify those boundaries in the
consuming service.
