// §12 Kiosk — zero automated coverage before this run.
//
// A kiosk device holds its own identity, separate from a member login, so most
// of these run in a context with NO member session: that is the state a real
// entryway screen is in.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql, seedHouseholdB } = require('../tests/db');
const helpers = require('./helpers-settings');

const KIOSK_PAGES = ['/kiosk/chores', '/kiosk/meals', '/kiosk/shopping', '/kiosk/calendar', '/kiosk/photos'];

// generateCode asks the app for an activation code as a parent and returns the
// plaintext code. The code is only ever shown once, in the response, so it is
// scraped from the rendered page rather than read from the database (which
// stores a hash).
async function generateCode(page, name = 'Test kiosk') {
  await login(page, PERSONAS.owner);
  await page.goto('/settings');
  const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();

  // The reveal fragment is the only place the plaintext code exists — the
  // server keeps a hash — and it carries it in an input value, not in text.
  await page.evaluate(async ({ csrf_token, name }) => {
    const body = new URLSearchParams({ csrf_token, name }).toString();
    const res = await fetch('/settings/kiosk/generate', {
      method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' }, body,
    });
    document.body.innerHTML = await res.text();
  }, { csrf_token, name });

  const code = await page.locator('#kiosk-code-value').inputValue();
  expect(code, 'the generate response should reveal an activation code').toBeTruthy();
  return code;
}

// statusOf fetches a page for its status alone. page.goto() throws on some 4xx
// responses, which would mask exactly the refusals these tests assert.
async function statusOf(page, path) {
  const res = await page.request.get(path, { maxRedirects: 0 });
  return res.status();
}

test.describe('§12 kiosk', () => {
  test('T-12.1.1 a valid code activates a device and the views render', async ({ page, browser }) => {
    const code = await generateCode(page, 'Entryway');

    // A fresh context with no member session: the device's own identity is
    // what must carry these pages.
    const device = await browser.newContext();
    const kiosk = await device.newPage();

    await kiosk.goto(`/kiosk/activate?code=${encodeURIComponent(code)}`);
    const csrf_token = await kiosk.locator('input[name="csrf_token"]').first().inputValue();
    const status = await postForm(kiosk, '/kiosk/activate', { csrf_token, code });
    expect(status, 'a valid activation code must be accepted').toBe(303);

    const failures = [];
    for (const path of KIOSK_PAGES) {
      const status = await statusOf(kiosk, path);
      if (status !== 200) failures.push(`${path} -> ${status}`);
    }
    expect(failures, 'kiosk pages that did not render for an activated device').toEqual([]);
    await device.close();
  });

  test('T-12.2.1 unknown, expired and reused codes are refused', async ({ page, browser }) => {
    const code = await generateCode(page, 'Reuse probe');

    const device = await browser.newContext();
    const kiosk = await device.newPage();
    await kiosk.goto('/kiosk/activate');
    const csrf_token = await kiosk.locator('input[name="csrf_token"]').first().inputValue();

    const unknown = await postForm(kiosk, '/kiosk/activate', { csrf_token, code: 'ZZZZZZZZ' });
    expect(unknown, 'an unknown code must be refused').toBe(401);

    // Consume the real code, then replay it.
    const first = await postForm(kiosk, '/kiosk/activate', { csrf_token, code });
    expect(first, 'the first use of a valid code must succeed').toBe(303);

    const replay = await browser.newContext();
    const kiosk2 = await replay.newPage();
    await kiosk2.goto('/kiosk/activate');
    const token2 = await kiosk2.locator('input[name="csrf_token"]').first().inputValue();
    const reused = await postForm(kiosk2, '/kiosk/activate', { csrf_token: token2, code });
    expect(reused, 'an already-used code must not activate a second device').toBe(401);

    await device.close();
    await replay.close();
  });

  test('T-12.2.1b an expired code is refused', async ({ page, browser }) => {
    const code = await generateCode(page, 'Expiry probe');
    // Activation codes live 15 minutes; age this one past that rather than wait.
    psql("UPDATE nestova.kiosk_activation_code SET expires_at = now() - interval '1 minute' WHERE used_at IS NULL;");

    const device = await browser.newContext();
    const kiosk = await device.newPage();
    await kiosk.goto('/kiosk/activate');
    const csrf_token = await kiosk.locator('input[name="csrf_token"]').first().inputValue();
    const status = await postForm(kiosk, '/kiosk/activate', { csrf_token, code });
    expect(status, 'an expired code must be refused').toBe(401);
    await device.close();
  });

  test('T-12.2.2 kiosk pages refuse a device with no identity', async ({ browser }) => {
    const anon = await browser.newContext();
    const kiosk = await anon.newPage();
    const leaked = [];
    for (const path of KIOSK_PAGES) {
      if (await statusOf(kiosk, path) === 200) leaked.push(path);
    }
    expect(leaked, 'kiosk pages that rendered without a device identity').toEqual([]);
    await anon.close();
  });

  test('T-12.2.3 revoking a device takes effect immediately', async ({ page, browser }) => {
    const code = await generateCode(page, 'Revoke probe');

    const device = await browser.newContext();
    const kiosk = await device.newPage();
    await kiosk.goto('/kiosk/activate');
    const activateToken = await kiosk.locator('input[name="csrf_token"]').first().inputValue();
    expect(await postForm(kiosk, '/kiosk/activate', { csrf_token: activateToken, code })).toBe(303);
    expect(await statusOf(kiosk, '/kiosk/chores'), 'the device should work before revocation').toBe(200);

    const deviceId = psql(
      "SELECT id FROM nestova.kiosk_device ORDER BY created_at DESC LIMIT 1;",
    ).trim();
    await page.goto('/settings');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
    await postForm(page, `/settings/kiosk/${deviceId}/revoke`, { csrf_token });

    expect(await statusOf(kiosk, '/kiosk/chores'), 'a revoked device must lose access at once').toBe(401);
    await device.close();
  });

  test('T-12.2.4 a kiosk device cannot reach member-only routes', async ({ page, browser }) => {
    const code = await generateCode(page, 'Scope probe');

    const device = await browser.newContext();
    const kiosk = await device.newPage();
    await kiosk.goto('/kiosk/activate');
    const csrf_token = await kiosk.locator('input[name="csrf_token"]').first().inputValue();
    expect(await postForm(kiosk, '/kiosk/activate', { csrf_token, code })).toBe(303);

    const leaked = [];
    for (const path of ['/settings', '/admin/rewards', '/members/new', '/tasks/new']) {
      if (await statusOf(kiosk, path) === 200) leaked.push(path);
    }
    expect(leaked, 'member-only routes a kiosk device could reach').toEqual([]);
    await device.close();
  });
});

// ---------------------------------------------------------------------------
// Kiosk data, polling and resilience.
//
// Each kiosk tab's content wrapper re-polls GET /kiosk/<tab>/content every
// 15s (hx-trigger "every 15s", NES-130) and swaps itself in place. The poll
// tests install Playwright's fake clock before the page loads and pause it,
// so "15 seconds" is exact and no test waits in real time.
// ---------------------------------------------------------------------------

const { unique, householdId, seedChoreInstance, seedShoppingItem, shoppingStatus, newKiosk, latestDeviceId, settingsToken } = helpers;
const OWNER_B = { email: 'owner@other.local', password: PERSONAS.owner.password, displayName: 'Owner B' };
const POLL_MS = 15_000;

// pausedKiosk activates a fresh device, opens tab with a fake clock paused
// just after load, and records every content poll the tab issues.
async function pausedKiosk(browser, code, tab) {
  const context = await browser.newContext();
  const page = await context.newPage();
  await page.clock.install();
  const { status } = await helpers.activate(page, code);
  expect(status, 'a valid activation code must be accepted').toBe(303);

  const polls = [];
  page.on('request', (r) => {
    if (new URL(r.url()).pathname === `/kiosk/${tab}/content`) polls.push(r);
  });
  await page.goto(`/kiosk/${tab}`);
  const now = await page.evaluate(() => Date.now());
  await page.clock.pauseAt(now + 100);
  // A marker that only survives if the page is never reloaded.
  await page.evaluate(() => { window.__kioskNeverReloaded = true; });
  return { context, page, polls, content: page.locator(`#kiosk-${tab}-content`) };
}

// nextPoll advances the paused clock by ms, expects exactly one content poll
// to fire, and returns its response once the swap has been given time to run.
// The swapped-in wrapper arms its own 15s timer during htmx's settle step,
// which the extra 50ms of clock lets run.
async function nextPoll(kiosk, tab, ms = POLL_MS) {
  const responded = kiosk.page.waitForResponse((r) => new URL(r.url()).pathname === `/kiosk/${tab}/content`);
  await kiosk.page.clock.runFor(ms);
  const response = await responded;
  await response.finished();
  await kiosk.page.waitForTimeout(150);
  await kiosk.page.clock.runFor(50);
  return response;
}

async function ownerCode(page, label) {
  await login(page, PERSONAS.owner);
  return helpers.generateCode(page, `${label} ${unique()}`);
}

test.describe('§12 kiosk data and polling', () => {
  test('T-12.1.2 a shopping item can be ticked in-cart from the kiosk', async ({ page, browser }) => {
    const item = seedShoppingItem({ name: `Kiosk cart ${unique()}` });
    const kiosk = await newKiosk(browser, await ownerCode(page, 'Cart kiosk'));

    await kiosk.page.goto('/kiosk/shopping');
    const row = kiosk.page.getByTestId('kiosk-shopping-needed').locator('li', { hasText: item.name });
    await row.getByRole('button', { name: 'In cart' }).click();
    await kiosk.page.waitForURL((u) => new URL(u).pathname === '/kiosk/shopping');

    await expect(kiosk.page.getByTestId('kiosk-shopping-in-cart')).toContainText(item.name);
    await expect(kiosk.page.getByTestId('kiosk-shopping-needed').locator('li', { hasText: item.name })).toHaveCount(0);
    expect(shoppingStatus(item.id), 'the tick must persist').toBe('in_cart');
    await kiosk.context.close();
  });

  test('T-12.1.3 a kiosk view polls every 15 seconds and swaps in place without a reload', async ({ page, browser }) => {
    const kiosk = await pausedKiosk(browser, await ownerCode(page, 'Poll kiosk'), 'chores');

    // The first poll after load re-arms the wrapper at a known clock time.
    expect((await nextPoll(kiosk, 'chores')).status()).toBe(200);
    const armed = kiosk.polls.length;

    const chore = seedChoreInstance({ title: `Poll probe ${unique()}` });
    await kiosk.page.clock.runFor(POLL_MS - 100);
    await kiosk.page.waitForTimeout(300);
    expect(kiosk.polls.length, 'no poll may fire before 15 seconds').toBe(armed);
    await expect(kiosk.content).not.toContainText(chore.title);

    expect((await nextPoll(kiosk, 'chores', 200)).status()).toBe(200);
    expect(kiosk.polls.length, 'exactly one poll at 15 seconds').toBe(armed + 1);
    await expect(kiosk.content, 'the new chore must be swapped in').toContainText(chore.title);
    await expect(kiosk.page.locator('#kiosk-chores-content'), 'the wrapper is replaced, not duplicated').toHaveCount(1);
    expect(await kiosk.page.evaluate(() => window.__kioskNeverReloaded), 'the page must not reload').toBe(true);
    await kiosk.context.close();
  });

  test('T-12.2.5 a kiosk cannot see or touch household B data', async ({ page, browser }) => {
    const b = seedHouseholdB({
      householdName: 'Household B', ownerName: OWNER_B.displayName, ownerEmail: OWNER_B.email, copyHashFrom: PERSONAS.owner.email,
    });
    const choreA = seedChoreInstance({ title: `A chore ${unique()}` });
    const choreB = seedChoreInstance({ household: b.householdId, title: `B chore ${unique()}` });
    const itemA = seedShoppingItem({ name: `A item ${unique()}` });
    const itemB = seedShoppingItem({ household: b.householdId, name: `B item ${unique()}` });
    const kiosk = await newKiosk(browser, await ownerCode(page, 'Tenant kiosk'));

    await kiosk.page.goto('/kiosk/chores');
    await expect(kiosk.page.locator('#kiosk-chores-content')).toContainText(choreA.title);
    await expect(kiosk.page.locator('#kiosk-chores-content')).not.toContainText(choreB.title);
    await kiosk.page.goto('/kiosk/shopping');
    const shopping = kiosk.page.locator('#kiosk-shopping-content');
    await expect(shopping).toContainText(itemA.name);
    await expect(shopping).not.toContainText(itemB.name);

    // The one kiosk mutation, aimed at B's item by id.
    const csrf_token = await kiosk.page.locator('input[name="csrf_token"]').first().inputValue();
    const cross = await postForm(kiosk.page, `/kiosk/shopping/${itemB.id}/in-cart`, { csrf_token });
    expect(cross, "B's item must be indistinguishable from a missing one").toBe(404);
    expect(shoppingStatus(itemB.id), "B's item must be untouched").toBe('needed');
    // Sanity guard: the same request succeeds for A's own item.
    expect(await postForm(kiosk.page, `/kiosk/shopping/${itemA.id}/in-cart`, { csrf_token })).toBe(303);
    expect(shoppingStatus(itemA.id)).toBe('in_cart');
    await kiosk.context.close();
  });

  test('T-12.2.6 a change made in a member session reaches the kiosk within one poll', async ({ page, browser }) => {
    const kiosk = await pausedKiosk(browser, await ownerCode(page, 'Sync kiosk'), 'shopping');

    // The member adds an item through the groceries page, as a person would.
    const item = `Member added ${unique()}`;
    await page.goto('/groceries');
    await page.locator('#shopping-add-name').fill(item);
    await page.locator('#shopping-add-amount').fill('1');
    await page.locator('#shopping-add-unit').selectOption('count');
    await page.getByRole('button', { name: 'Add to list' }).click();
    await expect.poll(() => psql(`SELECT count(*) FROM nestova.shopping_list_item WHERE name = '${item}';`).trim()).toBe('1');

    await expect(kiosk.content).not.toContainText(item);
    expect((await nextPoll(kiosk, 'shopping')).status()).toBe(200);
    await expect(kiosk.content, 'the member change must appear after one poll').toContainText(item);
    expect(await kiosk.page.evaluate(() => window.__kioskNeverReloaded)).toBe(true);
    await kiosk.context.close();
  });

  test('T-12.3.1 polling resumes after a transient network drop', async ({ page, browser }) => {
    const kiosk = await pausedKiosk(browser, await ownerCode(page, 'Offline kiosk'), 'chores');
    expect((await nextPoll(kiosk, 'chores')).status()).toBe(200);

    await kiosk.context.setOffline(true);
    const failed = kiosk.page.waitForEvent('requestfailed', (r) => new URL(r.url()).pathname === '/kiosk/chores/content');
    await kiosk.page.clock.runFor(POLL_MS);
    await failed;
    await kiosk.page.clock.runFor(50);

    const chore = seedChoreInstance({ title: `After outage ${unique()}` });
    await kiosk.context.setOffline(false);
    expect((await nextPoll(kiosk, 'chores')).status(), 'the next poll after reconnecting').toBe(200);
    await expect(kiosk.content, 'data changed during the outage must arrive').toContainText(chore.title);
    await kiosk.context.close();
  });

  test('T-12.3.2 a long-open kiosk keeps one poll timer and a flat DOM and heap', async ({ page, browser }) => {
    // Hours cannot run in a test; forty polls (ten simulated minutes, past the
    // two-minute screensaver) is the proxy. Stacked timers would show as more
    // than one poll per interval; a leak as growing nodes, listeners or heap.
    // The shopping tab carries the same wrapper as every tab but no per-row QR
    // renders, which keeps forty real round trips inside the time budget.
    test.setTimeout(180_000);
    const kiosk = await pausedKiosk(browser, await ownerCode(page, 'Soak kiosk'), 'shopping');
    const cdp = await kiosk.context.newCDPSession(kiosk.page);
    const sample = async () => {
      await cdp.send('HeapProfiler.collectGarbage');
      const { usedSize } = await cdp.send('Runtime.getHeapUsage');
      const { nodes, jsEventListeners } = await cdp.send('Memory.getDOMCounters');
      return { heap: usedSize, nodes, listeners: jsEventListeners };
    };

    for (let i = 0; i < 10; i += 1) await nextPoll(kiosk, 'shopping'); // warm-up, screensaver engages
    const baseline = await sample();
    const pollsBefore = kiosk.polls.length;
    const intervals = 30;
    for (let i = 0; i < intervals; i += 1) await nextPoll(kiosk, 'shopping');
    const after = await sample();

    expect(kiosk.polls.length - pollsBefore, 'one poll per interval, never stacked').toBe(intervals);
    expect(after.nodes - baseline.nodes, 'DOM node growth').toBeLessThanOrEqual(50);
    expect(after.listeners - baseline.listeners, 'event listener growth').toBeLessThanOrEqual(10);
    expect(after.heap - baseline.heap, 'JS heap growth in bytes').toBeLessThan(1024 * 1024);
    await kiosk.context.close();
  });

  test('T-12.3.3 revoking mid-poll yields a clean 401 and leaves the view intact', async ({ page, browser }) => {
    const chore = seedChoreInstance({ title: `Before revoke ${unique()}` });
    const kiosk = await pausedKiosk(browser, await ownerCode(page, 'Mid-poll kiosk'), 'chores');
    const deviceId = latestDeviceId();
    await expect(kiosk.content).toContainText(chore.title);
    const before = await kiosk.content.innerHTML();
    const csrf_token = await settingsToken(page);

    // Hold the poll in flight, revoke the device, then let the poll through.
    await kiosk.page.route('**/kiosk/chores/content', async (route) => {
      expect(await postForm(page, `/settings/kiosk/${deviceId}/revoke`, { csrf_token }), 'revoke').toBe(303);
      await route.continue();
    });
    const response = await nextPoll(kiosk, 'chores');
    expect(response.status(), 'the in-flight poll must be refused').toBe(401);
    expect(await response.text(), 'the 401 carries no fragment').toBe('');

    await expect(kiosk.page.locator('#kiosk-chores-content'), 'the wrapper survives').toHaveCount(1);
    expect(await kiosk.content.innerHTML(), 'the fragment must not be replaced').toBe(before);
    await expect(kiosk.page.locator('body')).not.toContainText(/unauthori[sz]ed/i);
    await kiosk.context.close();
  });

  test('T-12.3.4 every kiosk touch target is at least 48x48 CSS pixels', async ({ page, browser }) => {
    // AC3 (kioskTabBar's doc): targets are at least 48x48. Measured at the
    // HP 24-r014's 1920x1080 on every tab, with a needed shopping item so the
    // "In cart" button renders.
    seedShoppingItem({ name: `Touch probe ${unique()}` });
    const code = await ownerCode(page, 'Touch kiosk');
    const context = await browser.newContext({ viewport: { width: 1920, height: 1080 }, hasTouch: true });
    const kiosk = await context.newPage();
    expect((await helpers.activate(kiosk, code)).status).toBe(303);

    const tooSmall = [];
    for (const tab of ['chores', 'meals', 'shopping', 'calendar', 'photos']) {
      await kiosk.goto(`/kiosk/${tab}`);
      const found = await kiosk.evaluate(() => {
        const selector = 'a[href], button, input:not([type="hidden"]), select, textarea, [role="button"], [hx-get], [hx-post], [x-on\\:click]';
        return [...document.querySelectorAll(selector)]
          .filter((el) => el.id !== 'kiosk-chores-content' && !/^kiosk-.*-content$/.test(el.id))
          .map((el) => ({ el, box: el.getBoundingClientRect(), style: getComputedStyle(el) }))
          .filter(({ box, style }) => box.width > 0 && box.height > 0 && style.visibility !== 'hidden' && style.display !== 'none')
          .filter(({ box }) => box.width < 48 || box.height < 48)
          .map(({ el, box }) => `${el.tagName.toLowerCase()} "${(el.textContent || el.getAttribute('aria-label') || '').trim().slice(0, 30)}" ${Math.round(box.width)}x${Math.round(box.height)}`);
      });
      tooSmall.push(...found.map((f) => `${tab}: ${f}`));
    }
    expect(tooSmall, 'touch targets under 48x48').toEqual([]);
    await context.close();
  });
});
