# Native TypeScript Text benchmark — 2026-08-03

## Scope and environment

This is a controlled local development measurement of the new plain-text
`NativeText` root in `native-ts-v1`. It is not a browser/mobile SLA, a rich-text
benchmark, or a Go RGA/Wasm comparison. The cold workload includes document
construction, Unicode-scalar node creation, canonical JSON validation/encoding,
and a remote merge assertion.

| Field | Value |
| --- | --- |
| Commit basis | `9107269` plus the NativeText candidate in this worktree |
| OS / architecture | macOS 26.5.2 / arm64 |
| Node / npm | v26.5.0 / 11.17.0 |
| Command | `npm --prefix clients/typescript run bench:native` |
| Sampling | 2 warmups, 5 samples; heap delta is process heap before/after, not retained memory |

## Workloads and results

| Workload | What it verifies | Payload / reads | Samples (ms/op) | Median (ms/op) |
| --- | --- | ---: | --- | ---: |
| cold Text insert + encoded merge, 4,096 UTF-16 units | 3,072 Unicode-scalar RGA nodes (`a😀中` × 1,024), canonical byte decode, remote convergence | 331,677 bytes | 51.007, 48.277, 50.048, 51.518, 47.725 | 50.048 |
| cached Text reads, 4,096 UTF-16 units | 262,144 alternating `length`/`toString()` reads after an outside-timer projection | 262,144 read pairs | 29.970, 28.520, 37.386, 26.555, 26.915 | 28.520 |

The same run also retained the existing Map/Array workloads. They are regression
signals only because the two new rows have different work shapes; this report
does not claim a relative speedup or a capacity threshold.

## Local update-path optimization follow-up

The local write path previously normalized a one-operation update before
admission, then normalized the accumulated update again before emitting it.
Each normalization canonicalizes its whole JSON envelope to enforce the byte
limit. The first benchmark made that repeated work visible in the cold Text
path, where the same 3,072-entry operation was repeatedly traversed.

The follow-up keeps the boundary checks but changes their placement:

- Normalize one local operation before state admission, and compute its
  canonical byte size once for the exact future-envelope budget.
- Store a private normalized copy in the pending transaction. This preserves
  the published update if JavaScript callers mutate a returned operation
  object at runtime despite its readonly TypeScript contract.
- Emit the already-admitted pending operation list directly. Incoming updates
  still use full `normalizeUpdate` validation before any mutation.

On the same host, Node version, command, payload, warmup count, and five-sample
scheme, the cold Text workload changed as follows. These are controlled local
development measurements rather than browser, mobile, retained-memory, or
production-capacity claims.

| Version | Samples (ms/op) | Median (ms/op) | Change from baseline |
| --- | --- | ---: | ---: |
| NativeText baseline | 51.007, 48.277, 50.048, 51.518, 47.725 | 50.048 | — |
| Local preflight + flush optimization | 39.547, 36.109, 39.436, 35.659, 36.312 | 36.312 | -27.4% |

The optimized run also retained the 331,677-byte canonical wire payload and
the remote convergence assertion. The focused regression test additionally
checks that the emitted bytes remain canonical and that a pending delete cannot
be changed through mutation of its returned JavaScript object.

## Interpretation and limits

- Text indexes are UTF-16 code units, while immutable wire nodes hold exactly
  one Unicode scalar. The benchmark deliberately includes a supplementary
  character to exercise both representations without permitting a surrogate
  split.
- `NativeText` caches the visible scalar projection and joined string. The
  cached-read result describes an idle UI read phase; an insert or tombstone
  invalidates both caches and the following read is linear in visible nodes.
- One scalar per retained node intentionally trades metadata and update size
  for deterministic reordering/delete-before-insert semantics. The default
  `maxTextItems`, `maxTextTombstones`, update-byte, and pending-parent limits
  remain the resource boundary; callers should batch edits with `transact()`
  and use a Worker for a large body.
- V8 heap deltas varied with garbage collection and are diagnostic only. They
  are not retained-memory, browser, device, or production-capacity evidence.

## Reproduce

```sh
npm --prefix clients/typescript run bench:native
npm --prefix clients/typescript test
make wasm
CRDT_WASM_DIR="$PWD/.tmp/crdt-rga-wasm" npm --prefix clients/typescript run test:compat
```
