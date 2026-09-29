// §14 PWA and offline — previously covered only by a manual guide.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login } = require('./helpers');

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
