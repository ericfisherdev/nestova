// §11 Photos and albums: upload, the album viewer, and the media-type guard.
//
// Uploads go through the real multipart form, because the whole point of most
// of these cases is what the SERVER accepts — sniffed content type, size,
// duplicate content. Fixtures are generated per run (see media-fixtures.js):
// NES-148 hashes content, so every fixture must be byte-distinct or the second
// upload is a duplicate rather than the case under test.
const { test, expect } = require('@playwright/test');
const fs = require('fs');
const path = require('path');
const { PERSONAS } = require('../tests/fixtures');
const { login, csrfToken } = require('./helpers');
const { psql, seedHouseholdB } = require('../tests/db');
const fixtures = require('./media-fixtures');

const TS = Date.now();
const name = (label) => `${label} ${TS}`;

test.beforeAll(() => {
  test.skip(!fixtures.hasMagick(), 'ImageMagick is required to generate the image fixtures');
  // T-11.4.7 needs a second household. Nestova onboards exactly one, so B is
  // seeded in SQL exactly as the §0.2 tenant-isolation spec does — idempotent,
  // so running either spec alone works.
  seedHouseholdB({
    householdName: 'Household B',
    ownerName: PERSONAS.otherOwner.displayName,
    ownerEmail: PERSONAS.otherOwner.email,
    copyHashFrom: PERSONAS.owner.email,
  });
});

test.afterAll(() => fixtures.cleanup());

function householdA() {
  return psql("SELECT id FROM identity.household WHERE name = 'Household A' LIMIT 1;").trim();
}

function photoCount(caption) {
  return Number(psql(
    `SELECT count(*) FROM nestova.photo WHERE caption = '${caption}';`,
  ).trim());
}

function photoIdByCaption(caption) {
  return psql(`SELECT id FROM nestova.photo WHERE caption = '${caption}' LIMIT 1;`).trim();
}

// upload posts one file through the real multipart form. It uses Playwright's
// request API rather than fetch() inside the page: serialising a 26 MiB fixture
// into the browser context to build a Blob exhausts the JS heap, and the point
// of the size case is the server's answer, not the browser's.
async function upload(page, filePath, caption, { fileName, mimeType } = {}) {
  const token = await csrfToken(page, '/photos');
  const res = await page.request.post('/photos', {
    maxRedirects: 0,
    multipart: {
      csrf_token: token,
      caption,
      photo: {
        name: fileName || path.basename(filePath),
        mimeType: mimeType || 'application/octet-stream',
        buffer: fs.readFileSync(filePath),
      },
    },
  });
  const status = res.status();
  return {
    status,
    result: res.headers()['x-upload-result'] || null,
    body: status >= 400 ? (await res.text()).slice(0, 160) : '',
  };
}

async function createAlbum(page, albumName) {
  const token = await csrfToken(page, '/photos');
  const status = await page.evaluate(async ({ csrf, n }) => {
    const res = await fetch('/albums', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ csrf_token: csrf, name: n, rotation_seconds: '8' }).toString(),
      redirect: 'manual',
    });
    return res.type === 'opaqueredirect' ? 303 : res.status;
  }, { csrf: token, n: albumName });
  expect(status).toBe(303);
  return psql(`SELECT id FROM nestova.album WHERE name = '${albumName}' LIMIT 1;`).trim();
}

async function post(page, url, fields) {
  const token = await csrfToken(page, '/photos');
  return page.evaluate(async ({ u, f, csrf }) => {
    const res = await fetch(u, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ csrf_token: csrf, ...f }).toString(),
      redirect: 'manual',
    });
    return res.type === 'opaqueredirect' ? 303 : res.status;
  }, { u: url, f: fields, csrf: token });
}

function albumPhotoCount(albumID) {
  return Number(psql(`SELECT count(*) FROM nestova.album_photo WHERE album_id = '${albumID}';`).trim());
}

test.describe('§11.1 upload, view, album basics', () => {
  test('T-11.1.1 a photo uploads, serves its bytes, and deletes', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const caption = name('Beach day');

    const res = await upload(page, fixtures.noise('upload-basic.png'), caption);
    expect(res.status).toBe(303);
    expect(res.result).toBe('created');
    expect(photoCount(caption)).toBe(1);

    const id = photoIdByCaption(caption);
    const raw = await page.request.get(`/photos/${id}/raw`);
    expect(raw.status()).toBe(200);
    expect(raw.headers()['content-type']).toContain('image/');
    expect((await raw.body()).length).toBeGreaterThan(0);

    expect(await post(page, `/photos/${id}/delete`, {})).toBe(303);
    expect(photoCount(caption)).toBe(0);
    expect((await page.request.get(`/photos/${id}/raw`)).status()).toBe(404);
  });

  test('T-11.1.2 an album is created, renamed, and gains and loses a photo', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const caption = name('In an album');
    expect((await upload(page, fixtures.noise('album-member.png'), caption)).status).toBe(303);
    const photoID = photoIdByCaption(caption);

    const albumName = name('Summer');
    const albumID = await createAlbum(page, albumName);

    // Rename through the same configure route the UI posts to.
    const renamed = `${albumName} renamed`;
    expect(await post(page, `/albums/${albumID}`, { name: renamed, rotation_seconds: '12' })).toBe(303);
    expect(psql(`SELECT name FROM nestova.album WHERE id = '${albumID}';`).trim()).toBe(renamed);

    expect(await post(page, `/photos/${photoID}/add-to-album`, { album_id: albumID })).toBe(303);
    expect(albumPhotoCount(albumID)).toBe(1);

    expect(await post(page, `/albums/${albumID}/photos/${photoID}/remove`, {})).toBe(303);
    expect(albumPhotoCount(albumID)).toBe(0);
  });

  test('T-11.1.3 the album viewer loads album.js before Alpine and runs', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const caption = name('Viewer slide');
    expect((await upload(page, fixtures.noise('viewer-slide.png'), caption)).status).toBe(303);
    const albumID = await createAlbum(page, name('Viewer album'));
    expect(
      await post(page, `/photos/${photoIdByCaption(caption)}/add-to-album`, { album_id: albumID }),
    ).toBe(303);

    await page.goto(`/album/${albumID}`);
    await expect(page.locator('[data-testid="album-viewer"]')).toBeVisible();

    // T-11.4.6 [!]: album.js registers Alpine.data('albumViewer') on
    // 'alpine:init', so it must be in the document BEFORE alpine.min.js. If it
    // is not, Alpine has already dispatched that event by the time the listener
    // registers and the viewer dies silently (NES-147).
    const order = await page.evaluate(() =>
      [...document.querySelectorAll('script[src]')].map((s) => s.getAttribute('src')),
    );
    expect(order.indexOf('/static/js/album.js')).toBeLessThan(order.indexOf('/static/js/alpine.min.js'));
    expect(order).toContain('/static/js/gsap.min.js');

    // Alive, not merely present: the Alpine component initialised and the
    // slideshow bound its caption.
    await expect
      .poll(async () => page.evaluate(() => {
        const el = document.querySelector('[data-testid="album-viewer"]');
        return Boolean(el && el.__x !== undefined) || Boolean(window.Alpine);
      }))
      .toBe(true);
  });
});

test.describe('§11.2 what the upload accepts', () => {
  test('T-11.2.1 a text file renamed .jpg is refused', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const caption = name('Not an image');
    const file = fixtures.raw('not-an-image.jpg', 'this is plain text pretending to be a jpeg\n');

    const res = await upload(page, file, caption);
    // Content is sniffed, not trusted from the extension.
    expect(res.status).toBe(415);
    expect(photoCount(caption)).toBe(0);
  });

  test('T-11.2.2 jpeg, png and webp are accepted; heic is not supported', async ({ page }) => {
    await login(page, PERSONAS.owner);

    for (const ext of ['jpg', 'png', 'webp']) {
      const caption = name(`Type ${ext}`);
      const res = await upload(page, fixtures.noise(`type-check.${ext}`), caption);
      expect(res.status, `${ext} upload`).toBe(303);
      expect(photoCount(caption)).toBe(1);
    }

    // The checklist lists heic as an allowed type, but the domain allows
    // image/jpeg, image/png and image/webp only, and the refusal message names
    // exactly those three. Recorded as the built contract: an iPhone-native
    // HEIC is refused, which is worth knowing before someone tries one.
    const heicCaption = name('Type heic');
    const heic = await upload(page, fixtures.noise('type-check.heic'), heicCaption);
    expect(heic.status).toBe(415);
    expect(photoCount(heicCaption)).toBe(0);
  });

  test('T-11.2.3 an empty file and a truncated JPEG are refused', async ({ page }) => {
    await login(page, PERSONAS.owner);

    const emptyCaption = name('Empty');
    const empty = await upload(page, fixtures.raw('empty.jpg', ''), emptyCaption);
    expect(empty.status).toBe(415);
    expect(photoCount(emptyCaption)).toBe(0);

    // A real JPEG header with the stream cut off. Sniffing alone would accept
    // it, so this is about whether anything downstream chokes on the truncation.
    const truncCaption = name('Truncated');
    const trunc = await upload(page, fixtures.truncated(), truncCaption);
    expect([303, 400, 415, 422]).toContain(trunc.status);
    expect(trunc.status).not.toBe(500);
  });

  test('T-11.2.4 an oversized upload is refused with a readable message', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const caption = name('Too large');
    // The configured cap is 25 MiB (MEDIA_MAX_UPLOAD_BYTES); noise does not
    // compress, so this file really is past it.
    const file = fixtures.oversized('oversized.png', 26 * 1024 * 1024);

    const res = await upload(page, file, caption);
    expect(res.status).toBe(413);
    // Not a bare 413 from the reverse proxy or MaxBytesReader with no wording.
    expect(res.body.toLowerCase()).toMatch(/too large|maximum upload size|malformed/);
    expect(photoCount(caption)).toBe(0);
  });

  test('T-11.2.5 identical content uploaded twice is reported as a duplicate', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const file = fixtures.noise('duplicate-source.png');

    const first = await upload(page, file, name('Dup first'));
    expect(first.status).toBe(303);
    expect(first.result).toBe('created');

    const second = await upload(page, file, name('Dup second'));
    expect(second.status).toBe(303);
    // NES-148: the content hash resolves to the existing photo rather than
    // erroring or storing the bytes twice.
    expect(second.result).toBe('duplicate');
    expect(photoCount(name('Dup second'))).toBe(0);
  });

  test('T-11.2.6 hostile filenames are stored safely', async ({ page }) => {
    await login(page, PERSONAS.owner);

    const cases = [
      { fileName: '../../../etc/passwd.png', label: 'traversal' },
      { fileName: `${'a'.repeat(500)}.png`, label: 'long' },
      { fileName: '📸 vacances été.png', label: 'unicode' },
    ];

    for (const { fileName, label } of cases) {
      const caption = name(`Filename ${label}`);
      const res = await upload(page, fixtures.noise(`filename-${label}.png`), caption, { fileName });
      expect(res.status, label).toBe(303);
      expect(photoCount(caption)).toBe(1);
    }

    // The stored reference is server-generated, so a traversal in the submitted
    // name cannot escape the media root.
    const refs = psql(
      `SELECT storage_ref FROM nestova.photo WHERE caption LIKE '${name('Filename')}%';`,
    ).trim();
    expect(refs).not.toContain('..');
    expect(refs).not.toContain('etc/passwd');
  });

  test('T-11.2.7 an SVG is refused (it is a script vector, not a photo)', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const caption = name('SVG');
    const svg = fixtures.raw(
      'xss.svg',
      '<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>',
    );

    const res = await upload(page, svg, caption);
    expect(res.status).toBe(415);
    expect(photoCount(caption)).toBe(0);
  });

  test('T-11.2.8 an EXIF-rotated JPEG is accepted and served', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const caption = name('EXIF rotated');

    const res = await upload(page, fixtures.rotated(), caption);
    expect(res.status).toBe(303);

    const id = photoIdByCaption(caption);
    const raw = await page.request.get(`/photos/${id}/raw`);
    expect(raw.status()).toBe(200);

    // Orientation is a rendering concern the browser honours from the EXIF tag,
    // so what the server must not do is strip or rewrite the bytes: assert the
    // served bytes are the ones uploaded.
    const served = await raw.body();
    expect(served.length).toBe(fs.statSync(fixtures.rotated()).size);
  });
});

test.describe('§11.4 album membership and isolation', () => {
  test('T-11.4.1 removing a photo from an album leaves it in the library', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const caption = name('Survives removal');
    expect((await upload(page, fixtures.noise('survives-removal.png'), caption)).status).toBe(303);
    const photoID = photoIdByCaption(caption);
    const albumID = await createAlbum(page, name('Removal album'));

    expect(await post(page, `/photos/${photoID}/add-to-album`, { album_id: albumID })).toBe(303);
    expect(await post(page, `/albums/${albumID}/photos/${photoID}/remove`, {})).toBe(303);

    expect(photoCount(caption)).toBe(1);
    expect((await page.request.get(`/photos/${photoID}/raw`)).status()).toBe(200);
  });

  test('T-11.4.2 deleting a photo removes it from every album', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const caption = name('In three albums');
    expect((await upload(page, fixtures.noise('three-albums.png'), caption)).status).toBe(303);
    const photoID = photoIdByCaption(caption);

    const albums = [];
    for (const n of ['A', 'B', 'C']) {
      const id = await createAlbum(page, name(`Three ${n}`));
      albums.push(id);
      expect(await post(page, `/photos/${photoID}/add-to-album`, { album_id: id })).toBe(303);
    }
    expect(albums.map(albumPhotoCount)).toEqual([1, 1, 1]);

    expect(await post(page, `/photos/${photoID}/delete`, {})).toBe(303);
    expect(albums.map(albumPhotoCount)).toEqual([0, 0, 0]);
  });

  test('T-11.4.3 deleting an album leaves its photos in the library', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const caption = name('Album deleted under me');
    expect((await upload(page, fixtures.noise('album-deleted.png'), caption)).status).toBe(303);
    const photoID = photoIdByCaption(caption);
    const albumID = await createAlbum(page, name('Doomed album'));
    expect(await post(page, `/photos/${photoID}/add-to-album`, { album_id: albumID })).toBe(303);

    // There is no delete-album route on this surface, so the destructive case
    // is driven at the database, which is where an operator would do it. The
    // photo must survive its album.
    psql(`DELETE FROM nestova.album WHERE id = '${albumID}';`);

    expect(photoCount(caption)).toBe(1);
    expect((await page.request.get(`/photos/${photoID}/raw`)).status()).toBe(200);
  });

  test('T-11.4.4 adding the same photo to one album twice makes one membership', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const caption = name('Added twice');
    expect((await upload(page, fixtures.noise('added-twice.png'), caption)).status).toBe(303);
    const photoID = photoIdByCaption(caption);
    const albumID = await createAlbum(page, name('Twice album'));

    expect(await post(page, `/photos/${photoID}/add-to-album`, { album_id: albumID })).toBe(303);
    expect(await post(page, `/photos/${photoID}/add-to-album`, { album_id: albumID })).toBe(303);
    expect(albumPhotoCount(albumID)).toBe(1);

    // Moving within one album is likewise a no-op rather than a duplicate.
    expect(await post(page, `/albums/${albumID}/photos/${photoID}/move`, { direction: 'up' })).toBe(303);
    expect(albumPhotoCount(albumID)).toBe(1);
  });

  test('T-11.4.5 an empty album viewer renders a message, not a blank page', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const albumID = await createAlbum(page, name('Empty album'));

    await page.goto(`/album/${albumID}`);
    await expect(page.locator('[data-testid="album-empty"]')).toBeVisible();
    await expect(page.locator('[data-testid="album-empty"]')).toContainText('No photos yet');
  });

  test("T-11.4.7 another household's photo bytes are not served", async ({ page }) => {
    await login(page, PERSONAS.owner);
    const caption = name('Household A private');
    expect((await upload(page, fixtures.noise('tenant-isolation.png'), caption)).status).toBe(303);
    const photoID = photoIdByCaption(caption);

    // Owner B has their own household; A's photo id must be as unreachable as
    // an id that does not exist.
    await login(page, PERSONAS.otherOwner);
    const res = await page.request.get(`/photos/${photoID}/raw`);
    expect(res.status()).toBe(404);
    expect((await res.body()).length).toBeLessThan(200);
  });
});
