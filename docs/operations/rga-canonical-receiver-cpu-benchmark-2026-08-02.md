# RGA canonical receiver CPU and allocation benchmark — 2026-08-02

## Decision and scope

Accept the internal canonical-order reuse optimization for decoded run-v2 and
packed-v3 deltas. It removes a second `O(n log n)` tag sort from the common
receive-and-install path without changing wire bytes, TypeIDs, manifest
semantics, decoder limits, or RGA ordering.

This report is a controlled local regression baseline, not a WAN, TLS,
storage, authorization, browser/device, or production-capacity claim.

## Why this path was selected

CPU profiles of a 100,000-node canonical frame receiver showed two independent
sorts of the same opaque node map:

1. The decoder sorts nodes to reconstruct and byte-compare the canonical
   payload before accepting a frame.
2. `ApplyDelta` immediately sorts that same map again to determine whether the
   complete delta is a resolved linear run.

The first sort is a security and compatibility requirement and remains. The
second is avoidable when the decoder has already accepted an exact canonical
payload. The decoder now retains its sorted IDs as an unexported hint. Before
using it, `ApplyDelta` still checks length, strictly increasing order, and map
membership; any invalid, stale, locally assembled, merged, partial, branching,
duplicate, or out-of-order delta falls back to the former conservative path.

The resolved-run installer also writes each newly created non-first parent’s
known-single child directly into the inline child index. Only the first node
uses the general sibling ordering algorithm, preserving deterministic placement
beside existing concurrent children.

## Safety and resource boundary

- Input limits, tag conflict detection, canonical-byte comparison, acyclicity,
  parent completeness, HLC witness, tombstones, pending replay, and version
  advancement execute before or exactly as before the optimization.
- The cache is never decoded from wire and does not appear in a frame,
  snapshot, JSON form, or public API. A failed canonical comparison never
  returns it.
- Its length is bounded by the caller-selected `DecoderLimits.MaxElements`. In
  the normal decode-then-apply case it replaces the old temporary sorted-ID
  allocation, which lowered per-operation allocations in the comparison below.
  A caller that deliberately retains decoded `Delta` values after applying them
  retains this private slice too; durable queues should retain canonical frame
  bytes or bounded application data rather than unbounded decoded deltas.
- The optimization is not authorization, delivery receipt, membership, or
  tombstone-GC evidence. Manifest validation and authenticated transport remain
  required at the provider boundary.

## Method

| Item | Value |
| --- | --- |
| Host | Apple M4 Pro, macOS / darwin arm64 |
| Go | go1.26.5 |
| GOMAXPROCS | 12 (host default) |
| Baseline | `71f4631`, pre-optimization source |
| Candidate | same source plus this change, before rebasing to current beta |
| Samples | five independent samples, three operations each; median reported |
| Workload | one canonical 100,000-rune linear delta, decode with default bounds, install into a fresh RGA with explicit 200,000-node limits |

The benchmark deliberately excludes frame production from the timed region.
It therefore measures the receiver CPU and allocation peak relevant to
initial-sync installation rather than conflating it with sender work.

```sh
go test ./text -run '^$' \
  -bench '^BenchmarkRGAReceiveCanonicalLinearFrames$' \
  -benchtime=3x -benchmem -count=5
```

## Results

| Protocol | Baseline median | Candidate median | Change | Baseline alloc/op | Candidate alloc/op | Allocation change |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| run-v2 | 73.476 ms | 63.737 ms | -13.3% | 94.28 MB | 91.66 MB | -2.62 MB (-2.8%) |
| packed-v3 | 74.034 ms | 70.399 ms | -4.9% | 95.25 MB | 91.32 MB | -3.93 MB (-4.1%) |

The pprof cross-check agrees with those deltas: the baseline attributed 1.02 s
of samples to `sortedNodeIDs` under `ApplyDelta` and another 0.83 s to decoder
canonicalization. The candidate retains the required decoder sort (0.75 s)
but no longer attributes a second sort to `ApplyDelta`. Sample variance remains
visible on the shared host, especially for packed-v3, which is why the table
uses medians rather than a throughput SLO.

At the default one-million-node RGA limit and a two-million-node stress
instance, retained RGA memory remained about **456.2 heap-B/char**. This is a
direct in-memory RGA residency measurement; it does not represent queued decoded
delta retention described above.

## Validation matrix

| Layer | Evidence | Result |
| --- | --- | --- |
| Protocol correctness | canonical byte vectors; wrong-type and non-canonical frame rejection | pass |
| CRDT simulation | duplicate/reordered run-v2 and packed-v3 three-replica edits, tombstones, pending replay, snapshot recovery | pass; focused set repeated 20 times |
| Structural safety | batch sibling order plus complete eligible tombstone compaction | pass |
| Concurrency | `go test -race ./text ./richtext ./replica` | pass |
| Broad local suite | `go test ./...` | pass |
| Fuzz smoke | `make fuzz-smoke` | pass |
| Receiver performance | 100k canonical frame decode plus fresh-replica install | results above |
| Peak residency | 1M and 2M direct RGA resident benchmarks | pass; 456.2 heap-B/char |
| Live transport control | real local loopback WebSockets with every receiver decoding/installing through `replica.Inbox` | 1/4/16 receiver medians: 21.341 / 20.440 / 18.069 ms/op |

The live transport control is a real in-process WebSocket flow, but it has no
target storage, TLS, identity provider, WAN loss, checkpoint latency, or
production quota. It must not be used to set fleet capacity or durable receipt
SLOs.

## Follow-up: prove the single-chain graph shape before walking it

A second receiver profile, taken after canonical-order reuse, still showed the
general `acyclicAgainst` walk consuming 0.63 s (13.38% cumulative samples) on
the canonical large-paste workload. That walk is necessary for a partial,
branching, multi-block, or reordered delta. It is redundant for exactly one
fully decoded chain block: the decoder has already checked every later node's
implicit edge to its immediate predecessor, so the first node's parent is the
only edge that can return to any node in the frame.

The decoder now uses that O(1) proof only when the payload has exactly one
chain block. It rejects the frame when the first parent is present in the
decoded node set; an absent first parent remains a valid out-of-order delta.
All node blocks, multi-block payloads, and any shape not proven single-chain
continue through the former full `acyclicAgainst` traversal. Canonical-byte
comparison, complete-state parent checks, and `ApplyDelta` validation remain
unchanged.

The test matrix adds both sides of the proof for run-v2 and packed-v3:

- a byte-canonical two-node loop is rejected before returning a delta;
- a valid single chain whose first parent is outside the delta decodes, then
  converges after the retained parent arrives at the receiver.

### Controlled before/after measurement

This comparison used a clean detached worktree at `17728f3` for the baseline
and the same host, Go version, limits, frame bytes, sample count, and benchmark
code for the candidate. The detached worktree added only the decode benchmark
and was removed after measurement. Values are five medians of three operations
each for a canonical 100,000-rune linear frame.

| Layer | Protocol | Baseline median | Candidate median | Time change | Baseline alloc/op | Candidate alloc/op | Allocation change |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Decode only | run-v2 | 36.245 ms | 23.474 ms | -35.2% | 54.12 MB | 32.37 MB | -21.76 MB (-40.2%) |
| Decode only | packed-v3 | 36.300 ms | 24.042 ms | -33.8% | 55.04 MB | 32.73 MB | -22.31 MB (-40.5%) |
| Decode + fresh install | run-v2 | 56.365 ms | 44.149 ms | -21.7% | 91.10 MB | 69.16 MB | -21.94 MB (-24.1%) |
| Decode + fresh install | packed-v3 | 57.575 ms | 44.546 ms | -22.6% | 91.11 MB | 69.53 MB | -21.58 MB (-23.7%) |

The candidate pprof contains no `acyclicAgainst` samples on either canonical
single-chain benchmark. Canonical map sorting remains the intentional dominant
cost because it is still the compatibility and input-normalization boundary;
this change does not treat a sender's chain claim as trusted.

## Reproduction and follow-up

Run the focused receiver benchmark, the multi-replica simulations, race test,
and fuzz target after any change to RGA framing or `Delta` construction. Before
setting deployment limits, repeat measurements against the intended durable
store, authentication, TLS, loss/reconnect, checkpoint, and queue-budget
configuration.
