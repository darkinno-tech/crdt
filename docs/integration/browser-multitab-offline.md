# Browser multi-tab delivery and offline shell

The runnable [`multitab-offline`](../../clients/typescript/examples/multitab-offline/)
example combines the persistent browser facade with a same-origin live path:

```ts
const live = new BroadcastChannelNativeTransport(channelName);
const document = await openNativeBrowserDocument({
  documentID: authenticatedDocumentID,
  replicaID: createBrowserReplicaID(),
  liveTransport: live,
});

// Later, after handshake/authorization, attach the durable receipt adapter.
await document.connect(authenticatedReceiptTransport);
```

Each active tab must have a distinct replica ID. The selected channel name must
be scoped to exactly one app protocol and document; it is a routing label, not
an authorization decision.

## Delivery boundary

```text
local CRDT mutation
  -> IndexedDB append + metadata transaction
  -> durable local outbox entry
  -> optional BroadcastChannel live publication
  -> authenticated server transport send
  -> product-defined durable receipt
  -> outbox acknowledgement and eligible compaction
```

BroadcastChannel publication runs only after the local persistence append
completes. It is deliberately represented by `NativeBrowserLiveTransport`, not
`NativeBrowserTransport`, so a `postMessage()` can never remove an outbox entry.
The same boundary applies to the Go/Wasm RGA browser facade. A receiver still
uses the bounded canonical decoder (or manifest-checked RGA frame decoder)
before a message changes local state.

BroadcastChannel is volatile and same-origin only. It has no authentication,
authorization, history, late-join bootstrap, anti-entropy, receipt, or
partition repair. Keep it as a UI-latency hint beside an authenticated durable
relay; it must not carry secrets or cause a product to claim a write is saved
remotely.

## Offline Service Worker scope

The example's module worker precaches only a reviewed static shell: its HTML,
JavaScript, worker source, and compiled local client modules. It handles no
API route, arbitrary path, cross-origin resource, opaque response, credential,
or CRDT update. It has no `skipWaiting` or `clients.claim`, so an existing tab
continues with its compatible asset set until navigation. IndexedDB plus the
durable outbox remain the recovery mechanism; Cache Storage is only an offline
application shell.

Before production, bind worker cache versioning to the deployed client/schema
compatibility policy, request persistent storage where policy requires it, and
test quota eviction, background termination, an offline restart, relay replay,
and authorization failures on each supported browser.

## Verification

- Native browser unit test: persistence-before-live publish, no live-path
  acknowledgement, recovery/outbox failure handling, and three-editor
  shuffled/duplicate convergence.
- Wasm RGA integration test: the same live-path/no-receipt property on actual
  Go/Wasm RGA frames.
- Offline policy test: only reviewed same-origin static URLs are cacheable;
  opaque, failed, cross-origin, API, and non-GET requests are rejected.

Run `make typescript-test` for the native and policy coverage, then `make wasm`
followed by `make wasm-test` for the real RGA integration. The browser harness
is a controlled local Chromium check; it is not evidence of server receipts,
mobile quota behavior, or crash durability.
