# Monaco incremental RGA binding — controlled benchmark, 2026-08-03

## Decision

Enable the local incremental path only for a Monaco content event that has one
validated former UTF-16 range, one string replacement, a valid post-edit
`getValueLength()`, and neither `isFlush` nor `isEolChange`. Keep batches,
model resets, EOL conversions, legacy ports, malformed events, and inconsistent
lengths on the existing one-frame full-projection path.

This improves local typing/editing work without changing RGA frames, manifest
negotiation, remote merge behavior, or the resource limits enforced before a
local replacement enters Go/Wasm.

## Evidence and boundaries

| Dimension | Evidence | Scope boundary |
|---|---|---|
| Monaco API contract | `IModelContentChange` supplies the old range offset/length and replacement text; `IModelContentChangedEvent` batches changes and identifies flush/EOL changes. | A structural port avoids taking Monaco as a production dependency. |
| Correctness | Unit cases cover UTF-16/rune conversion, chunks, batches, flushes, EOL changes, old ports, resource rejection recovery, and remote no-echo. | The full fallback is intentionally one whole-document replacement, not a multi-frame interpretation of a batch. |
| Protocol interoperability | `make wasm-test` includes a Monaco-shaped local edit through the real Go/Wasm RGA and a remote RGA merge back into the model. | It is a model-shaped test; it is not an interactive browser/device run of Monaco. |
| Security and resources | Event fields are checked before an internal replacement is constructed; the existing byte/rune preflight restores rejected local text. | Authentication, manifest admission, outbox delivery, presence, and host telemetry remain application-owned. |
| Performance | Five controlled samples compare native and forced full-projection paths with the same text and edit schedule. | Node heap deltas are diagnostics only; neither result is a browser, WAN, TLS, storage, or service-capacity SLA. |

## Controlled simulated measurement

Command:

```sh
make typescript-bindings-benchmark
```

Runtime: Node `v26.5.0`; 32,768 ASCII initial Unicode scalars; 512 local
single-character replacements; 256 remote replacements; five warm samples per
mode. The Monaco simulation exposes the same `getValue`, `getValueLength`,
`setValue`, and `onDidChangeContent` contract used by the binding. The fallback
case hides `getValueLength`, which is the conservative behavior for a legacy
or incomplete structural port.

| Surface | Local path | Sample range, ms/edit | Median, ms/edit | Relative median |
|---|---:|---:|---:|---:|
| Monaco | native incremental | 0.221–0.226 | **0.222** | 1.00× |
| Monaco | full projection fallback | 0.481–0.484 | **0.483** | 2.18× |

The controlled local native path was about **54% lower median adapter time per
edit** than the fallback. Both modes emitted 512 local RGA frames and ended
with the same editor and document projection. Remote work remained roughly
0.304–0.310 ms/edit in this harness because a remote RGA frame does not carry a
trusted Monaco display-change set and is intentionally rendered through the
full projection.

## Reproduction and acceptance

```sh
make typescript-test
make wasm-test
make typescript-bindings-benchmark
```

The first command exercises actual CodeMirror, Tiptap, Lexical, BlockNote and
the simulated adapter boundaries. The second loads the real negotiated Go/Wasm
artifact and tests frame interoperability. Run the target application's own
browser/device trace with its Monaco version, extensions, worker topology,
document sizes, and transport before treating these local measurements as a
release capacity result.
