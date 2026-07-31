# Rich-text editor binding controlled validation — 2026-07-31

## Scope

This record covers the rich-text v1 editor transaction added for Quill Delta
binding. It is a controlled local baseline, not a browser, WAN, service, or
device-capacity SLA.

The transaction benchmark intentionally includes the safety work that makes a
replacement atomic: an isolated RGA simulation, text-delta merge, mark-plan
validation, canonical-frame preflight, then the real mutation. Do not compare
it to a plain append that does not provide those failure guarantees.

## Environment

- Date: 2026-07-31
- Host: Apple M4 Pro, macOS/Darwin arm64
- Go package benchmark, `-benchtime=1s -count=3 -benchmem`
- Input: a formatted review transaction over a locally generated rich-text
  document, including text replacement and approved marks

## Results

| Path | Run 1 | Run 2 | Run 3 | Allocation range |
| --- | ---: | ---: | ---: | ---: |
| `richtext.Document.ApplyEditorDelta` | 12.18 ms/op | 12.03 ms/op | 12.09 ms/op | 17.52 MiB/op, 40,738–40,747 allocs/op |
| `internal/wasm.RichTextRuntime.ApplyEditorDelta` | 10.41 ms/op | 10.36 ms/op | 10.87 ms/op | 17.02–17.03 MiB/op, 33,584–33,596 allocs/op |

The high allocation cost is expected for this first safety-oriented version:
each local editor transaction clones and preflights the complete private RGA
state before mutation. It is a correctness boundary, not a queueing strategy.
For sustained large-document editing, measure document-size growth and edit
shape first; optimize only if the same atomic validation is retained and a
representative regression benchmark improves.

## Correctness and integration evidence

```sh
go test ./richtext ./internal/wasm -count=1
go test -race ./richtext ./internal/wasm -count=1
make wasm-test
go test ./extensions -run 'YJS|Yjs|Yjs' -count=1
go test -run '^$' \
  -bench='Benchmark(RichTextApplyEditorDeltaReviewTransaction|RichTextRuntimeApplyEditorDelta)$' \
  -benchmem -count=3 -benchtime=1s ./richtext ./internal/wasm
```

The deterministic simulated coverage has three replicas, duplicate and
reordered delivery, rejected replacement atomicity, resource-bound rejection,
and snapshot restore. The integration path runs the real Go/Wasm rich-text
runtime from TypeScript and checks a Quill-shaped Delta port for approved
inline marks, newline-owned block marks, remote no-echo, and recovery. This
does not replace a production browser test against a pinned Quill package,
network transport, durable outbox, or a real Yjs document engine.
