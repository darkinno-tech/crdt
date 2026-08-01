# Native Collections structured-editor example

Run from the repository root after `npm --prefix clients/typescript run build`:

```sh
npx --yes serve .
```

Then open `/clients/typescript/examples/collections-editor/`.

The example intentionally maps each structured editing concern to one CRDT:

- title: `NativeLWWRegister`;
- labels: add-wins `NativeORSet`;
- sections: immutable-parent `NativeORTree`;
- revision events: `NativeCounter`.

It does not model a rich-text body. Do not serialize HTML, editor JSON, cursor
state, or an arbitrary block tree through these values. Use the manifest-bound
Wasm RGA/rich-text API for that data, and send `onEncodedUpdate` output only
through an authenticated, document-bound adapter with a durable receipt.
