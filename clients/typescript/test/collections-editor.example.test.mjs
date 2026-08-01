import assert from "node:assert/strict";
import test from "node:test";

import { EditorialCollectionsModel } from "../examples/collections-editor/editor-model.mjs";

test("structured editor example converges after reversed duplicate updates", () => {
  const alice = new EditorialCollectionsModel("alice");
  const bob = new EditorialCollectionsModel("bob");
  const updates = [];
  alice.onEncodedUpdate((encoded, local) => {
    if (local) updates.push(encoded);
  });

  alice.setTitle("Release notes");
  alice.addLabel("reviewed");
  const section = alice.addSection("Summary");
  alice.addParagraph(section, "Bounded state is required.");

  for (const encoded of [...updates].reverse()) {
    bob.applyEncodedUpdate(encoded);
    bob.applyEncodedUpdate(encoded);
  }

  assert.deepEqual(bob.state(), alice.state());
  assert.equal(bob.state().revisions, 4n);
  assert.deepEqual(bob.state().outline.map((node) => node.value), [{ kind: "section", title: "Summary" }]);
});
