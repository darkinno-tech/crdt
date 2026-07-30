# Merkle State-Repair CLI Runbook

`crdt-merkle-sync` is a bounded, authenticated, offline repair tool for a
dedicated directory of stable G-Counter state frames. It turns the existing
Merkle digest primitive into one complete anti-entropy pass:

```text
local root ── equal ──> report convergence; no inventory or state transfer
     │
     └── different ──> remote inventory ──> fetch divergent states
                              │
                    G-Counter join on both sides
                              │
                     final Merkle-root equality
```

It is intentionally not a general “copy every valid CRDT frame” utility. A
frame envelope and a SHA-256 digest do not provide the semantic merge,
application codec, HLC recovery, tombstone retention, authenticated manifest,
or durable transaction boundary needed by the other CRDT types.

## Scope and decision record

| Dimension | Decision and evidence |
| --- | --- |
| Correctness | Admit only stable G-Counter state frames (TypeID 1); `counter.UnmarshalBinaryWithLimits` validates every stored and received state, and G-Counter `Merge` supplies the commutative, associative, idempotent join. Unknown, delta, and experimental states fail closed. |
| Integrity | The inventory carries a per-state SHA-256. The client verifies the downloaded body and response digest against that inventory, then compares both complete Merkle roots after repair. CRC-32C/frame validation remains input validation, not peer authentication. |
| Resource safety | The default limits are 1 MiB per state, 1,024 files, 65,536 counter components, 1,024-byte replica IDs, 128-byte flat keys, and a hard maximum of 4,096 files. Limits are checked before decoder allocation or mutation. |
| Security | The loopback-only server requires a non-empty shared token and compares a hashed token in constant time. `-token-file` avoids process-list exposure. Non-loopback listening is explicit; the server does not terminate TLS. |
| Performance | Equal roots use the cached `merkle.Tree.Root()` fast path and never request an inventory. A mismatch sends one bounded inventory plus only missing/divergent frames; it does not transfer matching frames. |
| Operations | Writes are temporary-file + file sync + rename + directory sync. A final root mismatch is a failed repair, not a false success. The caller retries after concurrent writers quiesce. |

The tool never treats a Merkle root as an acknowledgement, membership proof,
or authorization decision. It cannot authorize a tombstone compaction or make
an offline member safe to rejoin.

## State-directory contract

The directory is owned by exactly one tool process at a time. Do **not** start
`serve`, `sync`, or `gcounter-add` against the same directory concurrently;
the long-lived server keeps an in-memory validated view. Stop it before another
process opens that directory, then restart it if needed.

Each state is a regular, flat file named `<key>.frame`. Keys allow only ASCII
letters, digits, `.`, `_`, and `-`; paths, nested directories, symlinks, empty
states, and unrelated files are rejected. The server writes state atomically
inside that directory. The directory therefore must be dedicated to this tool
and protected by the same account/volume policy as the application's state.

Only canonical G-Counter state frames are supported today. The closed merger
registry is the extension point for a future type only after that type has a
concrete bounded decoder and a documented recovery/merge lifecycle. Do not add
a type merely because `crdt.ProtocolPolicy` exposes its TypeID.

## HTTP contract

All paths require `X-CRDT-Merkle-Token`; responses are `Cache-Control: no-store`.
The body and JSON responses are bounded by the configured state and inventory
limits.

| Method and path | Meaning |
| --- | --- |
| `GET /v1/merkle/root` | Version, cached root, and number of states. |
| `GET /v1/merkle/inventory` | Sorted `{key, sha256, type_id}` entries after a root mismatch. |
| `GET /v1/state/{key}` | One validated state frame, with `X-CRDT-State-SHA256`. |
| `PUT /v1/state/{key}` | Validate and merge one incoming state; success is `204`. |

The client rejects an inventory whose version, sort order, key, digest, or type
is invalid, and rejects a state body that differs from the advertised digest or
type. A root change while inventory is being discovered is retriable before
the client writes anything. A root difference after merge is an error: it can
mean a concurrent writer or a remote peer that did not complete a compatible
join.

## Local end-to-end exercise

Build the tool and generate a short-lived test token. Do not use production
state or a reusable credential for this exercise.

```sh
go build -o ./dist/crdt-merkle-sync ./cmd/crdt-merkle-sync
umask 077
openssl rand -hex 32 > ./merkle.token
chmod 600 ./merkle.token
mkdir -m 700 ./state-a ./state-b
```

Create intentionally divergent, valid G-Counter states while both directories
are offline:

```sh
./dist/crdt-merkle-sync -mode gcounter-add -state-dir ./state-a \
  -key orders -replica web-a -amount 2
./dist/crdt-merkle-sync -mode gcounter-add -state-dir ./state-b \
  -key orders -replica warehouse-b -amount 3
./dist/crdt-merkle-sync -mode gcounter-add -state-dir ./state-b \
  -key returns -replica warehouse-b -amount 1
```

Start one controlled receiver. The default bind is loopback; keep it that way
for a local exercise.

```sh
./dist/crdt-merkle-sync -mode serve -state-dir ./state-b \
  -listen 127.0.0.1:49821 -token-file ./merkle.token
```

In another terminal, repair A from B. The JSON report records how many remote
states were fetched, how many local files changed, and how many remote merge
requests were made. Its `final_root` is returned only after the two roots
match.

```sh
./dist/crdt-merkle-sync -mode sync -state-dir ./state-a \
  -target http://127.0.0.1:49821 -token-file ./merkle.token
```

Run the same command again: `already_equal` must be `true`, with zero fetched
states. Stop the receiver using its recorded PID before opening `./state-b` in
another tool process. For a three-replica partition exercise, repair A with B,
stop B, repair B with C, stop C, then start A and repair C with A. Each pass
joins rather than overwrites state, so all three converge once the cycle is
complete.

For a cross-host window, build a static binary, verify its SHA-256 before and
after transfer, keep the state directory and token mode `0700`/`0600`, and use
private networking or a TLS-terminating, access-controlled proxy. A raw
non-loopback listener requires `-allow-non-loopback`; the bearer token alone
does not encrypt traffic or establish peer identity.

## Validation and measurement

The command package contains four complementary checks:

- End-to-end `httptest` repair of divergent, local-only, and remote-only state.
- A three-replica partition/recovery simulation with a full convergence check.
- Unauthorized, unsafe-path, unsupported-type, oversized-body, malformed
  inventory/digest, and concurrent-client cases.
- Loopback HTTP + temporary-filesystem benchmarks for same-root and sparse
  repair paths.

Run the targeted gates and capture a local capacity sample before selecting
deployment limits:

```sh
go test ./cmd/crdt-merkle-sync
go test -race ./cmd/crdt-merkle-sync
go vet ./cmd/crdt-merkle-sync
go test -run '^$' -bench '^BenchmarkSynchronize(SameRoot|SparseRepair)$' \
  -benchmem -count=3 ./cmd/crdt-merkle-sync
```

For `N` objects and `K` mismatched objects, an equal-root pass is a cached-root
check plus one HTTP round trip. A mismatch uses `O(N)` inventory metadata and
`O(K)` state transfers/joins; each updated G-Counter frame is decoded and
re-encoded. This first version intentionally does not implement subtree or
multiproof pagination: the bounded flat inventory is simpler to audit at the
configured object cap. Measure real state size, component count, repair rate,
disk sync latency, and network RTT before raising any limit or adding a more
complex Merkle proof protocol.

Passing these tests proves this command's bounded G-Counter repair flow at this
revision. It does not prove a production TLS setup, identity provider,
application checkpoint transaction, live multi-process state ownership, or a
tombstone-GC policy.
