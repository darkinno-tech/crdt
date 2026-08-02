# Tiptap and ProseMirror rich-text binding

`bindTiptapRichText` binds a fixed, manifest-bound Tiptap 3 profile to the stable Go/Wasm `richtext` v1 protocol. `bindProseMirrorRichText` accepts a small application-owned port for the same profile. Both use state/delta TypeIDs `23/24`, semantic version `1`, and the exact renderer Schema ID `darkinno:tiptap-core-richtext-v1`.

This is a richer surface than `bindTiptapPlainText`, but it still intentionally rejects arbitrary editor JSON. Read the [profile decision](../design/tiptap-richtext-profile.md) before enabling it.

## Tiptap setup

Configure a Tiptap schema containing only the approved blocks/marks plus each atomic embed node used below. An embed extension must be an inline atom; its attributes are application data, not HTML or authorization claims.

```ts
import {
  bindTiptapRichText,
  initRichTextWasm,
  TIPTAP_CORE_RICH_TEXT_SCHEMA_ID,
} from "@darkinno/crdt-client";

// Authenticate a rich-text Manifest with SchemaID
// TIPTAP_CORE_RICH_TEXT_SCHEMA_ID before this construction.
const runtime = await initRichTextWasm({ wasmURL: "/assets/crdt-rga.wasm" });
const document = runtime.create("browser-replica-7");

const mentionCodec = {
  kind: "mention",
  nodeType: "mention", // an inline atom registered in this Tiptap schema
  encode(node) {
    if (node.type !== "mention" || typeof node.attrs?.id !== "string" ||
      typeof node.attrs?.label !== "string") throw new Error("invalid mention");
    return { id: node.attrs.id, label: node.attrs.label };
  },
  decode(payload) {
    if (typeof payload.id !== "string" || typeof payload.label !== "string") {
      throw new Error("invalid mention payload");
    }
    return { type: "mention", attrs: { id: payload.id, label: payload.label } };
  },
};

const binding = bindTiptapRichText(document, editor, {
  initialContent: "document", // normal join/recovery path
  embeds: [mentionCodec],
  onLocalFrame(frame) {
    // Append to a durable outbox before transport publication.
    outbox.append(frame);
  },
});

socket.onmessage = ({ data }) => binding.applyRemote(new Uint8Array(data));
```

Use `initialContent: "editor"` only once when seeding an empty rich-text group from a validated local editor. A joining client must restore the state/frontier/HLC snapshot first and then use `"document"`; adding the local editor's initial paragraph is otherwise a concurrent edit.

## ProseMirror port

Keep the ProseMirror dependency and transaction policy in the application:

```ts
const binding = bindProseMirrorRichText(document, {
  readJSON: () => view.state.doc.toJSON(),
  replaceJSON(json) {
    // Create and dispatch a new transaction tagged as a CRDT remote write.
    // The exact Node.fromJSON call is owned by this schema-aware host.
    return replaceStateFromApprovedJSON(view, json, { crdtRemote: true });
  },
  observeUpdate(listener) {
    return observeLocalTransactions(view, (transaction) => {
      if (transaction.getMeta("crdtRemote") !== true) listener();
    });
  },
}, {
  initialContent: "document",
  onLocalFrame: outbox.append,
});
```

Do not alter an already dispatched ProseMirror transaction. Validate its schema, apply local policy, and make the tagged remote replacement in a new transaction. The port must keep remote replacements out of `observeUpdate`, otherwise it creates a transport echo and pollutes local undo history.

## Limits and failure behavior

Before local CRDT mutation, the binding caps the profile at 4,096 blocks, 16,384 inline nodes, 64 KiB text, 16,384 Unicode scalar values, 64 KiB of canonical embed payloads in total, depth-32 embed JSON, and 512 editor operations. Options may lower these values, never raise them. The Go/Wasm runtime independently enforces its accepted frame, retained-node, mark, and operation limits.

For a local update, unknown nodes, marks, attrs, malformed codec output, source overages, or a rejected Go/Wasm transaction restore the last replicated editor projection before a frame is emitted. They never leave a local-only editor fork or a delete-only CRDT state.

The rich-text core deliberately accepts generic attributes that the Tiptap Profile does not. Therefore, an application must authenticate and Schema-ID/Manifest-validate a received frame before calling `applyRemote`. If a profile violation nevertheless reaches the binding (for example an unknown embed kind or non-canonical received embed JSON), the binding restores the last safe editor projection, stops observing and freezes. The already-merged CRDT state is not rolled back, because doing so can erase a concurrent valid merge. Drop that replica projection and recover it from an authenticated checkpoint after fixing the admission boundary.

## Out of scope

Tables, lists, media, arbitrary links, block embeds, custom marks/attrs, NodeViews, editor IDs, selection/presence, shared undo, persistence, replay, TLS, authentication, authorization, rate limiting, resource fetches, and rendering sanitisation are application responsibilities. Model structured objects in a separately negotiated `documenttree` group; do not place their raw JSON, HTML, CSS, or URLs in `rt.block`.

## Verification and controlled benchmark

```sh
make typescript-test
make wasm-test
make typescript-tiptap-richtext-benchmark
go test -race ./richtext ./internal/wasm
```

The benchmark exercises an actual Tiptap Core schema with 64 paragraphs, marked text, and atomic mentions. It asserts one frame for each local edit, remote no-echo, and final document convergence. It is not a browser paint, network, storage, or production capacity measurement.
