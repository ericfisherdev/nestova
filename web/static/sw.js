// Nestova service worker (NES-152).
//
// Nestova is server-rendered HTMX over per-household data, so this worker is
// deliberately conservative: serving a stale HTML document or fragment from
// cache would show one household's data to another member, or resurrect a
// chore someone already completed. Nothing that can contain household data
// is EVER cached. The worker exists for two things only: PWA installability,
// and a branded offline page instead of the browser's error screen.
//
// Three fetch branches, in order:
//   1. navigations      -> network-only, falling back to the offline page
//   2. HTML / HX-Request -> network-only, never cached, no fallback
//   3. same-origin /static/* -> cache-first + background revalidation
// Anything else (cross-origin, non-GET) is left to the browser untouched.
//
// CACHE VERSION BUMP PROCEDURE
// ---------------------------------------------------------------------
// Static asset URLs are not content-hashed, so a deploy reuses the same
// paths. Bump CACHE_NAME's version suffix whenever any pre-cached asset
// changes (CSS rebuild, font swap, icon or manifest edit) — that is what
// makes clients fetch the new bytes; activate() then deletes every older
// cache. Forgetting the bump leaves clients on old CSS indefinitely.
// See docs/pwa.md.
const CACHE_NAME = 'nestova-static-v1';

// Pre-cached on install. Only immutable, household-data-free assets, plus
// the offline page itself — that one is the reason install must succeed
// before the worker is useful.
const PRECACHE_URLS = [
  '/offline',
  '/static/favicon.svg',
  '/static/css/app.css',
  '/static/fonts/hanken-grotesk.woff2',
  '/static/fonts/space-mono.woff2',
  '/static/manifest.webmanifest',
  '/static/icons/icon-192.png',
  '/static/icons/icon-512.png',
  '/static/icons/icon-192-maskable.png',
  '/static/icons/icon-512-maskable.png',
  '/static/icons/icon-180.png',
];

// reloadRequest builds a request that bypasses the browser's own HTTP cache.
// This is load-bearing for the version-bump procedure: /static/ is served
// with max-age=3600, so a plain fetch here could be satisfied from the HTTP
// cache and prewarm a freshly bumped CACHE_NAME with the very bytes the bump
// was meant to replace. 'reload' forces the request to the network.
function reloadRequest(url) {
  return new Request(url, { cache: 'reload' });
}

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches
      .open(CACHE_NAME)
      .then((cache) => cache.addAll(PRECACHE_URLS.map(reloadRequest)))
      // Take over as soon as the pre-cache is warm rather than waiting for
      // every existing tab to close; activate() below claims those tabs.
      .then(() => self.skipWaiting())
      .catch((err) => {
        // A failed pre-cache aborts the install (correctly — the offline
        // page would be missing), but without this the only evidence is
        // buried in DevTools. Log it, then re-throw so install still fails.
        console.error('[sw] precache failed, install aborted:', err);
        throw err;
      }),
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(
          keys.filter((key) => key !== CACHE_NAME).map((key) => caches.delete(key)),
        ),
      )
      .then(() => self.clients.claim()),
  );
});

// isStaticAsset: same-origin /static/* only. Cross-origin requests are never
// touched, and the path check is what keeps household pages out of the cache.
function isStaticAsset(url) {
  return url.origin === self.location.origin && url.pathname.startsWith('/static/');
}

// wantsHTML: an htmx fragment request, or anything that would accept an HTML
// document. Both carry household data, so both are network-only.
function wantsHTML(request) {
  return (
    request.headers.get('HX-Request') !== null ||
    (request.headers.get('Accept') || '').includes('text/html')
  );
}

self.addEventListener('fetch', (event) => {
  const { request } = event;

  // Never interfere with mutations or non-GET verbs: a cached or replayed
  // POST would be a correctness disaster, not just a staleness one.
  if (request.method !== 'GET') {
    return;
  }

  const url = new URL(request.url);

  // (1) Navigations: network-only, offline page as the fallback. This is the
  // ONLY branch that serves cached HTML, and it serves a static page that
  // contains no household data.
  if (request.mode === 'navigate') {
    event.respondWith(
      fetch(request).catch(() => caches.match('/offline', { cacheName: CACHE_NAME })),
    );
    return;
  }

  // (2) HTMX fragments and other HTML: network-only, never cached, and no
  // fallback — htmx leaves the DOM untouched on a failed request, which is
  // the correct outcome (the page keeps its last good content).
  if (wantsHTML(request)) {
    event.respondWith(fetch(request));
    return;
  }

  // (3) Static assets: cache-first for instant repeat loads, with background
  // revalidation so a deploy's new bytes land in the cache for next time.
  // (A CACHE_NAME bump is what forces them to be used immediately.)
  if (isStaticAsset(url)) {
    event.respondWith(
      caches.open(CACHE_NAME).then((cache) =>
        cache.match(request).then((cached) => {
          // The revalidation must be handed to waitUntil, not just left
          // dangling: once respondWith settles with the cached response the
          // browser is free to kill the worker, and an unawaited background
          // fetch/put would be cancelled — so the cache would never pick up
          // new bytes. Note this resolves only after cache.put completes.
          const update = fetch(reloadRequest(request.url))
            .then((response) => {
              if (response && response.ok) {
                return cache.put(request, response.clone()).then(() => response);
              }
              return response;
            })
            .catch(() => cached);

          if (cached) {
            event.waitUntil(update);
            return cached;
          }
          return update;
        }),
      ),
    );
  }

  // Everything else falls through to the browser's own handling.
});
