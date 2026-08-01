import { NativeCollectionsDocument } from "../../dist/collections.js";
import { encodeNativeUpdate } from "../../dist/native.js";

/**
 * A small structured-editor model. It deliberately uses a different CRDT for
 * each editing concern instead of treating a rich document or HTML as one
 * mutable value: title is LWW, labels are add-wins, outline parents are
 * immutable OR-Tree links, and edits are a PN-Counter.
 */
export class EditorialCollectionsModel {
  #document;
  #title;
  #labels;
  #outline;
  #revisions;

  constructor(replicaID, document = new NativeCollectionsDocument(replicaID)) {
    this.#document = document;
    this.#title = document.getLWWRegister("title");
    this.#labels = document.getORSet("labels");
    this.#outline = document.getORTree("outline");
    this.#revisions = document.getCounter("revisions");
  }

  setTitle(value) {
    const title = requiredText(value, "title");
    this.#document.transact(() => {
      this.#title.set(title);
      this.#revisions.increment();
    }, "editor-title");
  }

  addLabel(value) {
    const label = requiredText(value, "label");
    this.#document.transact(() => {
      this.#labels.add(label);
      this.#revisions.increment();
    }, "editor-label");
  }

  removeLabel(value) {
    const label = requiredText(value, "label");
    let removed = false;
    this.#document.transact(() => {
      removed = this.#labels.remove(label);
      if (removed) this.#revisions.increment();
    }, "editor-label");
    return removed;
  }

  addSection(value) {
    const title = requiredText(value, "section title");
    let id;
    this.#document.transact(() => {
      id = this.#outline.add(null, { kind: "section", title });
      this.#revisions.increment();
    }, "editor-outline");
    return id;
  }

  addParagraph(parent, value) {
    const text = requiredText(value, "paragraph text");
    let id;
    this.#document.transact(() => {
      id = this.#outline.add(parent, { kind: "paragraph", text });
      this.#revisions.increment();
    }, "editor-outline");
    return id;
  }

  /** Hand the resulting bytes only to an authenticated, contract-bound adapter. */
  onEncodedUpdate(listener) {
    return this.#document.onUpdate((event) => listener(encodeNativeUpdate(event.update), event.local));
  }

  applyEncodedUpdate(encoded, origin = "authenticated-peer") {
    return this.#document.applyEncodedUpdate(encoded, origin);
  }

  observe(listener) {
    const stops = [this.#title.observe(listener), this.#labels.observe(listener), this.#outline.observe(listener), this.#revisions.observe(listener)];
    return () => stops.forEach((stop) => stop());
  }

  state() {
    return Object.freeze({
      title: this.#title.get() ?? "",
      labels: this.#labels.values(),
      outline: this.#outline.roots(),
      revisions: this.#revisions.value(),
    });
  }
}

function requiredText(value, field) {
  if (typeof value !== "string" || value.trim() === "") {
    throw new TypeError(`${field} must be a non-empty string`);
  }
  return value.trim();
}
