# Tiptap rich-text profile controlled validation — 2026-08-01

## Scope

This records a local regression baseline for
`darkinno-tech:tiptap-core-richtext-v1`: approved rich-text blocks and marks plus
codec-validated atomic inline embeds. It does not measure browser layout,
NodeView rendering, mobile devices, WAN/TLS, durable outboxes, database I/O,
or service capacity.

## Environment

| Field | Value |
| --- | --- |
| Host | Darwin 26.5.2, arm64 (Apple M4 Pro) |
| Go | go1.26.5 darwin/arm64 |
| Node | v26.5.0 |
| Tiptap Core | 3.29.2 from the locked development test dependency |
| Profile budget | 4,096 blocks, 16,384 inline nodes, 64 KiB text and aggregate embed JSON, 16,384 runes, 512 operations |

## Actual editor API + simulated protocol workload

`make typescript-tiptap-richtext-benchmark` creates an actual Tiptap Core
schema with 64 paragraphs, alternating bold text, and eight atomic mention
nodes (8,646 final visible runes). It imports once, then applies 128 local
single-paragraph edits and 64 remote frames through an in-memory rich-text
document. Every sample asserts exactly 129 emitted local frames (including the
initial import), no remote echo, and equal final editor/CRDT text.

```sh
for run in 1 2 3 4 5; do
  node clients/typescript/bench/tiptap-richtext.bench.mjs
done
```

| Sample | Local ms/edit | Remote ms/merge |
| --- | ---: | ---: |
| 1 | 4.789 | 4.235 |
| 2 | 4.779 | 4.207 |
| 3 | 4.727 | 4.135 |
| 4 | 4.826 | 4.182 |
| 5 | 4.816 | 4.307 |
| Median | **4.789** | **4.207** |

The profile reads schema JSON and builds a bounded run projection for each
callback. These numbers therefore describe adapter work under a real editor
API but an in-memory CRDT simulation; they are useful for regression detection,
not UI-frame or network SLOs.

## Actual Go rich-text core

The Go benchmark exercises the existing atomic editor-transaction path used by
the Go/Wasm runtime:

```sh
go test -run '^$' \
  -bench '^BenchmarkRichTextApplyEditorDeltaReviewTransaction$' \
  -benchmem -count=3 ./richtext
```

| Sample | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| 1 | 12,809,276 | 18,376,174 | 40,749 |
| 2 | 12,924,291 | 18,412,904 | 40,742 |
| 3 | 12,683,854 | 18,359,033 | 40,742 |
| Median | **12,809,276** | **18,376,174** | **40,742** |

The core benchmark validates atomic preflight on a realistic review
transaction. It does not include a browser DOM or transport. The TypeScript
Wasm integration separately proves that a real Tiptap profile frame has TypeID
`24`, reaches a second actual Go/Wasm document, does not echo remotely, and
survives snapshot restoration.

## Interpretation

The new binding adds no TypeID, protocol decoder, or unbounded data path. Its
full-projection cost is intentional for this bounded rich-document profile.
For sustained high-frequency source editing, prefer the existing incremental
CodeMirror plain-text binding or create a new profile only after defining an
equally strict schema, operation, and remote-transaction contract.
