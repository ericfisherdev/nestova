// §5.4 chore-proof photos (NES-119/NES-120): the before/after capture on a
// chore row, the photo-policy completion gate, and the EXIF rules the upload
// enforces — a DateTimeOriginal must exist, sit inside the freshness window
// (MEDIA_CHORE_PROOF_FRESHNESS_WINDOW, 60 minutes by default) in either
// direction, and an "after" may not predate the "before".
//
// Tasks and instances are seeded with SQL (the scheduler would take minutes);
// uploads go through the real multipart endpoint the row's capture form posts
// to, and completions through the row's own Done button or its POST route.
// Every chore here is assigned to the owner persona, who has no PIN enrolled,
// so the NES-166 gate lets the owner act without one.
const crypto = require('crypto');
const { test, expect } = require('@playwright/test');
const { PERSONAS, HOUSEHOLD_B } = require('../tests/fixtures');
const { psql, seedHouseholdB } = require('../tests/db');
const { login, postForm } = require('./helpers');
const {
  memberId, seedInstance, instanceRow, tasksToken, exifDate, minutesFromNow,
  spliceExif, noiseJpeg, proofJpeg, readBackDateTimeOriginal, uploadProof,
  proofPhotoCount, cleanup,
} = require('./helpers-chore-photos');

const MESSAGES = {
  beforeRequired: 'Take a before photo before marking this chore complete.',
  afterRequired: 'Take an after photo before marking this chore complete.',
  missingTimestamp: "we couldn't find a camera timestamp on that photo",
  stale: "that photo's timestamp is too old",
  afterPrecedesBefore: "the after photo's timestamp is earlier than the before photo",
};
const MIB = 1024 * 1024;

test.afterAll(() => cleanup());

function ownerId() {
  return memberId(PERSONAS.owner.displayName);
}

function seedOwnedChore(photoPolicy) {
  return seedInstance({ assignee: ownerId(), titlePrefix: `Proof ${photoPolicy}`, photoPolicy });
}

// signedIn logs the owner in and returns the /tasks CSRF token for API calls.
async function signedIn(page) {
  await login(page, PERSONAS.owner);
  return tasksToken(page);
}

// attachBoth uploads a valid before (5 min ago) and after (1 min ago) photo.
async function attachBoth(page, instanceId, csrfToken) {
  const before = await uploadProof(page, { instanceId, kind: 'before', bytes: proofJpeg(minutesFromNow(-5)), csrfToken });
  const after = await uploadProof(page, { instanceId, kind: 'after', bytes: proofJpeg(minutesFromNow(-1)), csrfToken });
  expect(before.status, `a fresh before photo must be accepted: ${before.body}`).toBe(303);
  expect(after.status, `a fresh after photo must be accepted: ${after.body}`).toBe(303);
}

// completeFromRow clicks the row's Done button and waits for the HTMX
// response, returning its status.
async function completeFromRow(page, instanceId) {
  const row = page.locator(`#task-${instanceId}`);
  const response = page.waitForResponse(
    (r) => r.request().method() === 'POST' && r.url().endsWith(`/tasks/${instanceId}/complete`),
  );
  await row.getByRole('button', { name: 'Done' }).click();
  return (await response).status();
}

test.describe('§5.4 chore photos', () => {
  test('fixture sanity: the hand-built EXIF segment carries a readable DateTimeOriginal', () => {
    const takenAt = minutesFromNow(-3);
    expect(readBackDateTimeOriginal(proofJpeg(takenAt))).toBe(exifDate(takenAt));
  });

  test('T-5.4.1 a before and an after photo are captured from the chore row, stored, and unlock completion', async ({ page }) => {
    const { instanceId } = seedOwnedChore('before_after');
    await login(page, PERSONAS.owner);
    await page.goto('/tasks');
    const row = page.locator(`#task-${instanceId}`);

    // The after slot is hidden until a before photo exists.
    await expect(row.getByTestId('proof-photo-slot-after')).toHaveCount(0);
    await row.getByTestId('proof-photo-input-before').setInputFiles({
      name: 'before.jpg', mimeType: 'image/jpeg', buffer: proofJpeg(minutesFromNow(-5)),
    });
    await expect(page.locator(`#task-${instanceId}`).getByTestId('proof-photo-image-before')).toBeVisible();

    await page.locator(`#task-${instanceId}`).getByTestId('proof-photo-input-after').setInputFiles({
      name: 'after.jpg', mimeType: 'image/jpeg', buffer: proofJpeg(minutesFromNow(-1)),
    });
    const afterImage = page.locator(`#task-${instanceId}`).getByTestId('proof-photo-image-after');
    await expect(afterImage).toBeVisible();

    expect(proofPhotoCount(instanceId, 'before'), 'one before photo stored').toBe(1);
    expect(proofPhotoCount(instanceId, 'after'), 'one after photo stored').toBe(1);

    // The stored bytes are served back as a JPEG with the EXIF segment scrubbed.
    const raw = await page.request.get(await afterImage.getAttribute('src'));
    expect(raw.status()).toBe(200);
    expect(raw.headers()['content-type']).toBe('image/jpeg');
    const served = await raw.body();
    expect(served.subarray(0, 2).toString('hex'), 'served bytes are a JPEG').toBe('ffd8');
    expect(served.includes(Buffer.from('Exif\0\0', 'binary')), 'EXIF must be stripped before storage').toBe(false);

    expect(await completeFromRow(page, instanceId)).toBe(200);
    await expect(page.locator(`#task-${instanceId}`).getByText('Completed')).toBeVisible();
    expect(instanceRow(instanceId).status).toBe('done');
  });

  test('T-5.4.2 completing a before_after chore with no before photo is refused with ErrBeforePhotoRequired', async ({ page }) => {
    const { instanceId } = seedOwnedChore('before_after');
    const csrf_token = await signedIn(page);
    await page.goto('/tasks');

    expect(await completeFromRow(page, instanceId), 'the Done click must be refused').toBe(422);
    const row = page.locator(`#task-${instanceId}`);
    await expect(row.getByRole('alert')).toHaveText(MESSAGES.beforeRequired);
    await expect(row.getByRole('button', { name: 'Done' })).toBeVisible();
    expect(instanceRow(instanceId).status).toBe('pending');

    // Non-HTMX callers get the same refusal as a plain 422.
    expect(await postForm(page, `/tasks/${instanceId}/complete`, { csrf_token })).toBe(422);

    // Sanity guard: with both photos attached, the same route accepts.
    await attachBoth(page, instanceId, csrf_token);
    expect(await postForm(page, `/tasks/${instanceId}/complete`, { csrf_token })).toBe(303);
    expect(instanceRow(instanceId).status).toBe('done');
  });

  test('T-5.4.3 completing without a required after photo is refused with ErrAfterPhotoRequired', async ({ page }) => {
    const afterOnly = seedOwnedChore('after_only').instanceId;
    const beforeAfter = seedOwnedChore('before_after').instanceId;
    const csrf_token = await signedIn(page);

    // after_only with nothing attached.
    await page.goto('/tasks');
    expect(await completeFromRow(page, afterOnly)).toBe(422);
    await expect(page.locator(`#task-${afterOnly}`).getByRole('alert')).toHaveText(MESSAGES.afterRequired);

    // before_after with only the before attached.
    const before = await uploadProof(page, {
      instanceId: beforeAfter, kind: 'before', bytes: proofJpeg(minutesFromNow(-5)), csrfToken: csrf_token,
    });
    expect(before.status).toBe(303);
    await page.goto('/tasks');
    expect(await completeFromRow(page, beforeAfter)).toBe(422);
    await expect(page.locator(`#task-${beforeAfter}`).getByRole('alert')).toHaveText(MESSAGES.afterRequired);
    expect(instanceRow(afterOnly).status).toBe('pending');
    expect(instanceRow(beforeAfter).status).toBe('pending');

    // Sanity guard: the after photo is all that was missing.
    const after = await uploadProof(page, {
      instanceId: afterOnly, kind: 'after', bytes: proofJpeg(minutesFromNow(-1)), csrfToken: csrf_token,
    });
    expect(after.status).toBe(303);
    expect(await postForm(page, `/tasks/${afterOnly}/complete`, { csrf_token })).toBe(303);
  });

  test('T-5.4.4 an after photo taken before the before photo is refused with ErrAfterPrecedesBefore', async ({ page }) => {
    const { instanceId } = seedOwnedChore('before_after');
    const csrfToken = await signedIn(page);

    const before = await uploadProof(page, { instanceId, kind: 'before', bytes: proofJpeg(minutesFromNow(-5)), csrfToken });
    expect(before.status).toBe(303);

    const earlier = await uploadProof(page, { instanceId, kind: 'after', bytes: proofJpeg(minutesFromNow(-20)), csrfToken });
    expect(earlier.status).toBe(422);
    expect(earlier.body).toContain(MESSAGES.afterPrecedesBefore);
    expect(proofPhotoCount(instanceId, 'after'), 'the out-of-order after must not be stored').toBe(0);

    // Sanity guard: a correctly ordered after is accepted.
    const later = await uploadProof(page, { instanceId, kind: 'after', bytes: proofJpeg(minutesFromNow(-1)), csrfToken });
    expect(later.status).toBe(303);
    expect(proofPhotoCount(instanceId, 'after')).toBe(1);
  });

  test('T-5.4.5 a JPEG whose EXIF has no DateTimeOriginal is refused with ErrPhotoMissingTimestamp', async ({ page }) => {
    const { instanceId } = seedOwnedChore('after_only');
    const csrfToken = await signedIn(page);

    // A plain DateTime (file-modified) tag is deliberately not accepted as a
    // capture time — only DateTimeOriginal counts.
    const edited = spliceExif(noiseJpeg(), { dateTime: exifDate(minutesFromNow(-1)) });
    const refused = await uploadProof(page, { instanceId, kind: 'after', bytes: edited, csrfToken });
    expect(refused.status).toBe(422);
    expect(refused.body).toContain(MESSAGES.missingTimestamp);
    expect(proofPhotoCount(instanceId)).toBe(0);

    // Sanity guard: the same kind of file with DateTimeOriginal is accepted.
    const ok = await uploadProof(page, { instanceId, kind: 'after', bytes: proofJpeg(minutesFromNow(-1)), csrfToken });
    expect(ok.status).toBe(303);
    expect(proofPhotoCount(instanceId)).toBe(1);
  });

  test('T-5.4.6 a photo older than the 1-hour freshness window is refused with ErrPhotoStale', async ({ page }) => {
    const { instanceId } = seedOwnedChore('after_only');
    const csrfToken = await signedIn(page);

    const stale = await uploadProof(page, { instanceId, kind: 'after', bytes: proofJpeg(minutesFromNow(-120)), csrfToken });
    expect(stale.status).toBe(422);
    expect(stale.body).toContain(MESSAGES.stale);
    const justOutside = await uploadProof(page, { instanceId, kind: 'after', bytes: proofJpeg(minutesFromNow(-62)), csrfToken });
    expect(justOutside.status, 'two minutes past the window is still stale').toBe(422);
    expect(proofPhotoCount(instanceId)).toBe(0);

    // Sanity guard: inside the window (50 minutes old) is accepted.
    const inside = await uploadProof(page, { instanceId, kind: 'after', bytes: proofJpeg(minutesFromNow(-50)), csrfToken });
    expect(inside.status).toBe(303);
    expect(proofPhotoCount(instanceId)).toBe(1);
  });

  test('T-5.4.7 an EXIF timestamp in the future is refused', async ({ page }) => {
    const { instanceId } = seedOwnedChore('after_only');
    const csrfToken = await signedIn(page);

    // The window applies in both directions, so a camera clock set ahead is
    // refused the same way as a stale photo.
    const future = await uploadProof(page, { instanceId, kind: 'after', bytes: proofJpeg(minutesFromNow(120)), csrfToken });
    expect(future.status).toBe(422);
    expect(future.body).toContain(MESSAGES.stale);
    const farFuture = await uploadProof(page, {
      instanceId, kind: 'after', bytes: proofJpeg(new Date(Date.now() + 365 * 24 * 3600_000)), csrfToken,
    });
    expect(farFuture.status).toBe(422);
    expect(proofPhotoCount(instanceId)).toBe(0);

    // Sanity guard: a few minutes of clock skew ahead is tolerated.
    const skewed = await uploadProof(page, { instanceId, kind: 'after', bytes: proofJpeg(minutesFromNow(5)), csrfToken });
    expect(skewed.status).toBe(303);
  });

  test('T-5.4.8 a stripped-EXIF screenshot is refused with a clear reason', async ({ page }) => {
    const { instanceId } = seedOwnedChore('after_only');
    const csrfToken = await signedIn(page);

    // Real screenshots, taken by the browser: no EXIF at all.
    for (const [type, mimeType] of [['jpeg', 'image/jpeg'], ['png', 'image/png']]) {
      const shot = await page.screenshot({ type });
      const refused = await uploadProof(page, {
        instanceId, kind: 'after', bytes: shot, csrfToken, fileName: `screenshot.${type}`, mimeType,
      });
      expect(refused.status, `a ${type} screenshot must be refused`).toBe(422);
      expect(refused.body).toContain(MESSAGES.missingTimestamp);
      expect(refused.body, 'the reason must tell the member what to do').toContain('please take a new photo');
    }
    expect(proofPhotoCount(instanceId)).toBe(0);

    const ok = await uploadProof(page, { instanceId, kind: 'after', bytes: proofJpeg(minutesFromNow(-1)), csrfToken });
    expect(ok.status).toBe(303);
  });

  test('T-5.4.9 a non-image renamed .jpg is refused (as ErrPhotoMissingTimestamp, by design) and never stored', async ({ page }) => {
    // CHECKLIST WRONG: the checklist expects ErrUnsupportedMediaType (415).
    // ChoreProofPhotoService.Upload checks the EXIF capture time BEFORE the
    // store sniffs the content type, and documents that a non-JPEG "never has
    // one extracted and always fails this check" — so a renamed text file is
    // refused with 422 ErrPhotoMissingTimestamp instead. The refusal is real;
    // only the reason differs.
    const { instanceId } = seedOwnedChore('after_only');
    const csrfToken = await signedIn(page);

    const text = Buffer.from('this is plain text wearing a .jpg extension\n'.repeat(20));
    const refused = await uploadProof(page, { instanceId, kind: 'after', bytes: text, csrfToken, fileName: 'notes.jpg' });
    expect(refused.status).toBe(422);
    expect(refused.body).toContain(MESSAGES.missingTimestamp);

    // The one way past the timestamp check with non-image bytes: JPEG magic
    // plus a fresh EXIF segment, followed by text instead of image data. The
    // store's decode refuses it as ErrInvalidPhoto (400), not a server error.
    // ErrUnsupportedMediaType is unreachable here: anything starting with the
    // JPEG magic sniffs as image/jpeg, and anything else stops at the
    // timestamp check.
    const disguised = spliceExif(Buffer.concat([Buffer.from([0xff, 0xd8]), text]), {
      dateTimeOriginal: exifDate(minutesFromNow(-1)),
    });
    const probe = await uploadProof(page, { instanceId, kind: 'after', bytes: disguised, csrfToken, fileName: 'notes.jpg' });
    expect(probe.status, 'a disguised non-image must be a client error, not a 500').toBe(400);
    expect(proofPhotoCount(instanceId)).toBe(0);

    const ok = await uploadProof(page, { instanceId, kind: 'after', bytes: proofJpeg(minutesFromNow(-1)), csrfToken });
    expect(ok.status).toBe(303);
  });

  test('T-5.4.10 a photo over the upload cap is refused with 413 (the cap is MEDIA_MAX_UPLOAD_BYTES, 25 MiB, not 10 MiB)', async ({ page }) => {
    // CHECKLIST WRONG on the number: the checklist says a 10 MiB cap. The
    // chore-proof cap is MEDIA_MAX_UPLOAD_BYTES (config.go default 25 MiB),
    // the same operator limit the album path uses. A 12 MiB photo is accepted
    // and a 26 MiB one is refused with ErrPhotoTooLarge / 413.
    test.setTimeout(90_000);
    const { instanceId } = seedOwnedChore('after_only');
    const csrfToken = await signedIn(page);

    // A valid, fresh JPEG padded after its EOI marker: decoders stop at EOI,
    // so the bytes are still a real photo, just a large one.
    const padded = (bytes) => Buffer.concat([proofJpeg(minutesFromNow(-1)), crypto.randomBytes(bytes)]);

    const huge = await uploadProof(page, { instanceId, kind: 'after', bytes: padded(26 * MIB), csrfToken });
    expect(huge.status).toBe(413);
    expect(huge.body).toContain('exceeds the maximum');
    expect(proofPhotoCount(instanceId)).toBe(0);

    const twelve = await uploadProof(page, { instanceId, kind: 'after', bytes: padded(12 * MIB), csrfToken });
    expect(twelve.status, `a 12 MiB photo is inside the real cap: ${twelve.body}`).toBe(303);
    expect(proofPhotoCount(instanceId)).toBe(1);
  });

  test("T-5.4.11 another chore's photos cannot satisfy this chore, and another household's photo or chore is refused", async ({ page }) => {
    const withPhotos = seedOwnedChore('before_after').instanceId;
    const withoutPhotos = seedOwnedChore('before_after').instanceId;
    const csrf_token = await signedIn(page);
    await attachBoth(page, withPhotos, csrf_token);

    // Photos bind to the instance in the upload path; there is no field that
    // lets a completion point at a different chore's photo.
    expect(await postForm(page, `/tasks/${withoutPhotos}/complete`, { csrf_token })).toBe(422);
    expect(instanceRow(withoutPhotos).status).toBe('pending');

    // Household B: uploading onto its chore, and reading its photo, both 404.
    const { householdId: householdB } = seedHouseholdB({
      householdName: HOUSEHOLD_B,
      ownerName: PERSONAS.otherOwner.displayName,
      ownerEmail: PERSONAS.otherOwner.email,
      copyHashFrom: PERSONAS.owner.email,
    });
    const foreign = seedInstance({ titlePrefix: 'Foreign proof', photoPolicy: 'after_only', householdId: householdB });
    const intoForeign = await uploadProof(page, {
      instanceId: foreign.instanceId, kind: 'after', bytes: proofJpeg(minutesFromNow(-1)), csrfToken: csrf_token,
    });
    expect(intoForeign.status).toBe(404);
    expect(proofPhotoCount(foreign.instanceId)).toBe(0);

    const foreignPhotoId = psql(`
      INSERT INTO nestova.task_instance_photo
        (id, household_id, task_instance_id, kind, storage_ref, content_sha256, size_bytes, content_type, taken_at)
      VALUES (gen_random_uuid(), '${householdB}', '${foreign.instanceId}', 'after', 'chore-proof/never-served.jpg',
              encode(sha256(gen_random_uuid()::text::bytea), 'hex'), 10, 'image/jpeg', now())
      RETURNING id;`).trim();
    expect((await page.request.get(`/tasks/photos/${foreignPhotoId}/raw`)).status()).toBe(404);

    // Sanity guard: this household's own photo is served, and its own chore
    // completes.
    const ownPhotoId = psql(
      `SELECT id FROM nestova.task_instance_photo WHERE task_instance_id = '${withPhotos}' AND kind = 'after';`,
    ).trim();
    expect((await page.request.get(`/tasks/photos/${ownPhotoId}/raw`)).status()).toBe(200);
    expect(await postForm(page, `/tasks/${withPhotos}/complete`, { csrf_token })).toBe(303);
  });
});
