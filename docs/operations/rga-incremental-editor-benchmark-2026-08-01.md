# RGA incremental editor cross-host benchmark — 2026-08-01

## Scope and decision

This is a fresh, two-host measurement of the CodeMirror native single-range
incremental path. It supersedes the pre-incremental performance figures in
`rga-editor-bindings-2026-07-31.md`.

The tested source revision was `57ba7319c2051a4dcd48d6beee69018a322fbc3e`.
It contains the incremental editor engine (`2efc4e8` is an ancestor). Both
hosts ran the same built TypeScript output and the same Go/Wasm run-v2
artifact. `native_incremental` forwards the editor's one native change range;
`full_projection_fallback` intentionally withholds that range and exercises
the previous whole-document comparison path. A multi-range or inconsistent
event still takes the latter path for atomicity.

The result supports enabling the native path for a valid single CodeMirror
range. It does **not** establish browser, mobile, network, or production
capacity SLOs. Remote RGA application remains a full editor projection, and
the remote measurements below therefore are not expected to improve.

## Reproducibility and supply-chain boundary

| Item | Value |
| --- | --- |
| Node runtime | official Node `v26.5.0` Linux x64 archive; SHA-256 `9f619528f1db5ddc41dccf54211066fb42228d69a156733c69cb9d6cc92e358c` |
| TypeScript runtime bundle | built from the source revision above; SHA-256 `33c6986e738c8772592a26b876bb0976b1aaa568da1cee425a5f33bfad2364e7` |
| Go/Wasm runtime bundle | Go `1.26.5` build, run-v2; SHA-256 `1cd0232932ca875a31656c0176db8f2a1847b82b84e2864b0a884127db32a908` |
| Test bundle | `bindings.test.mjs`, `bindings.real.test.mjs`, and `wasm.integration.test.mjs`; SHA-256 `5f1d4ab708bd6ed840ff6f055201d1be15537f43a91c7b395d24bdc200d47e8b` |
| Isolation | each archive was copied to a unique mode-0700 `/tmp/crdt-incremental-bench.*` directory, SHA-256 verified there, then extracted without a system installation |
| CPU policy | each benchmark process used `taskset -c 0`; JavaScript work is single-threaded for these scenarios |

| Host | OS / CPU / memory | Node |
| --- | --- | --- |
| benchmark host A | Linux 6.12.38, Intel Xeon Platinum 8272CL, 4 vCPU, 3.83 GiB (`4,015,404 KiB`) | v26.5.0 |
| benchmark host B | Linux 6.12.38, Intel Xeon Platinum 8272CL, 4 vCPU, 3.83 GiB (`4,015,396 KiB`) | v26.5.0 |

The two hosts had no preinstalled Node or Go. The supplied portable runtime
was used only in the temporary directories. The small memory difference comes
from host accounting; it is reported exactly in KiB above rather than rounded
into a false hardware-equivalence claim.

## Correctness checks on each host

Both hosts ran these exact commands after archive verification:

```sh
node --test test/bindings.test.mjs test/bindings.real.test.mjs
CRDT_WASM_DIR=../wasm node --test test/wasm.integration.test.mjs
```

| Check | benchmark host A | benchmark host B |
| --- | ---: | ---: |
| Adapter and real-editor tests | 17 pass, 0 fail | 17 pass, 0 fail |
| Go/Wasm integration tests | 12 pass, 0 fail | 12 pass, 0 fail |

The first group includes actual CodeMirror 6 local/remote no-echo,
single-range Unicode conversion, chunk-boundary handling, atomic multi-range
fallback, and over-limit rejection. The second group runs actual Go/Wasm RGA
frames, receiver application, duplicate/reorder/snapshot recovery, and the
CodeMirror-shaped binding. These are protocol and adapter checks, not
authentication or transport acceptance tests.

## Workloads

Every row is the median of five independent samples. A simulated workload uses
a CodeMirror-shaped text port, 512 local one-rune replacements, then 256
remote replacements. The Go/Wasm workload uses a 12,288-rune document,
256 local replacements, a real Go/Wasm run-v2 source, and immediate receiver
application for every emitted frame. The functional tests above, rather than
the synthetic port, instantiate real CodeMirror 6.

### Local edit latency

| Host | Workload | Native median | Full-projection median | Reduction |
| --- | --- | ---: | ---: | ---: |
| benchmark host A | simulated, 32,768 runes | 0.649 ms/edit | 1.402 ms/edit | 53.7% |
| benchmark host B | simulated, 32,768 runes | 0.653 ms/edit | 1.515 ms/edit | 56.9% |
| benchmark host A | simulated, 262,144 runes | 0.577 ms/edit | 11.318 ms/edit | 94.9% |
| benchmark host B | simulated, 262,144 runes | 4.590 ms/edit | 11.298 ms/edit | 59.4% |
| benchmark host A | Go/Wasm + receiver, 12,288 runes | 0.338 ms/local merge | 2.948 ms/local merge | 88.5% |
| benchmark host B | Go/Wasm + receiver, 12,288 runes | 0.386 ms/local merge | 3.011 ms/local merge | 87.2% |

The 262,144-rune native result on benchmark host B was repeated because it was
slower than the otherwise comparable host: the repeat medians were 4.589
ms/edit native and 11.432 ms/edit fallback. It is therefore a repeatable
host-specific measurement, not a transcription error. This report deliberately
does not average the hosts or claim the fastest result as a universal speedup.

### Raw local samples (ms per edit / local merge)

| Host | Workload | Native samples | Full-projection samples |
| --- | --- | --- | --- |
| benchmark host A | simulated 32K | 0.722, 0.649, 0.646, 0.648, 0.650 | 1.524, 1.467, 1.402, 1.402, 1.402 |
| benchmark host B | simulated 32K | 0.740, 0.644, 0.653, 0.649, 0.660 | 1.515, 1.529, 1.568, 1.466, 1.439 |
| benchmark host A | simulated 262K | 0.639, 0.577, 0.589, 0.471, 0.541 | 11.388, 11.300, 11.318, 11.322, 11.366 |
| benchmark host B | simulated 262K | 4.605, 4.590, 4.535, 4.532, 4.638 | 12.310, 11.436, 11.232, 11.298, 11.182 |
| benchmark host A | Go/Wasm 12K | 0.721, 0.395, 0.338, 0.335, 0.303 | 3.143, 2.981, 2.948, 2.817, 2.896 |
| benchmark host B | Go/Wasm 12K | 0.707, 0.386, 0.330, 0.390, 0.313 | 3.170, 3.102, 3.011, 2.970, 2.778 |

Heap deltas were collected by the harness but are intentionally not compared:
V8 garbage collection makes them diagnostic, not a stable allocation metric.
The Go/Wasm frame totals stayed around 27,0xx bytes for every scenario; they
are not byte-for-byte comparable across separate fresh documents because RGA
tags include runtime-generated identities.

### Remote projection control

| Host | 32K native / fallback | 262K native / fallback |
| --- | ---: | ---: |
| benchmark host A | 0.862 / 0.845 ms/edit | 6.307 / 6.520 ms/edit |
| benchmark host B | 0.860 / 0.847 ms/edit | 6.395 / 6.472 ms/edit |

These near-equivalent remote results are expected: incoming RGA frames do not
contain a trusted display change set, so the adapter intentionally projects the
merged remote text as a whole. The optimization only applies to a valid local
CodeMirror single-range update.

## Commands

```sh
make typescript-test
make wasm-test
make typescript-bindings-benchmark
make wasm-bindings-benchmark

# Larger controlled simulated fixture
CRDT_BINDINGS_INITIAL_RUNES=262144 \
  npm --prefix clients/typescript run bench:bindings
```

For a server run, build the TypeScript and Wasm artifacts once from a pinned
source revision, verify their SHA-256 after transfer, and run the commands in
a disposable directory. Do not install an unpinned runtime globally, include
server transfer time in the benchmark, or represent this controlled result as
a live browser/device measurement.
