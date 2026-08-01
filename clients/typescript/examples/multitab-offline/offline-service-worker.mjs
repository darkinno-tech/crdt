import {
  OFFLINE_CACHE_NAME,
  isSafeOfflineResponse,
  offlineAssetURLs,
  offlineCacheKey,
} from "./offline-policy.mjs";

const scopeURL = self.registration.scope;

self.addEventListener("install", (event) => {
  event.waitUntil((async () => {
    const cache = await caches.open(OFFLINE_CACHE_NAME);
    await cache.addAll(offlineAssetURLs(scopeURL));
  })());
});

self.addEventListener("activate", (event) => {
  event.waitUntil((async () => {
    const names = await caches.keys();
    await Promise.all(names
      .filter((name) => name.startsWith("darkinno-crdt-local-first-") && name !== OFFLINE_CACHE_NAME)
      .map((name) => caches.delete(name)));
  })());
});

self.addEventListener("fetch", (event) => {
  const cacheKey = offlineCacheKey(event.request, scopeURL);
  if (cacheKey === undefined) return;
  event.respondWith(serveStaticAsset(event.request, cacheKey));
});

async function serveStaticAsset(request, cacheKey) {
  const cache = await caches.open(OFFLINE_CACHE_NAME);
  const cached = await cache.match(cacheKey);
  if (cached !== undefined) return cached;
  try {
    const response = await fetch(request);
    if (isSafeOfflineResponse(response)) {
      await cache.put(cacheKey, response.clone());
    }
    return response;
  } catch {
    return new Response("Offline assets are unavailable", {
      status: 503,
      headers: { "content-type": "text/plain; charset=utf-8" },
    });
  }
}
