export const OFFLINE_CACHE_NAME = "darkinno-crdt-local-first-v1";

const STATIC_ASSET_PATHS = Object.freeze([
  "./index.html",
  "./app.mjs",
  "./offline-policy.mjs",
  "./offline-service-worker.mjs",
  "../../dist/browser.js",
  "../../dist/native.js",
]);

export function offlineAssetURLs(scopeURL) {
  return STATIC_ASSET_PATHS.map((path) => new URL(path, scopeURL).href);
}

/**
 * Return the cache key only for reviewed same-origin static assets. API,
 * authenticated, cross-origin, and arbitrary user-provided URLs are never
 * cached by this demo's Service Worker.
 */
export function offlineCacheKey(request, scopeURL) {
  if (request.method !== "GET") return undefined;
  const scope = new URL(scopeURL);
  const url = new URL(request.url);
  if (url.origin !== scope.origin) return undefined;
  if (url.pathname === scope.pathname) return new URL("./index.html", scope).href;
  return offlineAssetURLs(scope).includes(url.href) ? url.href : undefined;
}

export function isSafeOfflineResponse(response) {
  return response.ok && (response.type === "basic" || response.type === "default");
}
