import assert from "node:assert/strict";
import test from "node:test";

import {
  isSafeOfflineResponse,
  offlineAssetURLs,
  offlineCacheKey,
} from "../examples/multitab-offline/offline-policy.mjs";

const scope = "https://example.test/clients/typescript/examples/multitab-offline/";

test("offline cache policy permits only reviewed same-origin static assets", () => {
  const assets = offlineAssetURLs(scope);
  assert.ok(assets.includes(`${scope}app.mjs`));
  assert.ok(assets.includes("https://example.test/clients/typescript/dist/browser.js"));
  assert.equal(offlineCacheKey(new Request(scope), scope), `${scope}index.html`);
  assert.equal(offlineCacheKey(new Request(`${scope}app.mjs`), scope), `${scope}app.mjs`);
  assert.equal(offlineCacheKey(new Request("https://example.test/api/v1/boards"), scope), undefined);
  assert.equal(offlineCacheKey(new Request("https://cdn.example.test/app.js"), scope), undefined);
  assert.equal(offlineCacheKey(new Request(`${scope}app.mjs`, { method: "POST" }), scope), undefined);
});

test("offline cache policy rejects opaque and unsuccessful responses", () => {
  assert.equal(isSafeOfflineResponse({ ok: true, type: "basic" }), true);
  assert.equal(isSafeOfflineResponse({ ok: true, type: "default" }), true);
  assert.equal(isSafeOfflineResponse({ ok: true, type: "opaque" }), false);
  assert.equal(isSafeOfflineResponse({ ok: false, type: "basic" }), false);
});
