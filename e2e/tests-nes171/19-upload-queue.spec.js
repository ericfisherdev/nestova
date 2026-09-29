// §11.3 the drag-and-drop upload queue (NES-124).
//
// Split from 18-photos.spec.js because this is a different subject: everything
// here is client-side behaviour in web/static/js/upload-queue.js — per-file
// progress, per-file failure, and what survives navigating away — rather than
// what the server accepts.
//
// Files are supplied through #photo-file, which is wired to the same
// enqueueFiles() the drop handler calls (@change on the input,
// @drop.prevent on the dropzone), so setInputFiles exercises the real queue
// without synthesising a DataTransfer. The one case that genuinely needs a drop
// event constructs one.
const { test, expect } = require('@playwright/test');
const fs = require('fs');
const { PERSONAS } = require('../tests/fixtures');
const { login } = require('./helpers');
const { psql } = require('../tests/db');
const fixtures = require('./media-fixtures');

const TS = Date.now();
const BATCH = 50;

test.beforeAll(() => {
  test.skip(!fixtures.hasMagick(), 'ImageMagick is required to generate the image fixtures');
});

test.afterAll(() => fixtures.cleanup());

// batchFiles generates n byte-distinct images. Distinct matters: NES-148
// resolves identical content to the existing photo, so a batch of clones would
// report 1 uploaded and 49 duplicates and prove nothing about the queue.
function batchFiles(prefix, n) {
  return Array.from({ length: n }, (_, i) =>
    fixtures.noise(`${prefix}-${i}.png`, { width: 24 + i, height: 24 + i }),
  );
}

function photosSince(marker) {
  return Number(psql(
    `SELECT count(*) FROM nestova.photo WHERE caption = '${marker}';`,
  ).trim());
}

// brokenPhotos counts rows that would be a half-written upload: no bytes, or a
// storage reference that resolves to nothing.
function brokenPhotos() {
  return Number(psql(
    "SELECT count(*) FROM nestova.photo WHERE size_bytes IS NULL OR size_bytes <= 0 OR storage_ref = '';",
  ).trim());
}

async function openPhotos(page, caption) {
  await page.goto('/photos');
  await expect(page.locator('[data-testid="upload-dropzone"]')).toBeVisible();
  if (caption) await page.locator('#upload-caption').fill(caption);
}

test.describe('§11.3 the upload queue', () => {
  test('T-11.3.1 a 50-file batch queues, shows per-file progress, and completes', async ({
    page,
  }) => {
    test.setTimeout(180_000);
    await login(page, PERSONAS.owner);
    const caption = `Batch 50 ${TS}`;
    await openPhotos(page, caption);

    await page.locator('#photo-file').setInputFiles(batchFiles('batch', BATCH));

    // Every file appears as its own row: the queue is per-file, not a single
    // aggregate bar.
    const rows = page.locator('[data-testid="upload-queue"] li');
    await expect(rows).toHaveCount(BATCH);

    // Progress is per file too, and only while that file is in flight — the
    // <progress> element is bound to item.progress and shown for 'uploading'.
    await expect
      .poll(async () => page.locator('[data-testid="upload-queue"] progress').count(), {
        timeout: 30_000,
      })
      .toBeGreaterThan(0);

    // The batch settles, and its summary reports what happened.
    const summary = page.locator('[data-testid="upload-summary"]');
    await expect(summary).toBeVisible({ timeout: 150_000 });
    await expect(summary).toContainText(`${BATCH} uploaded`);
    await expect(summary).toContainText('0 failed');

    expect(photosSince(caption)).toBe(BATCH);
  });

  test('T-11.3.2 one bad file among 50 is reported alone; the rest still upload', async ({
    page,
  }) => {
    test.setTimeout(180_000);
    await login(page, PERSONAS.owner);
    const caption = `Batch with a dud ${TS}`;
    await openPhotos(page, caption);

    const good = batchFiles('mixed', BATCH - 1);
    const bad = fixtures.raw('dud.png', 'not an image at all, despite the extension\n');
    // Dropped in the middle, so a naive implementation that stops at the first
    // failure would leave the tail unsent.
    const files = [...good.slice(0, 25), bad, ...good.slice(25)];

    await page.locator('#photo-file').setInputFiles(files);
    await expect(page.locator('[data-testid="upload-queue"] li')).toHaveCount(BATCH);

    const summary = page.locator('[data-testid="upload-summary"]');
    await expect(summary).toBeVisible({ timeout: 150_000 });

    // The 49 good ones land, and the failure is reported as one file, not as a
    // failed batch. The dud is caught client-side by the type pre-check
    // ('skipped') or server-side as a 415 ('failed') — either is a per-file
    // outcome, and both leave the rest alone.
    await expect(summary).toContainText(`${BATCH - 1} uploaded`);
    expect(await summary.textContent()).toMatch(/1 failed|1 skipped/);
    expect(photosSince(caption)).toBe(BATCH - 1);

    // The dud's own row says so, next to its name.
    const dudRow = page.locator('[data-testid="upload-queue"] li').filter({ hasText: 'dud.png' });
    await expect(dudRow).toHaveCount(1);
    expect(await dudRow.textContent()).toMatch(/skipped|unsupported|failed|error/i);
  });

  test('T-11.3.3 navigating away mid-batch leaves no half-written photo', async ({ page }) => {
    test.setTimeout(120_000);
    await login(page, PERSONAS.owner);
    const caption = `Abandoned batch ${TS}`;
    await openPhotos(page, caption);

    const brokenBefore = brokenPhotos();
    // Deliberately bulky (~1200px of noise each, a few MB apiece): the small
    // fixtures the other cases use finish in under a second, which would make
    // "navigate away mid-batch" navigate away from an already-finished batch
    // and assert nothing.
    const heavy = Array.from({ length: 20 }, (_, i) =>
      fixtures.noise(`abandoned-${i}.png`, { width: 1200 + i, height: 1200 }),
    );
    await page.locator('#photo-file').setInputFiles(heavy);

    // Leave while uploads are genuinely in flight.
    await expect
      .poll(async () => page.locator('[data-testid="upload-queue"] progress').count(), {
        timeout: 30_000,
      })
      .toBeGreaterThan(0);
    await page.goto('/tasks');
    await expect(page).toHaveURL(/\/tasks$/);

    // Whatever finished before the navigation is a complete photo; nothing is
    // left half-written. Each upload is its own request, so an aborted one is
    // simply a request the server never completed — the assertion is that no
    // row exists without its bytes.
    expect(brokenPhotos()).toBe(brokenBefore);

    // Proof the navigation actually interrupted the batch rather than racing a
    // finished one: the queue uploads at most maxConcurrent (4) at a time, so
    // leaving mid-flight must leave some of the twenty unsent.
    const landed = photosSince(caption);
    expect(landed).toBeLessThan(20);
    const unreadable = psql(`
      SELECT count(*) FROM nestova.photo
       WHERE caption = '${caption}' AND (content_type IS NULL OR content_type = '');
    `).trim();
    expect(Number(unreadable)).toBe(0);
  });

  test('T-11.3.4 there is no cancel control; navigating away is the only stop', async ({ page }) => {
    await login(page, PERSONAS.owner);
    await openPhotos(page, `Cancel probe ${TS}`);
    await page.locator('#photo-file').setInputFiles(batchFiles('cancel', 6));
    await expect(page.locator('[data-testid="upload-queue"] li')).toHaveCount(6);

    // The queue offers Retry on a failed row and nothing else: no per-item
    // cancel, no "stop batch". The checklist asks for cancel mid-queue, and the
    // honest answer is that the feature does not exist — recorded here rather
    // than asserted as if it did. T-11.3.3 covers the workaround a member
    // actually has.
    const queue = page.locator('[data-testid="upload-queue"]');
    await expect(queue.getByRole('button', { name: /cancel|stop|abort/i })).toHaveCount(0);
    expect(
      await page.evaluate(() => {
        const el = document.querySelector('[data-testid="upload-dropzone"]');
        const data = el && window.Alpine ? window.Alpine.$data(el) : null;
        return data ? Object.keys(data).filter((k) => /cancel|abort/i.test(k)) : [];
      }),
    ).toEqual([]);
  });

  test('T-11.3.1b a real drop event enqueues the same way the picker does', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const caption = `Dropped ${TS}`;
    await openPhotos(page, caption);

    // The picker and the dropzone call one enqueueFiles(), but only this proves
    // the @drop handler is wired: a DataTransfer built in the page, dispatched
    // as a real drop.
    const files = batchFiles('dropped', 3).map((p) => ({
      name: p.split('/').pop(),
      bytes: Array.from(fs.readFileSync(p)),
    }));

    await page.evaluate(async (payload) => {
      const dt = new DataTransfer();
      for (const f of payload) {
        dt.items.add(new File([new Uint8Array(f.bytes)], f.name, { type: 'image/png' }));
      }
      document
        .querySelector('[data-testid="upload-dropzone"]')
        .dispatchEvent(new DragEvent('drop', { dataTransfer: dt, bubbles: true, cancelable: true }));
    }, files);

    await expect(page.locator('[data-testid="upload-queue"] li')).toHaveCount(3);
    await expect(page.locator('[data-testid="upload-summary"]')).toContainText('3 uploaded', {
      timeout: 60_000,
    });
    expect(photosSince(caption)).toBe(3);
  });
});
