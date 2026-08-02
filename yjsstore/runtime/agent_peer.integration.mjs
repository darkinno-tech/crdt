import assert from "node:assert/strict";

import WebSocket from "ws";
import { WebsocketProvider } from "y-websocket";
import * as Y from "yjs";

const endpoint = process.argv[2];
if (typeof endpoint !== "string" || !endpoint.startsWith("ws://")) {
  throw new Error("agent-peer integration requires one ws:// endpoint");
}

// Browser clients would send this same-origin, HttpOnly session cookie without
// giving the agent or a document update a bearer credential. Node only adds it
// to model that browser connection shape in this end-to-end check.
class SessionWebSocket extends WebSocket {
  constructor(address, protocols) {
    super(address, protocols, { headers: { cookie: "yjs_session=native-yjs-editor" } });
  }
}

async function waitFor(predicate, description) {
  const deadline = Date.now() + 12_000;
  while (!predicate()) {
    if (Date.now() >= deadline) {
      throw new Error(`timed out waiting for ${description}`);
    }
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}

const document = new Y.Doc();
const provider = new WebsocketProvider(endpoint, "notes", document, {
  connect: false,
  disableBc: true,
  WebSocketPolyfill: SessionWebSocket,
});

try {
  provider.connect();
  await waitFor(() => provider.synced, "durable Yjs sync after agent publish");
  await waitFor(() => document.getText("shared").toString() === "hello", "agent text recovery");
  assert.equal(provider.wsconnected, true);
} finally {
  provider.destroy();
  document.destroy();
}
