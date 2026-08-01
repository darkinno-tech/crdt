import {
  BroadcastChannelNativeTransport,
  createBrowserReplicaID,
  openNativeBrowserDocument,
} from "../src/browser.js";

const live = new BroadcastChannelNativeTransport("typecheck-browser-live");

void openNativeBrowserDocument({
  documentID: "typecheck-document",
  replicaID: createBrowserReplicaID(),
  liveTransport: live,
});

void openNativeBrowserDocument({
  documentID: "typecheck-document",
  replicaID: createBrowserReplicaID(),
  // @ts-expect-error BroadcastChannel never satisfies the durable receipt boundary.
  transport: live,
});

live.close();
