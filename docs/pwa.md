# PWA: installability, the service worker, and offline behavior

Nestova installs to an Android home screen as a Progressive Web App. There
is no native code and no JSON API — the PWA wraps the same server-rendered
HTMX app, so it gets feature parity for free (NES-150).

Three pieces make that work:

- **`web/static/manifest.webmanifest`** — the app's identity: name, icons,
  standalone display mode, and the Hearth theme colors (NES-151).
- **`web/static/sw.js`** — the service worker: offline fallback and static
  asset caching (NES-152).
- **HTTPS via Tailscale** — a service worker only registers in a secure
  context (NES-153).

## The caching strategy, and why it is so conservative

Nestova renders HTML on the server, per household, per member. A cached
HTML document or HTMX fragment is therefore not merely stale — it can show
one member data that has already changed, or resurrect a chore someone
completed. So the worker **never caches anything that can contain
household data**.

Three fetch branches, in order:

| Request | Strategy | Rationale |
|---|---|---|
| Navigation (`mode === 'navigate'`) | Network-only, falling back to `/offline` | The only branch that serves cached HTML — and `/offline` is a static page with no household data |
| HTML / `HX-Request` | Network-only, never cached, no fallback | htmx leaves the DOM untouched on failure, so the page keeps its last good content |
| Same-origin `/static/*` | Cache-first + background revalidation | Static assets; instant repeat loads, refreshed opportunistically |

Non-GET requests are never intercepted: a cached or replayed POST would be
a correctness disaster rather than a staleness one. Cross-origin requests
are left alone entirely.

## Bumping the cache version

Static asset URLs are **not** content-hashed, so a deploy reuses the same
paths. `CACHE_NAME` in `web/static/sw.js` carries the version:

```js
const CACHE_PREFIX = 'nestova-static-';
const CACHE_NAME = `${CACHE_PREFIX}v1`;
```

Bump only the version suffix, keeping the prefix: `activate` evicts
caches carrying that prefix and leaves anything else on the origin
alone, so a renamed prefix would orphan the old cache instead of
replacing it.

**Bump the suffix whenever any pre-cached asset changes** — a CSS rebuild,
a font swap, an icon or manifest edit. The bump forces a fresh install-time
pre-cache of every listed asset, and the worker's `activate` handler then
deletes every older cache.

Background revalidation means an online client will *often* pick up new
bytes without a bump, but only for assets it actually re-requests, and only
once it is online. The bump is the reliable path: it refreshes the whole
pre-cache list at once, including for a client that has been offline. Skip
it and some clients keep old CSS for an unbounded time — and it will not
reproduce in a fresh browser, which makes it painful to diagnose.

Treat it as part of the release checklist, alongside `make assets`.

## Why `/sw.js` is served from the site root

A service worker's scope is capped at the directory it is served from. A
worker at `/static/sw.js` could only ever control `/static/*` — it would
never see a page navigation, so the offline fallback could not work. It
therefore gets a dedicated root route in
`internal/platform/httpserver/server.go` rather than riding the `/static/`
mount.

That route also sets `Cache-Control: no-cache`, deliberately bypassing the
one-hour lifetime `/static/` uses. An aggressively cached worker script is
the classic stuck-on-old-code trap: clients keep running a superseded
worker, including its old caching logic, long after a deploy. `no-cache`
still permits a revalidated 304, so this costs a conditional request rather
than a full refetch.

## Registration is optional by design

The registration snippet in `layout.templ` is guarded by
`'serviceWorker' in navigator` and swallows registration errors.
`navigator.serviceWorker` is undefined outside a secure context, so:

- The **kiosk** and any **plain-HTTP LAN** access simply skip registration
  and keep working exactly as before.
- A registration failure never breaks the page — the app is fully
  functional without a worker; only the offline page and asset caching are
  lost.

The manifest and worker are wired into the **member shell only**.
`album.templ` and `kiosk.templ` are standalone appliance documents nobody
installs to a home screen.

## Manual test plan

There is no service-worker unit-test harness; Go tests cover the routes
(`/sw.js` content type and `no-cache`, `/offline` rendering) and the
manifest, but the fetch branches need a browser. `localhost` counts as a
secure context, so all of this is verifiable on a dev box without HTTPS.

With the server running and a household onboarded:

1. **Registration** — DevTools > Application > Service Workers shows
   `/sw.js` **activated and running**, scope `/`.
2. **Pre-cache** — Application > Cache Storage > `nestova-static-v1`
   contains `/offline`, the favicon, the CSS, both fonts, the manifest, and all five
   icons (11 entries).
3. **No cached HTML** — nothing in Cache Storage besides `/offline` is a
   page. Navigate around, then re-check: still only `/offline`.
4. **HTML always hits the server** — with the Network tab open, visit a few
   pages and confirm each appears in the **server log**. (DevTools labels
   navigations "from ServiceWorker" even when the worker fetched them from
   the network — that label means "passed through the worker", not "served
   from cache", so the server log is the authoritative check.)
5. **Static assets come from cache** — revisit a page; in the Network tab
   `app.css` reports its size as **"(ServiceWorker)"**, meaning the cached
   copy was served. Do *not* judge this by the server log: the branch is
   cache-first **with background revalidation**, so a network request for
   the same asset is expected and correct — it refreshes the cache for
   next time without delaying this load.
6. **Offline fallback** — tick Network > Offline (or stop the server) and
   navigate: the branded offline page appears, not Chrome's dinosaur.
7. **Version bump** — change `CACHE_NAME`, reload twice: the new cache
   appears and the old one is gone.

Steps 1–6 were verified this way on the dev box for NES-152 by driving
headless Chrome; step 7 is exercised at each release.
