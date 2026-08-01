import assert from "node:assert/strict";

import WebSocket from "ws";
import { WebsocketProvider } from "y-websocket";
import * as Y from "yjs";

const endpoint = process.argv[2];
if (typeof endpoint !== "string" || !endpoint.startsWith("ws://")) {
  throw new Error("relay integration requires one ws:// endpoint");
}

// Browser WebSockets send same-origin HttpOnly cookies automatically. Node's
// ws polyfill needs an explicit header solely to exercise that browser-safe
// authentication shape in this end-to-end test.
class SessionWebSocket extends WebSocket {
  constructor(address, protocols) {
    super(address, protocols, { headers: { cookie: "yjs_session=native-yjs-editor" } });
  }
}

function createEditor() {
  const document = new Y.Doc();
  const provider = new WebsocketProvider(endpoint, "notes", document, {
    connect: false,
    disableBc: true,
    WebSocketPolyfill: SessionWebSocket,
  });
  return { document, provider };
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

function hasText(document, fragments) {
  const value = document.getText("shared").toString();
  return fragments.every((fragment) => value.includes(fragment));
}

async function main() {
  const alice = createEditor();
  const bob = createEditor();
  const carol = createEditor();
  try {
  // Each editor starts from an empty local state and writes before connecting.
  // This exercises a realistic offline/reconnect merge, not a happy-path
  // sequential edit or a same-process BroadcastChannel shortcut.
  alice.document.transact(() => {
    alice.document.getText("shared").insert(0, "alpha ");
    alice.document.getMap("metadata").set("source", "alice");
  });
  bob.document.transact(() => {
    bob.document.getText("shared").insert(0, "beta ");
    bob.document.getArray("items").insert(0, ["offline-card"]);
  });
  alice.provider.connect();
  bob.provider.connect();

  await waitFor(() => alice.provider.synced && bob.provider.synced, "initial Yjs sync");
  await waitFor(
    () => hasText(alice.document, ["alpha", "beta"]) && hasText(bob.document, ["alpha", "beta"]),
    "offline text convergence",
  );
  await waitFor(
    () => alice.document.getArray("items").get(0) === "offline-card" && bob.document.getMap("metadata").get("source") === "alice",
    "nested shared-type convergence",
  );

  alice.provider.awareness.setLocalStateField("user", { name: "Alice" });
  await waitFor(
    () => [...bob.provider.awareness.getStates().values()].some((state) => state?.user?.name === "Alice"),
    "ephemeral awareness propagation",
  );

  // Destroy the live editors before a fresh client connects. Carol has no
  // BroadcastChannel and no carried update, so this must use the Go relay's
  // state-vector handshake and the Node store's durable snapshot.
  alice.provider.destroy();
  bob.provider.destroy();
  carol.provider.connect();
  await waitFor(() => carol.provider.synced, "durable reconnect sync");
  await waitFor(
    () => hasText(carol.document, ["alpha", "beta"]) && carol.document.getMap("metadata").get("source") === "alice" && carol.document.getArray("items").get(0) === "offline-card",
    "durable recovered document",
  );
  assert.equal(carol.provider.wsconnected, true);
  } finally {
    alice.provider.destroy();
    bob.provider.destroy();
    carol.provider.destroy();
  }
}

try {
  await main();
  // y-websocket may retain a reconnect timer after a provider is destroyed in
  // Node. This standalone test has finished every resource assertion, so an
  // explicit exit prevents that implementation detail from stalling Go CI.
  process.exit(0);
} catch (error) {
  process.stderr.write(`${error instanceof Error ? error.stack : error}\n`);
  process.exit(1);
}
