// Service-worker helpers for the §14 PWA spec.
//
// localhost is a secure context, so /sw.js registers here exactly as it does
// behind Tailscale HTTPS. Every Playwright test gets a fresh browser context,
// which means a fresh registration: nothing a test does to the worker or its
// caches survives into the next one.
const { expect } = require('@playwright/test');

// waitForController blocks until the page is controlled by an ACTIVATED
// worker. The layout registers on window 'load' and the worker claims open
// clients on activate, so no reload is needed — but a test that asserts
// "this went through the worker" before that point would pass vacuously.
async function waitForController(page) {
  await page.waitForFunction(
    () => navigator.serviceWorker && navigator.serviceWorker.controller
      && navigator.serviceWorker.controller.state === 'activated',
    null,
    { timeout: 15_000 },
  );
}

// cacheInventory lists every cache and the request paths it holds.
async function cacheInventory(page) {
  return page.evaluate(async () => {
    const out = {};
    for (const key of await caches.keys()) {
      const cache = await caches.open(key);
      out[key] = (await cache.keys()).map((req) => new URL(req.url).pathname);
    }
    return out;
  });
}

// watchHtmx records htmx's request lifecycle on the page so a test can tell
// "the request failed and htmx noticed" from "nothing happened at all".
async function watchHtmx(page) {
  await page.evaluate(() => {
    window.__htmx = { sendErrors: 0, responseErrors: 0, afterRequests: 0 };
    document.body.addEventListener('htmx:sendError', () => { window.__htmx.sendErrors += 1; });
    document.body.addEventListener('htmx:responseError', () => { window.__htmx.responseErrors += 1; });
    document.body.addEventListener('htmx:afterRequest', () => { window.__htmx.afterRequests += 1; });
  });
}

async function htmxCounters(page) {
  return page.evaluate(() => ({ ...window.__htmx }));
}

// markDocument tags the current document so a test can prove a later
// assertion ran in the SAME document — i.e. nothing reloaded the page.
async function markDocument(page) {
  await page.evaluate(() => { window.__sameDocument = true; });
}

async function expectSameDocument(page) {
  expect(await page.evaluate(() => window.__sameDocument === true), 'the page must not have reloaded').toBe(true);
}

module.exports = {
  waitForController,
  cacheInventory,
  watchHtmx,
  htmxCounters,
  markDocument,
  expectSameDocument,
};
