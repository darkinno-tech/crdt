# Cross-library CRDT text baseline — 2026-07-30

This report fills a narrowly defined comparison gap. It is reproducible source
evidence, not a claim that one library is universally faster: darkinno-tech is Go,
Yjs is JavaScript, their wire formats are incompatible, and their text storage
models deliberately make different trade-offs.

## Workload contract

The `initial` scenario creates two fresh replicas, inserts a pre-created ASCII
string into the source at offset zero, encodes the source's initial update,
applies it to the target, and verifies target text equality. Fixture setup and
final full-state encoding are outside the timed region; encoded update/state
byte counts are recorded separately. A reported sample averages 20 operations;
each size has two unreported warm-up batches and five reported batches.

| Side | Implementation | Protocol / API |
| --- | --- | --- |
| darkinno-tech | Go `text.RGA` | run-v2 `InsertRunBinaryWithLimits`, decode, `ApplyDelta` |
| Comparator | `yjs@13.6.31` | `Y.Text.insert`, `encodeStateAsUpdate`, `applyUpdate` |

This is the no-conflict initial-sync baseline only. It excludes WebSocket/WAN,
TLS, storage, authentication, reconnection, garbage collection policy, rich
text formatting, repeated edits, concurrent conflicts, and retained heap. Do
not divide the elapsed times across runtimes to make an absolute speed claim.

## Controlled local result

Host: Apple M4 Pro (12 logical CPUs, 24 GiB), Darwin arm64; Go `1.26.5`, Node
`v26.5.0`; harness revision `8cc25755dc5dbc4163c7c68eb2de80075f5d29bf`.
The `yjs` dependency is pinned by
[`bench/competitors/package-lock.json`](../../bench/competitors/package-lock.json).

| Runes | darkinno-tech median ms/op | Yjs median ms/op | darkinno-tech update/state bytes | Yjs update/state bytes |
| ---: | ---: | ---: | ---: | ---: |
| 4,096 | 6.076 | 0.117 | 36,774 / 36,774 | 4,113 / 4,113 |
| 16,384 | 25.650 | 0.079 | 147,367 / 147,367 | 16,403 / 16,403 |

The byte comparison is valid for this exact interoperably **unrelated**
workload: it reveals that darkinno-tech's stable per-Unicode-scalar RGA identifiers
cost about nine times the bytes of Yjs's compact single-string initial update.
The time rows are useful regression baselines within their own runtime, but
not a direct cross-language capacity ranking. In particular, Yjs can represent
this one uninterrupted insertion compactly, while darkinno-tech keeps independent
positions so deletes, out-of-order delivery, anchors, and future concurrent
inserts preserve their documented RGA semantics.

That is a design trade-off, not a defect. A separately negotiated outer frame
v2 may compress repeated run-v2 payload fields for large pastes and snapshots,
but it retains the same independently addressable HLC tags and parent links.
It must not be presented as changing this cross-library byte ratio or as making
the two wire formats interoperable. For a v2-negotiated Go/Wasm group,
`InsertRunFrameV2WithLimits`, `MarshalRunFrameV2`, and
`SnapshotRunFrameV2CurrentStateWithLimits` write that representation directly;
small edits may still use a raw v2 payload when compression would not reduce
the complete envelope.

## Offline-concurrent, duplicate, and reordered delivery

`offline-concurrent` closes the first planned comparison cell without changing
either stable wire protocol. A seed writes the same ASCII base text; two
writers receive it, independently replace the same middle scalar with `A` or
`B`, and exchange each other's update twice. A third, initially empty observer
receives the right update twice, the left update twice, and only then the base
update twice. Every replica must converge, and the darkinno-tech observer must leave
no pending parents. Reported update bytes are the one base update plus the two
unique writer updates; state bytes are the final left-replica state.

| Base runes | darkinno-tech median ms/op | Yjs median ms/op | darkinno-tech update/state bytes | Yjs update/state bytes |
| ---: | ---: | ---: | ---: | ---: |
| 4,096 | 14.639 | 0.444 | 36,949 / 36,897 | 4,177 / 4,181 |
| 16,384 | 62.830 | 0.527 | 147,414 / 147,362 | 16,473 / 16,477 |

This verifies each implementation's own convergence under the same edit and
delivery trace. It does not prove identical visible tie-break presentation
between independent CRDT algorithms, browser/WAN performance, retained heap,
or end-to-end provider recovery. The time columns remain runtime-specific;
the wire-byte columns make the representation trade-off observable, not a
universal capacity claim.

## Supplied-host darkinno-tech confirmation

The same source was cross-compiled with `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`.
Its SHA-256 (`06e91e54085e031b08d521fa225c40e7bbf3047094f30911a88409457bb0d1a2`)
was checked on each temporary remote copy before execution. Both supplied hosts
reported Debian GNU/Linux 13, `linux/amd64`, and four vCPUs. Five samples again
averaged 20 operations; temporary mode-0700 directories and binaries were
removed after the runs.

| Workload | Host A median ms/op | Host B median ms/op | Update/state bytes |
| --- | ---: | ---: | ---: |
| 4,096 runes | 22.609 | 22.700 | 36,774 / 36,774 |
| 16,384 runes | 102.368 | 103.812 | 147,111 / 147,111 |

Neither host had Go or Node installed. The self-contained Go binary makes the
darkinno-tech remote figures valid controlled evidence; no global package was
installed merely to obtain a Yjs number. The Yjs column remains local Node
evidence until a separately approved, pinned Node runtime can be provisioned
in a disposable remote environment.

## Reproduce

```sh
npm --prefix bench/competitors ci --ignore-scripts
revision="$(git rev-parse HEAD)"
reports="$(mktemp -d)"
go run ./cmd/crdt-compare \
  -scenario=initial -sizes=4096,16384 -samples=5 -warmups=2 -iterations=20 \
  -revision="$revision" -output="$reports/darkinno-tech-initial.json"
npm --prefix bench/competitors run yjs -- \
  --scenario initial --sizes 4096,16384 --samples 5 --warmups 2 --iterations 20 \
  --revision "$revision" --report "$reports/yjs-initial.json"
go run ./cmd/crdt-compare \
  -scenario=offline-concurrent -sizes=4096,16384 -samples=5 -warmups=2 -iterations=20 \
  -revision="$revision" -output="$reports/darkinno-tech-offline.json"
npm --prefix bench/competitors run yjs -- \
  --scenario offline-concurrent --sizes 4096,16384 --samples 5 --warmups 2 --iterations 20 \
  --revision "$revision" --report "$reports/yjs-offline.json"
```

Run each side on the same idle host and retain both JSON outputs with host,
runtime, revision, and package-lock data. Both runners create the requested
report parent directories. The runner rejects invalid sizes and verifies
convergence on every operation; `node --no-experimental-webstorage --expose-gc`
avoids Node's localStorage warning and performs collection before each Yjs
sample.

## Next comparison cells

The two completed cells still do not justify a product-speed claim. Before one,
add the remaining trace-driven cells for both libraries:

1. Long editing session with reconnect and state-vector/snapshot catch-up;
   measure peak retained memory in isolated runtime-specific profiles.
2. Authenticated provider fan-out (1/4/16 observers) on the supplied Linux
   hosts; separate loopback relay overhead from WAN/TLS and persistence.
3. Rich text formatting and relative-cursor behavior, reported as feature
   coverage and failure handling rather than byte-only performance.

darkinno-tech should use these cells to decide a future protocol evolution. The
current stable run-v2 contract must not be silently replaced with Yjs framing
or a new chunk model merely to improve this no-conflict number.
