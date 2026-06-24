// NES-75 Photos / Albums e2e specs.
//
// The Playwright config logs in once (auth.setup.js) and shares storageState,
// so every test below starts ALREADY AUTHENTICATED as the household owner.
// Test bodies just `page.goto('/photos')` — they never log in.
//
// The /photos page is server-rendered templ markup (web/components/photos.templ)
// with native <form method="post"> elements that also carry hx-post attributes.
// Each form embeds a hidden csrf_token input; mutations are CSRF-verified
// server-side (internal/media/adapter/web.go) and respond with a redirect back
// to /photos (HX-Redirect for htmx, 303 otherwise). We submit by filling the
// real inputs and clicking the real submit button, so the genuine CSRF token and
// session cookie are always sent, then assert on the re-rendered page.
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

test.describe('NES-75 Photos / Albums', () => {
  test('renders the photos page with upload form and album/photo regions', async ({ page }) => {
    await page.goto('/photos');

    // Page heading establishes we landed on the right view.
    await expect(page.getByRole('heading', { name: 'Photos', exact: true })).toBeVisible();

    // The three management regions are always present regardless of data.
    await expect(page.getByRole('heading', { name: 'Upload a photo' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Albums' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'All photos' })).toBeVisible();

    // The multipart upload form and its file input exist.
    const uploadForm = page.getByTestId('upload-form');
    await expect(uploadForm).toBeVisible();
    await expect(uploadForm).toHaveAttribute('enctype', 'multipart/form-data');
    await expect(page.locator('#photo-file')).toBeVisible();
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

      await page.locator('#photo-file').setInputFiles(pngPath);
      await page.locator('#photo-caption').fill(caption);

      const uploadForm = page.getByTestId('upload-form');
      await uploadForm.getByRole('button', { name: 'Upload' }).click();

      // The grid now exists (the empty-state <p> is replaced once a photo lands)
      // and holds at least one more card than before.
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
      await page.locator('#photo-file').setInputFiles(pngPath);
      await page.locator('#photo-caption').fill(caption);
      await page.getByTestId('upload-form').getByRole('button', { name: 'Upload' }).click();

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
});
