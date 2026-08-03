# TypeScript Collections structured-editor integration

`native-ts-collections-v1` is suitable for the structured state around an
editor, not as a shortcut rich-text protocol. The runnable
[`collections-editor`](../../clients/typescript/examples/collections-editor/)
example deliberately chooses one collection for each editing concern:

| Editor concern | Collection | Reason |
| --- | --- | --- |
| Document title | `NativeLWWRegister<string>` | One current scalar with deterministic concurrent winner. |
| Labels | `NativeORSet<string>` | Concurrent add/remove needs retained per-add tombstones. |
| Outline sections | `NativeORTree` | Parent links are immutable; a move is remove plus a new node. |
| Revision events | `NativeCounter` | Each actor advances only its own monotone component. |

This is intentionally different from a document body. Do not store HTML,
editor JSON, marks, embeds, selections, cursors, or arbitrary block trees in a
register, set, or tree value. Plain body text uses the negotiated Go/Wasm RGA;
approved formatted text uses the separately manifest-bound `richtext-v1`
runtime. An editor bridge must not flatten a rich editor tree without an
explicit schema contract.

## Host boundary

`NativeCollectionsDocument#onUpdate` gives the host a canonical native update.
Encode it only after authenticating the peer and binding the exact document,
`native-ts-collections-v1` semantic version, declared roots, and compatible
limits. A raw `NativeDocument` must never receive those updates: that bypasses
the collection validator for monotone counter components, OR-Set tombstones,
and immutable tree parents.

Persist the collection snapshot (logical root declarations, native state, and
actor counter) atomically with the authenticated outbox and delivery frontier.
A local event or a browser `send()` call is not a durable server receipt.

## Verification

The runnable model is covered by a reverse-delivery and duplicate-delivery
convergence test. Run it with the standard TypeScript suite:

```sh
make typescript-test
```

The UI is an integration reference, not a production editor shell. It uses DOM
`textContent`/node construction rather than inserting user input as HTML, and
leaves authentication, transport, storage, access control, undo, and rich-text
schema selection to the host.

The TypeScript suite also projects a reordered, duplicated `NativeORTree`
outline into an actual Tiptap Core schema. Its host projection calls
`setContent(..., { emitUpdate: false })`, so a remote structural update cannot
re-enter an editor-local outbox. This validates the
`native-ts-collections-v1` editor boundary only: it is not a claim that this
native collection wire format interoperates with the separate Go OR-Tree v1
frames. Keep a Go OR-Tree group and a native collection group under distinct
manifests and transports.
