// NES-75 / NES-124 Photos / Albums e2e specs.
//
// The Playwright config logs in once (auth.setup.js) and shares storageState,
// so every test below starts ALREADY AUTHENTICATED as the household owner.
// Test bodies just `page.goto('/photos')` — they never log in.
//
// The /photos page is server-rendered templ markup (web/components/photos.templ)
// with native <form method="post"> elements that also carry hx-post attributes,
// CSRF-verified server-side (internal/media/adapter/web.go). Album mutations
// are unchanged: fill the real inputs and click the real submit button, then
// assert on the re-rendered page (HX-Redirect for htmx, 303 otherwise).
//
// The upload form (NES-124) is different: it is a drag-and-drop queue driven
// by web/static/js/upload-queue.js (Alpine.data("uploadQueue")), with no
// submit button — selecting files via the hidden multi-file input (or a
// drop event) enqueues and immediately starts uploading each one through its
// own request. Playwright's setInputFiles() fires the input's native change
// event, which is exactly what the queue listens for, so no click is needed.
// The photo grid only refreshes once the whole batch drains (an htmx
// hx-trigger="photos-uploaded" on #photo-grid), so assertions on the grid
// must wait for that round trip rather than a synchronous form submit.
const { test, expect } = require('@playwright/test');
const fs = require('fs');
const os = require('os');
const path = require('path');

// A minimal valid 1x1 PNG (transparent pixel), base64-encoded. Written to a temp
// file at runtime and fed to the file <input> via setInputFiles. The upload form
// accepts image/jpeg, image/png, image/webp (see photos.templ accept=...).
const ONE_BY_ONE_PNG_BASE64 =
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==';

/**
 * Writes the embedded 1x1 PNG to a unique temp path and returns that path.
 * Each call uses a fresh filename so concurrent runs never collide (the suite
 * runs single-worker, but this keeps the helper side-effect free per call).
 */
function writeTempPng() {
  const filePath = path.join(os.tmpdir(), `nestova-e2e-${Date.now()}-${Math.random().toString(36).slice(2)}.png`);
  fs.writeFileSync(filePath, Buffer.from(ONE_BY_ONE_PNG_BASE64, 'base64'));
  return filePath;
}

test.describe('NES-75 / NES-124 Photos / Albums', () => {
  test('renders the photos page with upload dropzone and album/photo regions', async ({ page }) => {
    await page.goto('/photos');

    // Page heading establishes we landed on the right view.
    await expect(page.getByRole('heading', { name: 'Photos', exact: true })).toBeVisible();

    // The three management regions are always present regardless of data.
    await expect(page.getByRole('heading', { name: 'Upload photos' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Albums' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'All photos' })).toBeVisible();

    // The dropzone and its multipart upload form/multi-file input exist. The
    // file input is visually hidden (sr-only) behind a "choose files" label,
    // so it's asserted as attached/present rather than visible.
    await expect(page.getByTestId('upload-dropzone')).toBeVisible();
    const uploadForm = page.getByTestId('upload-form');
    await expect(uploadForm).toHaveAttribute('enctype', 'multipart/form-data');
    await expect(page.locator('#photo-file')).toHaveAttribute('multiple', '');
  });

  test('creating an album adds it to the album list', async ({ page }) => {
    await page.goto('/photos');

    // Unique name so the assertion is unambiguous across repeated runs.
    const albumName = `E2E Album ${Date.now()}`;

    // The "New album" create form is the one carrying the create-mode inputs
    // (#create-album-name / #create-rotation). Fill via those stable ids.
    await page.locator('#create-album-name').fill(albumName);
    await page.locator('#create-rotation').fill('5');

    // Submit the create form. Scope the button click to the form so we don't
    // accidentally hit the upload form's submit.
    const createForm = page.locator('form', { has: page.locator('#create-album-name') });
    await createForm.getByRole('button', { name: 'Create album' }).click();

    // After the redirect the new album appears in the album list. Be tolerant of
    // either the htmx redirect or a full 303 navigation by asserting on content.
    await expect(page.getByTestId('album-list')).toBeVisible();
    await expect(page.getByTestId('album-list').getByText(albumName, { exact: false })).toBeVisible();
  });

  test('uploading a photo makes it appear in the photo grid', async ({ page }) => {
    await page.goto('/photos');

    const pngPath = writeTempPng();
    const caption = `E2E Photo ${Date.now()}`;

    try {
      // Count any existing photo cards so the assertion works whether the grid
      // started empty (empty-state text) or already had photos from prior runs.
      const gridBefore = page.getByTestId('photo-grid');
      const beforeCount = (await gridBefore.count()) ? await gridBefore.locator('figure').count() : 0;

      // Fill the batch caption before selecting the file: the queue reads it
      // at upload time, which fires as soon as the file input's change event
      // enqueues this one file (no submit button in the new dropzone).
      await page.locator('#upload-caption').fill(caption);
      await page.locator('#photo-file').setInputFiles(pngPath);

      // The queue shows a batch summary once its single upload settles.
      await expect(page.getByTestId('upload-summary')).toHaveText(/1 uploaded/);

      // The grid only refreshes once the batch drains (a single htmx round
      // trip the queue triggers, not a per-file update) — the empty-state <p>
      // is replaced once a photo lands, and it holds at least one more card
      // than before.
      const gridAfter = page.getByTestId('photo-grid');
      await expect(gridAfter).toBeVisible();
      await expect(async () => {
        expect(await gridAfter.locator('figure').count()).toBeGreaterThan(beforeCount);
      }).toPass();

      // The caption we supplied is rendered on the new card.
      await expect(gridAfter.getByText(caption, { exact: false })).toBeVisible();
    } finally {
      fs.rmSync(pngPath, { force: true });
    }
  });

  test('owner can fetch an uploaded photo via /photos/{id}/raw (200)', async ({ page }) => {
    await page.goto('/photos');

    // Ensure at least one photo exists by uploading one (idempotent for this
    // assertion — we only need a raw URL to hit).
    const pngPath = writeTempPng();
    try {
      const caption = `Raw probe ${Date.now()}`;
      await page.locator('#upload-caption').fill(caption);
      await page.locator('#photo-file').setInputFiles(pngPath);
      await expect(page.getByTestId('upload-summary')).toHaveText(/1 uploaded/);

      const grid = page.getByTestId('photo-grid');
      await expect(grid).toBeVisible();

      // Bind to the card we just uploaded (by its unique caption) so the check
      // can't pass off an older photo if this upload failed. Each card's <img src>
      // is exactly "/photos/{id}/raw".
      const uploadedCard = grid.locator('figure').filter({ hasText: caption });
      await expect(uploadedCard).toBeVisible();
      const rawUrl = await uploadedCard.locator('img').first().getAttribute('src');
      expect(rawUrl).toBeTruthy();
      expect(rawUrl).toMatch(/^\/photos\/[^/]+\/raw$/);

      // GET the raw bytes through the browser context (carries the session
      // cookie). The owning household must get 200 with an image content type.
      const resp = await page.request.get(rawUrl);
      expect(resp.status()).toBe(200);
      const contentType = resp.headers()['content-type'] || '';
      expect(contentType).toContain('image/');
    } finally {
      fs.rmSync(pngPath, { force: true });
    }
  });

  // The three tests below exercise the queue's higher-risk states (skip,
  // duplicate, error+retry). They were written to the same style/selectors
  // as the tests above but, like the rest of this file, were NOT executed in
  // this environment — there is no running server/browser here to drive
  // them against. Each one's comments call out anything that is best-effort
  // (an assumption about server/timing behavior this environment couldn't
  // verify) rather than a fact confirmed by an actual run.

  test('a file that fails the client-side precheck is listed as skipped, not uploaded', async ({ page }) => {
    await page.goto('/photos');

    // A real text file, never claiming to be an image — data-accept rejects
    // it before any request is sent (web/static/js/upload-queue.js's
    // precheckReason), so this never touches the server at all.
    await page.locator('#photo-file').setInputFiles({
      name: 'notes.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from('not a photo'),
    });

    const skippedRow = page.getByTestId('upload-queue').locator('li').filter({ hasText: 'notes.txt' });
    await expect(skippedRow).toBeVisible();
    await expect(skippedRow).toContainText('unsupported type');

    // NES-124 CodeRabbit fix: a batch that is entirely precheck-skipped used
    // to never resolve a summary at all (nothing ever reached the
    // upload/settle path that used to be the only place computing one).
    // enqueueFiles() now also resolves immediately after a skip-only batch.
    await expect(page.getByTestId('upload-summary')).toHaveText(
      /0 uploaded, 0 duplicates skipped, 0 failed, 1 skipped/
    );
  });

  test('uploading the same photo twice reports the second as a duplicate', async ({ page }) => {
    await page.goto('/photos');
    const pngPath = writeTempPng();

    try {
      // This first upload's own outcome (created vs. already-duplicate) is
      // deliberately not asserted: this file's tests all share one
      // long-lived household with no per-test reset, and other tests upload
      // this exact same embedded 1x1 PNG, so whether THIS is the very first
      // time these bytes land depends on suite run order. What is guaranteed
      // regardless of that history is that re-uploading the identical bytes
      // a second time, immediately, within this same test, collides with the
      // first — that's the only thing asserted below.
      await page.locator('#photo-file').setInputFiles(pngPath);
      await expect(page.getByTestId('upload-summary')).not.toHaveText('');

      await page.locator('#photo-file').setInputFiles(pngPath);
      const secondRow = page.getByTestId('upload-queue').locator('li').last();
      await expect(secondRow).toContainText('duplicate');
    } finally {
      fs.rmSync(pngPath, { force: true });
    }
  });

  test('a file that fails server-side validation shows an error with a retry option', async ({ page }) => {
    await page.goto('/photos');

    // Best-effort: this relies on the server sniffing real file content
    // (internal/media/adapter, NES-123's DetectContentType) independent of
    // the extension/MIME type the client declared. Playwright's buffer
    // upload lets us claim image/png while sending plain-text bytes, so the
    // client-side precheck (which only looks at the declared type) passes
    // and the request actually reaches the server — which then rejects it
    // with 415, unlike the precheck-skip case above.
    await page.locator('#photo-file').setInputFiles({
      name: 'corrupt.png',
      mimeType: 'image/png',
      buffer: Buffer.from('this is not actually a png'),
    });

    const errorRow = page.getByTestId('upload-queue').locator('li').filter({ hasText: 'corrupt.png' });
    await expect(errorRow).toContainText('failed');
    await expect(errorRow.getByRole('button', { name: 'Retry' })).toBeVisible();
  });
});
