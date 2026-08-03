import WebSocket from "ws";
import { WebsocketProvider } from "y-websocket";
import * as Y from "yjs";

const endpoint = process.argv[2];
if (typeof endpoint !== "string" || !endpoint.startsWith("ws://")) {
  throw new Error("revocation integration requires one ws:// endpoint");
}

// Node needs an explicit header to model the same Secure/HttpOnly cookie
// authentication shape that a browser sends automatically to its own origin.
class SessionWebSocket extends WebSocket {
  constructor(address, protocols) {
    super(address, protocols, { headers: { cookie: "yjs_session=revocation-test" } });
  }
}

async function waitForRevocation(provider) {
  await new Promise((resolve, reject) => {
    const deadline = setTimeout(() => reject(new Error("timed out waiting for live subscription revocation")), 12_000);
    let connected = false;
    provider.on("status", (event) => {
      if (event.status === "connected") {
        connected = true;
        return;
      }
      if (connected && event.status === "disconnected") {
        clearTimeout(deadline);
        provider.disconnect();
        resolve();
      }
    });
    provider.connect();
  });
}

const document = new Y.Doc();
const provider = new WebsocketProvider(endpoint, "notes", document, {
  connect: false,
  disableBc: true,
  WebSocketPolyfill: SessionWebSocket,
});

let failure;
try {
  await waitForRevocation(provider);
} catch (error) {
  failure = error;
} finally {
  provider.destroy();
  document.destroy();
}
if (failure !== undefined) {
  process.stderr.write(`${failure instanceof Error ? failure.stack : failure}\n`);
  process.exit(1);
}
process.exit(0);
