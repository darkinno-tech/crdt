# End-to-end integration tutorial

[English](overview.md) | [简体中文](overview.zh-CN.md)

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
(cd examples && go run ./collaborative-board)
```

Expected output:

```text
completed-inspections=5
open-tasks=[close-shift inspect-pump replace-filter]
```

The program serializes every delta and decodes it with an explicit receive
budget before applying it. It delivers one counter delta and one reopened-task delta twice. While the field van is
partitioned, it removes `inspect-pump` after observing it while dispatch
independently adds that task again. The new add has a different tag, so it
survives the observed remove: this is add-wins. It then creates
`SnapshotCurrentState`, restores a same-ID field replica, and emits a new
mutation safely. See [the source](../../examples/collaborative-board/main.go).

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
limits counter/OR-Set request bodies to 1 MiB. It has no TLS, durable state,
membership, replay policy, or authorization model. Do not expose it to the
public Internet.

### RGA diagnostic boundary

`/rga` defaults to stable RGA run-v2 (TypeIDs 19/20). It is only a controlled
probe and does not negotiate a manifest or a production `ProtocolPolicy`. The
legacy scalar v1 format (TypeIDs 11/12) is stable compatibility mode and requires explicit
`-rga-protocol=v1` on both sender and receiver; mismatched frames are rejected
before text mutates. The route allows at most 16 MiB and 200,000 generated
runes per delta, sends an empty `204` acknowledgement, and reports only final
convergence data in `/state`.

```sh
# The default protocol on both receivers and this sender is stable run-v2.
go run ./cmd/crdt-sync-probe -mode send \
  -target http://127.0.0.1:49511,http://127.0.0.1:49512 \
  -replica text-gate -token-file "$scenario_dir/probe.token" \
  -counter-increment 0 -element '' \
  -rga-runes 4096 -rga-rune 'λ' -duplicates 3
```

The two final `text` objects must agree on `protocol`, `runes`, `sha256`, and
zero `pending`. This proves only the exercised in-memory duplicate/reorder path;
it does not prove HLC persistence, recovery, membership, or tombstone-GC safety.

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
6. Before exchanging any state or delta frame, authenticate one exact manifest
   agreement that binds its state/delta pair. Both peers must advertise the
   same pair before either sends that type. Unknown,
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
G-Set and MV-Register frames use the zero-value policy.

## 5. Stable text, rich text, and observed-remove tree integration

Run-v2 RGA (`text`, TypeIDs 19/20) is a stable framed protocol included by the
zero-value policy. Bind `text.StableFrameType()` and
`text.RunV2SemanticsVersion` in the authenticated manifest, decode deltas with
`text.UnmarshalRGARunDeltaWithLimits`, and never fall back to legacy v1 based
on a frame that failed to match. The legacy scalar RGA v1 (`11/12`), LWW-Set
(`lww.Set`), LWW-Map (`lww.Map`), and generic RGA list (`list.RGA`) are stable
and require the exact-manifest check above. The policy is local to a replication group
and is not a dynamic plugin mechanism. Do not dispatch an untrusted frame to a
type merely because it has a valid checksum.

Inline rich text (`richtext`, TypeIDs 23/24) is also stable under the zero-value
policy. Bind `richtext.SemanticsVersion` and one exact application renderer /
attribute `SchemaID` in its own manifest, decode with
`richtext.UnmarshalDeltaWithLimits`, and persist its state, shared RGA HLC
clock, delivery frontier, and outbox atomically. Attribute keys and values are
not HTML or authorization claims: validate the manifest-selected schema and
sanitize before rendering. The [rich-text v1 protocol](../protocol/richtext-v1.md)
defines canonical vectors, limits, and exact-acknowledgement compaction.

Observed-remove tree (`tree`, TypeIDs 17/18) is also stable under the zero-value
policy. Bind `tree.SemanticsVersion` and an exact application node-value
`SchemaID` in its own manifest, decode with `tree.UnmarshalDeltaWithLimits`,
and persist its state, HLC clock, delivery frontier, and outbox atomically.
Tree v1 uses immutable parent links with add/remove only: a move is a remove
plus a new node instance, never a rewritten parent link. The
[OR-Tree v1 protocol](../protocol/or-tree-v1.md) defines canonical vectors,
limits, and exact-acknowledgement compaction.

Persist a local LWW-Set, LWW-Map, RGA, or OR-Tree state frame and its HLC state atomically with the
outbox/receipt transaction. Restore a same-ID replica only through
`SnapshotCurrentState()` and the package's `NewFromSnapshot`; state bytes alone
cannot prove the next locally emitted tag will be unique. RGA and OR-Tree retain
delete tombstones for out-of-order delivery. RGA's `CompactTombstones` can
remove only deleted leaves; the application must first establish an authenticated
exact-acknowledgement epoch, durably save a post-compaction snapshot, and retire
old deltas. Tree's `CompactEligibleTombstones` can compact an already
exact-acknowledged deleted branch leaf-to-root; an unselected or live child
remains an anchor. LWW-Set and LWW-Map compact only through an
exact-acknowledgement coordinator; integrations must budget and monitor
tombstones until that proof is complete.

### 5.1 Attachment references

`attachment.Register` is a stable, schema-constrained use of the LWW-Map
frames for image, audio, video, and arbitrary data references. Create one
separate manifest per attachment group with state/delta IDs 9/10, schema ID
`github.com/im10furry/crdt/attachment-reference/v1`, an empty codec ID, and
`attachment.SemanticsVersion`. Do not use the same manifest for RGA text: one
manifest binds exactly one concrete CRDT protocol.

At the receive boundary, authenticate the exact manifest, decode with
`attachment.UnmarshalDeltaWithLimits` using both transport and attachment
retention limits, then apply the delta. Persist `SnapshotCurrentState()` and
restore a same-ID replica with `attachment.NewFromSnapshotWithOptions`.

An attachment reference is metadata only. The surrounding application owns
storage authorization, uploads/downloads, scanning, encryption, and retries.
After an authorized download, call `Reference.Verify` before decode or render;
it streams the object, rejects short/oversized content, and compares SHA-256.
The [attachment integration guide](attachment.md) and its
[runnable example](../../examples/attachment-collaboration) provide the complete
flow and limits checklist.

### 5.2 Browser and JavaScript/WebView RGA clients

`clients/typescript` keeps the cross-language boundary narrow: its TypeScript
module validates the bounded common frame envelope, while the Go/Wasm RGA
runtime calls the canonical decoder and merge engine. `make wasm` builds the
default run-v2 artifact (state/delta TypeIDs 19/20, semantics version 2), which
matches `crdt.DefaultRGAFrameType()`. `make wasm-test` verifies Go-to-client
frames and a duplicated/reordered three-replica session; `make wasm-v1-test`
separately verifies the legacy scalar-v1 artifact (TypeIDs 11/12).

First authenticate one exact manifest/capability agreement, including the
state/delta IDs and semantics version, then validate the application transport
body limit before calling `document.applyDelta`. CRC-32C detects accidental
corruption only. Persist the returned `{ state, clock, frontier }` as one
atomic local record; restoring only state permits a reused replica ID to emit
an unsafe HLC tag. Split an editor transaction larger than 64 KiB or 16,384
runes before local insertion, and use a Worker for long documents.

Each manifest binds exactly one RGA wire format. Do not connect a legacy v1
artifact to a run-v2 group, or vice versa. A native client without a compatible
Wasm runtime must implement and verify the normative [RGA run-v2 wire
protocol](../protocol/rga-run-v2.md), including its canonical-vector suite,
before it is admitted to a run-v2 replication group.

For an application-owned WebSocket integration reference, see the [WebSocket
provider guide](websocket-provider.md) and its [runnable example](../../examples/websocket-provider).

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

For a disposable local cache or rebuildable server-owned default that can
never merge a delayed operation, an explicit `tombstonegc.SimpleCollector`
provides bounded local-only collection. It does not relax this replicated
workflow; see [tombstone GC mode selection](tombstone-gc.md) before choosing
it.

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
| Protocol agreement | Every stable frame pair is enabled only after authenticated bilateral exact-manifest comparison; all HLC-backed protocols persist their clock state and retain tombstones until exact-ack compaction is authorized. |
| Operations | Outbox retry, monitoring, backups, member retirement, and tombstone policy have a clear owner. |

Passing `go test` proves the library and examples at this revision. It does not
prove a browser, mobile client, production network, identity provider,
database transaction, or real member lifecycle; verify those boundaries in the
consuming service.
