# Local-first multi-tab and offline example

After building the package, serve the repository root over HTTP(S), then open
`/clients/typescript/examples/multitab-offline/` in two tabs:

```sh
npm --prefix clients/typescript run build
npx --yes serve .
```

The sample uses a fresh replica ID per tab, IndexedDB recovery, and
`BroadcastChannelNativeTransport` through `liveTransport`. The live path only
delivers an already-persisted update to another tab. It cannot authenticate a
peer, replay history, bootstrap a late tab, or acknowledge the persistent
outbox; the example deliberately displays the pending count until a product
adds an authenticated server transport whose `send()` resolves at the chosen
durable receipt boundary.

The module Service Worker precaches only this public static shell and the
compiled client modules. It has no `skipWaiting`/`clients.claim`, does not
cache APIs, arbitrary routes, opaque responses, credentials, or CRDT data, and
does not replace IndexedDB/outbox recovery. Version cache changes only after
the old worker is no longer controlling a tab.
