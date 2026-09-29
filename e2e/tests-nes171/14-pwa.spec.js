// §14 PWA and offline — previously covered only by a manual guide.
//
// The worker (web/static/sw.js) registers on localhost because localhost is a
// secure context, so everything here runs without Tailscale HTTPS. Offline is
// simulated with context.setOffline, which the worker's own fetch() also
// observes. What still needs a real phone over HTTPS — installing to the home
// screen, Android's standalone window — is NES-153/NES-154's manual guide and
// is not claimed here.
//
// One Playwright trap shapes these tests: a request the worker answers with
// respondWith() never reaches Playwright's router. Only requests the worker
// leaves alone (non-GET, cross-origin) can be intercepted with route().
const { test, expect } = require('@playwright/test');
const { PASSWORD, PERSONAS } = require('../tests/fixtures');
const { login } = require('./helpers');
const { psql, seedMemberInA } = require('../tests/db');
const {
  waitForController,
  cacheInventory,
  watchHtmx,
  htmxCounters,
  markDocument,
  expectSameDocument,
} = require('./helpers-pwa');

function uniqueSuffix() {
  return `${Date.now()}-${Math.floor(Math.random() * 1e6)}`;
}

function shoppingRows(itemName) {
  return Number(psql(
    `SELECT count(*) FROM nestova.shopping_list_item WHERE name = '${itemName}';`,
  ).trim());
}

// seedChild adds a fresh child with a known point balance, so a page that
// shows "Your Balance" identifies exactly whose data it is rendering.
function seedChild(label, points) {
  const suffix = uniqueSuffix();
  const displayName = `PWA ${label} ${suffix}`;
  const email = `pwa-${label.toLowerCase()}-${suffix}@test.local`;
  const id = seedMemberInA({ displayName, email, role: 'child', copyHashFrom: PERSONAS.owner.email });
  const household = psql("SELECT id FROM identity.household WHERE name = 'Household A' LIMIT 1;").trim();
  psql(`
    INSERT INTO nestova.point_ledger (id, household_id, member_id, source_type, source_id, points, created_at)
    VALUES (gen_random_uuid(), '${household}', '${id}', 'adjustment', gen_random_uuid(), ${points}, now());
  `);
  return { email, password: PASSWORD, displayName, role: 'child' };
}

// fillShoppingItem types a new shopping item into the /groceries add form
// without submitting it.
async function fillShoppingItem(page, itemName) {
  await page.locator('#shopping-add-name').fill(itemName);
  await page.locator('#shopping-add-amount').fill('1');
  await page.locator('#shopping-add-unit').selectOption('count');
}

const shoppingAddButton = (page) => page.getByRole('button', { name: 'Add to list' });

test.describe('§14 PWA', () => {
  test('T-14.1.1 the manifest is served and linked from every page', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const missing = [];
    for (const path of ['/', '/tasks', '/settings', '/photos']) {
      await page.goto(path);
      const href = await page.locator('link[rel="manifest"]').first().getAttribute('href');
      if (!href) missing.push(path);
    }
    expect(missing, 'pages with no manifest link').toEqual([]);

    const res = await page.request.get('/static/manifest.webmanifest');
    expect(res.status(), 'the manifest itself must be served').toBe(200);
    const manifest = await res.json();
    expect(manifest.name || manifest.short_name, 'the manifest must name the app').toBeTruthy();
    expect(Array.isArray(manifest.icons) && manifest.icons.length, 'the manifest must declare icons').toBeTruthy();
  });

  test('T-14.2.5 every declared icon resolves', async ({ page }) => {
    const manifest = await (await page.request.get('/static/manifest.webmanifest')).json();
    const broken = [];
    for (const icon of manifest.icons ?? []) {
      const url = icon.src.startsWith('/') ? icon.src : `/static/${icon.src}`;
      const res = await page.request.get(url);
      if (res.status() !== 200) broken.push(`${url} -> ${res.status()}`);
    }
    expect(broken, 'manifest icons that did not resolve').toEqual([]);
  });

  test('T-14.1.2 the service worker is served and registers', async ({ page }) => {
    const res = await page.request.get('/sw.js');
    expect(res.status(), '/sw.js must be served').toBe(200);
    expect(res.headers()['content-type'] ?? '', 'the worker must be served as JavaScript')
      .toMatch(/javascript/);

    await login(page, PERSONAS.owner);
    const registered = await page.evaluate(async () => {
      if (!('serviceWorker' in navigator)) return 'unsupported';
      const reg = await navigator.serviceWorker.getRegistration();
      return reg ? 'registered' : 'none';
    });
    expect(registered, 'the page should register a service worker').toBe('registered');
  });

  test('T-14.1.3 the offline fallback page renders', async ({ page }) => {
    const res = await page.request.get('/offline');
    expect(res.status(), '/offline must be served').toBe(200);
    const body = await res.text();
    expect(body, 'the offline page should explain the situation').toMatch(/offline/i);
  });
});

test.describe('§14 the worker in use', () => {
  test('T-14.1.4 HTMX requests still work with the worker active', async ({ page }) => {
    await login(page, PERSONAS.owner);
    await page.goto('/photos');
    await waitForController(page);
    await markDocument(page);

    // An HTMX GET: the worker's HX-Request branch passes it to the network
    // (respondWith(fetch)), so the response must come back THROUGH the worker
    // and still be swapped in. The grid re-fetch is the app's own trigger.
    await page.evaluate(() => { document.getElementById('photo-grid').dataset.stale = '1'; });
    const [grid] = await Promise.all([
      page.waitForResponse((r) => new URL(r.url()).pathname === '/photos/grid'),
      page.evaluate(() => window.htmx.trigger('#photo-grid', 'photos-uploaded')),
    ]);
    expect(grid.status()).toBe(200);
    expect(grid.fromServiceWorker(), 'the fragment must have passed through the worker').toBe(true);
    expect(await grid.request().headerValue('hx-request')).toBe('true');
    await expect.poll(() => page.evaluate(() => document.getElementById('photo-grid').dataset.stale ?? 'swapped'))
      .toBe('swapped');

    // An HTMX POST: the worker leaves non-GET alone, and the write must land.
    await page.goto('/groceries');
    await waitForController(page);
    const item = `SW probe ${uniqueSuffix()}`;
    await fillShoppingItem(page, item);
    await shoppingAddButton(page).click();
    await expect(page.locator('li').filter({ hasText: item }).first()).toBeVisible();
    expect(shoppingRows(item)).toBe(1);
  });

  test('T-14.2.1 going offline mid-HTMX-request shows a failure and leaves no stuck request state', async ({ page, context }) => {
    test.fail(true, 'DEFECT: a failed HTMX request (offline) gives no visible feedback; no htmx:sendError handler exists');
    await login(page, PERSONAS.owner);
    await page.goto('/groceries');
    await waitForController(page);
    await watchHtmx(page);
    const item = `Offline mid-request ${uniqueSuffix()}`;
    await fillShoppingItem(page, item);

    // The connection drops while the request is in flight: the route handler
    // takes the browser offline, then fails the request the way a dropped
    // network does. POST is not handled by the worker, so the router sees it.
    await page.route('**/groceries/shopping', async (route) => {
      if (route.request().method() !== 'POST') return route.continue();
      await context.setOffline(true);
      return route.abort('internetdisconnected');
    });
    // Whatever counts as "reported" must not already be on the page, or the
    // final assertion could pass on unrelated text.
    const failureNotice = page.getByRole('alert')
      .or(page.getByText(/offline|no connection|could ?n.t|failed|try again/i));
    await expect(failureNotice).toHaveCount(0);

    await shoppingAddButton(page).click();
    await expect.poll(async () => (await htmxCounters(page)).sendErrors, 'htmx must see the send fail').toBe(1);

    // No stuck spinner: htmx's in-flight class is cleared and the button is
    // usable again. (Verified passing; the app declares no hx-indicator.)
    await expect(page.locator('.htmx-request')).toHaveCount(0);
    await expect(shoppingAddButton(page)).toBeEnabled();
    expect(shoppingRows(item), 'nothing was written').toBe(0);

    // A visible failure: the member must be told the add did not happen.
    // Today the form just sits there as if the click was ignored.
    await expect(failureNotice.first(), 'a failed request must be reported to the member')
      .toBeVisible({ timeout: 3_000 });
  });

  test('T-14.2.2 offline then online, the app recovers without a manual reload', async ({ page, context }) => {
    await login(page, PERSONAS.owner);

    // An HTMX GET through the worker: fails offline with the DOM untouched,
    // then succeeds in the SAME document once the connection is back.
    await page.goto('/photos');
    await waitForController(page);
    await watchHtmx(page);
    await markDocument(page);
    await page.evaluate(() => { document.getElementById('photo-grid').dataset.stale = '1'; });
    const refreshGrid = () => page.evaluate(() => window.htmx.trigger('#photo-grid', 'photos-uploaded'));
    const gridIsStale = () => page.evaluate(() => document.getElementById('photo-grid').dataset.stale === '1');

    await context.setOffline(true);
    await refreshGrid();
    await expect.poll(async () => (await htmxCounters(page)).sendErrors, 'the offline request must fail').toBe(1);
    expect(await gridIsStale(), 'a failed request leaves the last good content').toBe(true);

    await context.setOffline(false);
    await refreshGrid();
    await expect.poll(gridIsStale, 'the same trigger works again once online').toBe(false);
    await expectSameDocument(page);

    // An HTMX POST: the form keeps what was typed while offline, and the same
    // button works once online. (A successful add answers with HX-Redirect,
    // so the app itself reloads the list — the test never does.)
    await page.goto('/groceries');
    await watchHtmx(page);
    const item = `Recovers ${uniqueSuffix()}`;
    await fillShoppingItem(page, item);
    await context.setOffline(true);
    await shoppingAddButton(page).click();
    await expect.poll(async () => (await htmxCounters(page)).sendErrors).toBe(1);
    expect(shoppingRows(item)).toBe(0);

    await context.setOffline(false);
    await expect(page.locator('#shopping-add-name')).toHaveValue(item);
    await shoppingAddButton(page).click();
    await expect(page.locator('li').filter({ hasText: item }).first()).toBeVisible();
    expect(shoppingRows(item)).toBe(1);

    // And navigations go to the network again, not the offline fallback.
    await page.goto('/tasks');
    await expect(page.getByRole('heading', { name: "You're offline" })).toHaveCount(0);
  });

  test('T-14.2.3 a new worker from a deploy activates and evicts the old cache', async ({ page }) => {
    // Detection, server half: /sw.js is revalidated on every update check
    // (no-cache) against a content-hash ETag, so a deploy that changes the
    // script is always seen and an unchanged one costs only a 304.
    const first = await page.request.get('/sw.js');
    expect(first.headers()['cache-control']).toBe('no-cache');
    const etag = first.headers().etag;
    expect(etag, 'the worker script needs a validator').toBeTruthy();
    const revalidated = await page.request.get('/sw.js', { headers: { 'If-None-Match': etag } });
    expect(revalidated.status(), 'an unchanged worker revalidates to 304').toBe(304);

    await login(page, PERSONAS.owner);
    await waitForController(page);
    await expect.poll(async () => Object.keys(await cacheInventory(page))).toEqual(['nestova-static-v1']);

    // Leftovers a real upgrade meets: a previous version's cache, and a cache
    // some other feature on the origin owns.
    await page.evaluate(async () => {
      await (await caches.open('nestova-static-v0')).put('/static/stale.css', new Response('stale'));
      await (await caches.open('unrelated-feature')).put('/unrelated', new Response('keep me'));
    });

    // Activation, client half. The update check's script fetch is made by the
    // browser process, where neither route() nor CDP Fetch can rewrite its
    // bytes, so the new version is introduced by script URL instead: a
    // registration whose script URL changes installs a new worker even though
    // /sw.js ignores the query — the spec's Update algorithm only skips the
    // install when BOTH the URL and the bytes are unchanged. What
    // follows — install, skipWaiting, activate, claim, evict — is the same code
    // path a byte-changed deploy takes.
    const outcome = await page.evaluate(async () => {
      let controllerChanges = 0;
      navigator.serviceWorker.addEventListener('controllerchange', () => { controllerChanges += 1; });
      const before = navigator.serviceWorker.controller.scriptURL;
      await navigator.serviceWorker.register('/sw.js?deploy=2');
      const deadline = Date.now() + 15_000;
      while (Date.now() < deadline) {
        const current = navigator.serviceWorker.controller;
        if (current && current.scriptURL !== before && current.state === 'activated') break;
        await new Promise((r) => setTimeout(r, 100));
      }
      return {
        before,
        after: navigator.serviceWorker.controller.scriptURL,
        state: navigator.serviceWorker.controller.state,
        controllerChanges,
      };
    });
    expect(new URL(outcome.before).search).toBe('');
    expect(new URL(outcome.after).search, 'the new version controls the page').toBe('?deploy=2');
    expect(outcome.state).toBe('activated');
    expect(outcome.controllerChanges, 'it took over the OPEN page, no reload').toBe(1);

    // activate() evicts only its own prefix's stale versions.
    await expect.poll(async () => Object.keys(await cacheInventory(page)).sort())
      .toEqual(['nestova-static-v1', 'unrelated-feature']);

    // The new worker serves: the offline fallback still works under it.
    await page.context().setOffline(true);
    await page.goto('/tasks');
    await expect(page.getByRole('heading', { name: "You're offline" })).toBeVisible();
    await page.context().setOffline(false);
  });

  test('T-14.2.4 after logout, Back does not bring the previous member\'s page back', async ({ page }) => {
    test.fail(true, 'DEFECT: authenticated HTML has no Cache-Control: no-store, so Back after logout re-renders the member page from the HTTP cache');
    const first = seedChild('Leaky', 4242);
    await login(page, first);
    await page.goto('/rewards');
    await expect(page.locator('h2:has-text("Your Balance") + p')).toContainText('4242');

    await page.getByRole('button', { name: 'Log out' }).click();
    await page.waitForURL((u) => new URL(u).pathname === '/login');

    // Whoever picks the device up next presses Back. The session is gone, so
    // the server would answer /rewards with a redirect to /login — but the
    // browser never asks: the page is restored from its HTTP cache, balance
    // and all. (Not the service worker: it reproduces with workers blocked,
    // and T-14.2.4b shows the worker's cache holds no pages.)
    await page.goBack();
    await page.waitForLoadState('load');
    await expect(page.locator('h2:has-text("Your Balance")'), 'a member-only page must not be shown after logout')
      .toHaveCount(0, { timeout: 3_000 });
  });

  test('T-14.2.4b the worker caches no member page and serves none offline after logout', async ({ page, context }) => {
    const first = seedChild('First', 4242);
    const second = seedChild('Second', 7);

    // One device, used by one member and then another.
    await login(page, first);
    await waitForController(page);
    for (const path of ['/', '/rewards', '/tasks', '/settings', '/photos', '/groceries']) {
      await page.goto(path);
    }
    await page.goto('/rewards');
    await expect(page.locator('h2:has-text("Your Balance") + p')).toContainText('4242');

    // Nothing that can hold member data is in Cache Storage: only /offline
    // and /static/* — the worker's whole safety argument.
    const inventory = await cacheInventory(page);
    const cachedPages = Object.values(inventory).flat()
      .filter((p) => p !== '/offline' && !p.startsWith('/static/'));
    expect(cachedPages, 'household pages found in Cache Storage').toEqual([]);

    await page.getByRole('button', { name: 'Log out' }).click();
    await page.waitForURL((u) => new URL(u).pathname === '/login');

    // Offline after logout: the member's page is not served from the worker —
    // it falls back to the static offline page.
    await context.setOffline(true);
    await page.goto('/rewards');
    await expect(page.getByRole('heading', { name: "You're offline" })).toBeVisible();
    await expect(page.locator('body')).not.toContainText('4242');
    await expect(page.locator('body')).not.toContainText(first.displayName);
    await context.setOffline(false);

    // Back online, the next member sees only their own data.
    await login(page, second);
    await page.goto('/rewards');
    await expect(page.locator('h2:has-text("Your Balance") + p')).toContainText('7');
    await expect(page.locator('h2:has-text("Your Balance") + p')).not.toContainText('4242');
  });
});
