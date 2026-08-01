import {
  BroadcastChannelNativeTransport,
  createBrowserReplicaID,
  openNativeBrowserDocument,
} from "../../dist/browser.js";

const documentID = "example-local-first-board-v1";
const channel = new BroadcastChannelNativeTransport(JSON.stringify(["darkinno-crdt", "native-ts-v1", documentID]));
const board = await openNativeBrowserDocument({
  documentID,
  replicaID: createBrowserReplicaID("local-tab"),
  liveTransport: channel,
});
const title = board.getMap("metadata");
const cards = board.getArray("cards");
const titleInput = requiredElement("title");
const cardInput = requiredElement("card");
const cardList = requiredElement("cards");
const outbox = requiredElement("outbox");
const serviceWorker = requiredElement("service-worker");
const error = requiredElement("error");

requiredElement("title-form").addEventListener("submit", (event) => {
  event.preventDefault();
  run(() => title.set("title", requiredText(titleInput.value)));
});
requiredElement("card-form").addEventListener("submit", (event) => {
  event.preventDefault();
  run(() => {
    cards.push([requiredText(cardInput.value)]);
    cardInput.value = "";
  });
});
board.onUpdate(() => render());
board.onError((cause) => {
  error.textContent = cause instanceof Error ? cause.message : String(cause);
});
render();
registerOfflineShell();

window.addEventListener("pagehide", () => {
  void board.flush().catch(() => {});
  board.disconnectLive();
  channel.close();
});

function render() {
  if (document.activeElement !== titleInput) titleInput.value = title.get("title") ?? "";
  outbox.textContent = String(board.pendingOutbox);
  cardList.replaceChildren(...cards.toArray().map((card) => {
    const item = document.createElement("li");
    item.textContent = card;
    return item;
  }));
}

async function registerOfflineShell() {
  if (!("serviceWorker" in navigator)) {
    serviceWorker.textContent = "Service Worker unavailable";
    return;
  }
  try {
    await navigator.serviceWorker.register("./offline-service-worker.mjs", { scope: "./", type: "module" });
    serviceWorker.textContent = "Offline shell registered; it controls after the next navigation.";
  } catch (cause) {
    serviceWorker.textContent = "Offline shell registration failed";
    error.textContent = cause instanceof Error ? cause.message : String(cause);
  }
}

function requiredText(value) {
  if (typeof value !== "string" || value.trim() === "") throw new TypeError("Value must be a non-empty string");
  return value.trim();
}

function run(operation) {
  try {
    error.textContent = "";
    operation();
    void board.flush().then(render, (cause) => {
      error.textContent = cause instanceof Error ? cause.message : String(cause);
      render();
    });
    render();
  } catch (cause) {
    error.textContent = cause instanceof Error ? cause.message : String(cause);
  }
}

function requiredElement(id) {
  const element = document.getElementById(id);
  if (element === null) throw new Error(`missing #${id}`);
  return element;
}
