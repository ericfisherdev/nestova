// §0.2 tenant isolation (IDOR) — the highest-value section in the checklist.
//
// Household B is SQL-seeded (see tests/db.js) because Nestova provisions a
// single household through the UI. The schema is household-scoped everywhere,
// and NES-165 was exactly this bug class — a cross-household write that the
// composite FK did not catch — so these are worth running even though the
// product does not expose multi-household onboarding.
const { test, expect } = require('@playwright/test');
const { PERSONAS, PASSWORD } = require('../tests/fixtures');
const { login, csrfToken, postForm } = require('./helpers');
const { psql, seedHouseholdB } = require('../tests/db');
const fixtures = require('./media-fixtures');
const {
  householdId, memberIdByEmail, signedIn, seedInstance, postPairs, uploadPhoto,
  probeForeignId, expectRefusedLikeMissing,
} = require('./helpers-isolation');

const OWNER_B = { email: 'owner@other.local', password: PASSWORD, displayName: 'Owner B' };

let b;

// One run's fixtures share a suffix so a re-run never matches rows an earlier
// run left behind.
const TS = Date.now();
const unique = (label) => `${label} ${TS}`;

test.beforeAll(() => {
  b = seedHouseholdB({
    householdName: 'Household B',
    ownerName: OWNER_B.displayName,
    ownerEmail: OWNER_B.email,
    copyHashFrom: PERSONAS.owner.email,
  });
});

// Creates a chore in household A and returns its instance id, so B can try to
// act on it. Seeded directly: instances are materialised by a background
// scheduler, so a chore created through the form has no actionable row yet.
function seedInstanceInA() {
  const householdA = psql(
    "SELECT id FROM identity.household WHERE name = 'Household A' LIMIT 1;",
  ).trim();
  const ownerA = psql(
    `SELECT id FROM identity.member WHERE household_id = '${householdA}' AND role = 'owner' LIMIT 1;`,
  ).trim();

  const taskId = psql(`
    INSERT INTO nestova.recurring_task
      (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days, active, created_at, updated_at)
    VALUES (gen_random_uuid(), '${householdA}', 'Isolation probe chore', 'chore',
            '{"Freq":"daily","Interval":1,"Anchor":"2026-08-25T00:00:00Z","ByWeekday":null}'::jsonb,
            'claimable', 5, 0, true, now(), now())
    RETURNING id;
  `).trim();

  const instanceId = psql(`
    INSERT INTO nestova.task_instance
      (id, recurring_task_id, household_id, assignee_id, due_on, status, kind, created_at, updated_at)
    VALUES (gen_random_uuid(), '${taskId}', '${householdA}', '${ownerA}', current_date, 'pending', 'scheduled', now(), now())
    RETURNING id;
  `).trim();

  return { householdA, ownerA, taskId, instanceId };
}

test.describe('§0.2 tenant isolation', () => {
  test('sanity: the seeded household B owner can sign in', async ({ page }) => {
    await login(page, OWNER_B);
    expect(b.householdId, 'household B should have been seeded').toBeTruthy();
  });

  test('T-0.2.1 household B cannot act on household A chores', async ({ page }) => {
    const a = seedInstanceInA();
    await login(page, OWNER_B);
    await page.goto('/tasks');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();

    const results = {};
    for (const action of ['complete', 'skip', 'claim']) {
      results[action] = await postForm(page, `/tasks/${a.instanceId}/${action}`, { csrf_token });
    }
    const leaked = Object.entries(results).filter(([, s]) => s < 400);
    expect(leaked, "household B actions on A's instance that were not refused").toEqual([]);

    const status = psql(
      `SELECT status FROM nestova.task_instance WHERE id = '${a.instanceId}';`,
    ).trim();
    expect(status, "A's instance must be untouched").toBe('pending');
  });

  test('T-0.2.9 household B cannot set a PIN on a household A member', async ({ page }) => {
    const a = seedInstanceInA();
    await login(page, OWNER_B);
    await page.goto('/settings');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();

    const status = await postForm(page, `/settings/members/${a.ownerA}/pin`, { csrf_token, pin: '9999' });
    expect(status, "B must not set a PIN on A's member").toBeGreaterThanOrEqual(400);

    const pins = psql(
      `SELECT count(*) FROM identity.member_pin WHERE member_id = '${a.ownerA}';`,
    ).trim();
    expect(pins, "no PIN row may exist for A's member").toBe('0');
  });

  test('T-0.2.11 a foreign id is indistinguishable from a missing one', async ({ page }) => {
    const a = seedInstanceInA();
    await login(page, OWNER_B);
    await page.goto('/tasks');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();

    const foreign = await postForm(page, `/tasks/${a.instanceId}/complete`, { csrf_token });
    const missing = await postForm(page, '/tasks/00000000-0000-0000-0000-000000000000/complete', { csrf_token });

    expect({ foreign, missing },
      'a foreign id and an unknown id must answer identically, or the response enumerates ids',
    ).toEqual({ foreign: missing, missing });
  });
});

// The per-feature IDOR sweep. Every test follows one shape: household A
// creates the object through its own routes, household B posts a complete,
// valid payload against A's id and is refused exactly as for a missing id, A's
// row is proven unchanged, and then a legitimate actor performs the SAME
// action on the SAME id and is accepted — the sanity guard that proves the
// refusal was about tenancy, not a malformed payload or a dead route.
test.describe('§0.2 tenant isolation — feature routes', () => {
  test('T-0.2.2 household B cannot propose, accept, decline or cancel household A trades', async ({ page, browser }) => {
    const householdA = householdId('Household A');
    const ownerA = memberIdByEmail(PERSONAS.owner.email);
    const childA = memberIdByEmail(PERSONAS.child.email);
    const offered = seedInstance(householdA, { assignee: ownerA });
    const requested = seedInstance(householdA, { assignee: childA });

    const a = await signedIn(browser, PERSONAS.owner);
    const aToken = await csrfToken(a.page, '/tasks');
    expect(
      await postForm(a.page, '/trades', { csrf_token: aToken, offered_instance_id: offered, requested_instance_id: requested }),
      "sanity: A's own proposal must be accepted",
    ).toBe(303);
    const tradeId = psql(`SELECT id FROM nestova.chore_trade WHERE offered_instance_id = '${offered}' LIMIT 1;`).trim();
    expect(tradeId, "A's trade should exist").toBeTruthy();

    await login(page, OWNER_B);
    const csrf_token = await csrfToken(page, '/tasks');
    for (const action of ['accept', 'decline', 'cancel']) {
      expectRefusedLikeMissing(`${action} A's trade`,
        await probeForeignId(page, (id) => `/trades/${id}/${action}`, tradeId, { csrf_token }));
    }
    expect(psql(`SELECT status FROM nestova.chore_trade WHERE id = '${tradeId}';`).trim(),
      "A's trade must still be pending").toBe('proposed');

    // Propose: B offers its own chore for one of A's. The requested id lives in
    // the form, not the path, so the probe is built by hand.
    const ownChore = seedInstance(b.householdId, { assignee: b.ownerId });
    const foreign = await postForm(page, '/trades', { csrf_token, offered_instance_id: ownChore, requested_instance_id: requested });
    const missing = await postForm(page, '/trades', { csrf_token, offered_instance_id: ownChore, requested_instance_id: '00000000-0000-0000-0000-000000000000' });
    expectRefusedLikeMissing("propose a trade for A's chore", { foreign, missing });
    expect(psql(`SELECT count(*) FROM nestova.chore_trade WHERE offered_instance_id = '${ownChore}';`).trim(),
      'no cross-household trade row may exist').toBe('0');

    // Sanity guard: A's proposer can still cancel the very trade B could not.
    expect(await postForm(a.page, `/trades/${tradeId}/cancel`, { csrf_token: aToken }),
      "sanity: A's proposer cancels A's trade").toBe(303);
    await a.context.close();
  });

  test('T-0.2.3 household B cannot redeem A rewards or fulfil, deny or cancel A redemptions', async ({ page, browser }) => {
    const householdA = householdId('Household A');
    const childA = memberIdByEmail(PERSONAS.child.email);
    const rewardA = psql(`
      INSERT INTO nestova.reward (id, household_id, name, cost_points, active, quantity_available, created_at, updated_at)
      VALUES (gen_random_uuid(), '${householdA}', '${unique('A reward')}', 5, true, 10, now(), now()) RETURNING id;`).trim();
    const redemptionA = psql(`
      INSERT INTO nestova.reward_redemption (id, household_id, reward_id, member_id, status, created_at, updated_at)
      VALUES (gen_random_uuid(), '${householdA}', '${rewardA}', '${childA}', 'pending', now(), now()) RETURNING id;`).trim();

    // B gets its own affordable reward and the points for it, so a refusal of
    // A's equally priced reward cannot be "insufficient points".
    const rewardB = psql(`
      INSERT INTO nestova.reward (id, household_id, name, cost_points, active, quantity_available, created_at, updated_at)
      VALUES (gen_random_uuid(), '${b.householdId}', '${unique('B reward')}', 5, true, 10, now(), now()) RETURNING id;`).trim();
    psql(`
      INSERT INTO nestova.point_ledger (id, household_id, member_id, source_type, source_id, points, created_at)
      VALUES (gen_random_uuid(), '${b.householdId}', '${b.ownerId}', 'adjustment', gen_random_uuid(), 50, now());`);

    await login(page, OWNER_B);
    const csrf_token = await csrfToken(page, '/rewards');
    expect(await postForm(page, `/rewards/${rewardB}/redeem`, { csrf_token }), "sanity: B redeems B's own reward").toBe(303);

    expectRefusedLikeMissing("redeem A's reward",
      await probeForeignId(page, (id) => `/rewards/${id}/redeem`, rewardA, { csrf_token }));
    expect(psql(`SELECT count(*) FROM nestova.reward_redemption WHERE reward_id = '${rewardA}' AND member_id = '${b.ownerId}';`).trim(),
      "B must hold no redemption of A's reward").toBe('0');

    for (const route of [
      (id) => `/admin/rewards/redemptions/${id}/fulfill`,
      (id) => `/admin/rewards/redemptions/${id}/deny`,
      (id) => `/rewards/redemptions/${id}/cancel`,
    ]) {
      expectRefusedLikeMissing(route('{id}'), await probeForeignId(page, route, redemptionA, { csrf_token, reason: 'probe' }));
    }
    expect(psql(`SELECT status FROM nestova.reward_redemption WHERE id = '${redemptionA}';`).trim(),
      "A's redemption must still be pending").toBe('pending');

    // Sanity guard: A's owner fulfils the same redemption B could not.
    const a = await signedIn(browser, PERSONAS.owner);
    const aToken = await csrfToken(a.page, '/admin/rewards');
    expect(await postForm(a.page, `/admin/rewards/redemptions/${redemptionA}/fulfill`, { csrf_token: aToken }),
      "sanity: A's owner fulfils A's redemption").toBe(303);
    await a.context.close();
  });

  test('T-0.2.4 household B cannot view, delete or album-add household A photos', async ({ page, browser }) => {
    test.skip(!fixtures.hasMagick(), 'ImageMagick is required to generate the image fixtures');
    const captionA = unique('A private photo');
    const albumAName = unique('A album for photos');
    const a = await signedIn(browser, PERSONAS.owner);
    expect(await uploadPhoto(a.page, fixtures.noise(`iso-a-${TS}.png`), captionA)).toBe(303);
    const aToken = await csrfToken(a.page, '/photos');
    expect(await postForm(a.page, '/albums', { csrf_token: aToken, name: albumAName, rotation_seconds: '8' })).toBe(303);
    const photoA = psql(`SELECT id FROM nestova.photo WHERE caption = '${captionA}' LIMIT 1;`).trim();
    const albumA = psql(`SELECT id FROM nestova.album WHERE name = '${albumAName}' LIMIT 1;`).trim();

    await login(page, OWNER_B);
    const captionB = unique('B own photo');
    const albumBName = unique('B album');
    expect(await uploadPhoto(page, fixtures.noise(`iso-b-${TS}.png`), captionB)).toBe(303);
    const csrf_token = await csrfToken(page, '/photos');
    expect(await postForm(page, '/albums', { csrf_token, name: albumBName, rotation_seconds: '8' })).toBe(303);
    const photoB = psql(`SELECT id FROM nestova.photo WHERE caption = '${captionB}' LIMIT 1;`).trim();
    const albumB = psql(`SELECT id FROM nestova.album WHERE name = '${albumBName}' LIMIT 1;`).trim();

    const raw = await page.request.get(`/photos/${photoA}/raw`);
    const rawMissing = await page.request.get('/photos/00000000-0000-0000-0000-000000000000/raw');
    expect(raw.status(), "A's photo bytes must not be served to B").toBe(404);
    expect(raw.status(), 'a foreign photo must answer like a missing one').toBe(rawMissing.status());

    expectRefusedLikeMissing("delete A's photo",
      await probeForeignId(page, (id) => `/photos/${id}/delete`, photoA, { csrf_token }));
    expectRefusedLikeMissing("add A's photo to B's album",
      await probeForeignId(page, (id) => `/photos/${id}/add-to-album`, photoA, { csrf_token, album_id: albumB }));
    // The album id lives in the form here, so the foreign/missing pair is by hand.
    const intoForeignAlbum = await postForm(page, `/photos/${photoB}/add-to-album`, { csrf_token, album_id: albumA });
    const intoMissingAlbum = await postForm(page, `/photos/${photoB}/add-to-album`, { csrf_token, album_id: '00000000-0000-0000-0000-000000000000' });
    expectRefusedLikeMissing("add B's photo to A's album", { foreign: intoForeignAlbum, missing: intoMissingAlbum });

    expect(psql(`SELECT count(*) FROM nestova.photo WHERE id = '${photoA}';`).trim(), "A's photo must survive").toBe('1');
    expect(psql(`SELECT count(*) FROM nestova.album_photo WHERE photo_id = '${photoA}' OR album_id = '${albumA}';`).trim(),
      'no cross-household album membership may exist').toBe('0');

    // Sanity guard: the same add-to-album payload works inside B's own household.
    expect(await postForm(page, `/photos/${photoB}/add-to-album`, { csrf_token, album_id: albumB }),
      "sanity: B adds B's photo to B's album").toBe(303);
    expect((await a.page.request.get(`/photos/${photoA}/raw`)).status(), "sanity: A can fetch A's photo").toBe(200);
    await a.context.close();
  });

  test('T-0.2.5 household B cannot rename household A albums or move or remove their photos', async ({ page, browser }) => {
    test.skip(!fixtures.hasMagick(), 'ImageMagick is required to generate the image fixtures');
    const albumName = unique('A album');
    const a = await signedIn(browser, PERSONAS.owner);
    const captions = [unique('A album photo one'), unique('A album photo two')];
    for (const [i, caption] of captions.entries()) {
      expect(await uploadPhoto(a.page, fixtures.noise(`iso-album-${i}-${TS}.png`), caption)).toBe(303);
    }
    const aToken = await csrfToken(a.page, '/photos');
    expect(await postForm(a.page, '/albums', { csrf_token: aToken, name: albumName, rotation_seconds: '8' })).toBe(303);
    const albumA = psql(`SELECT id FROM nestova.album WHERE name = '${albumName}' LIMIT 1;`).trim();
    const photos = captions.map((c) => psql(`SELECT id FROM nestova.photo WHERE caption = '${c}' LIMIT 1;`).trim());
    for (const photo of photos) {
      expect(await postForm(a.page, `/photos/${photo}/add-to-album`, { csrf_token: aToken, album_id: albumA })).toBe(303);
    }
    const positions = () => psql(
      `SELECT string_agg(photo_id::text, ',' ORDER BY position) FROM nestova.album_photo WHERE album_id = '${albumA}';`,
    ).trim();
    const before = positions();

    await login(page, OWNER_B);
    const csrf_token = await csrfToken(page, '/photos');
    expectRefusedLikeMissing("rename A's album",
      await probeForeignId(page, (id) => `/albums/${id}`, albumA, { csrf_token, name: 'Hijacked', rotation_seconds: '8' }));
    expectRefusedLikeMissing("move a photo in A's album",
      await probeForeignId(page, (id) => `/albums/${id}/photos/${photos[1]}/move`, albumA, { csrf_token, direction: 'up' }));
    expectRefusedLikeMissing("remove a photo from A's album",
      await probeForeignId(page, (id) => `/albums/${id}/photos/${photos[0]}/remove`, albumA, { csrf_token }));

    expect(psql(`SELECT name FROM nestova.album WHERE id = '${albumA}';`).trim(), "A's album keeps its name").toBe(albumName);
    expect(positions(), "A's album keeps its photos in order").toBe(before);

    // Sanity guard: A's owner performs the same three actions on the same ids.
    expect(await postForm(a.page, `/albums/${albumA}`, { csrf_token: aToken, name: `${albumName} renamed`, rotation_seconds: '8' })).toBe(303);
    expect(await postForm(a.page, `/albums/${albumA}/photos/${photos[1]}/move`, { csrf_token: aToken, direction: 'up' })).toBe(303);
    expect(positions(), "sanity: A's move reorders A's album").toBe([photos[1], photos[0]].join(','));
    expect(await postForm(a.page, `/albums/${albumA}/photos/${photos[0]}/remove`, { csrf_token: aToken })).toBe(303);
    await a.context.close();
  });

  test('T-0.2.6 household B cannot adjust or consume A pantry items or change A shopping status', async ({ page, browser }) => {
    const pantryName = unique('A pantry flour');
    const shoppingName = unique('A shopping soap');
    const a = await signedIn(browser, PERSONAS.owner);
    const aToken = await csrfToken(a.page, '/groceries');
    expect(await postForm(a.page, '/groceries/pantry', { csrf_token: aToken, name: pantryName, amount: '500', unit: 'g' })).toBe(303);
    expect(await postForm(a.page, '/groceries/shopping', { csrf_token: aToken, name: shoppingName, amount: '1', unit: 'count' })).toBe(303);
    const householdA = householdId('Household A');
    const pantryA = psql(`
      SELECT p.id FROM nestova.pantry_item p JOIN nestova.ingredient i ON i.id = p.ingredient_id
       WHERE p.household_id = '${householdA}' AND lower(i.canonical_name) = lower('${pantryName}') LIMIT 1;`).trim();
    const shoppingA = psql(`SELECT id FROM nestova.shopping_list_item WHERE name = '${shoppingName}' LIMIT 1;`).trim();
    const pantryQty = () => psql(`SELECT quantity::float8 || ' ' || unit FROM nestova.pantry_item WHERE id = '${pantryA}';`).trim();
    const shoppingStatus = () => psql(`SELECT status FROM nestova.shopping_list_item WHERE id = '${shoppingA}';`).trim();
    expect(pantryQty()).toBe('500 g');

    await login(page, OWNER_B);
    const csrf_token = await csrfToken(page, '/groceries');
    for (const action of ['consume', 'adjust']) {
      expectRefusedLikeMissing(`${action} A's pantry item`,
        await probeForeignId(page, (id) => `/groceries/pantry/${id}/${action}`, pantryA, { csrf_token, amount: '100', unit: 'g' }));
    }
    expectRefusedLikeMissing("change A's shopping status",
      await probeForeignId(page, (id) => `/groceries/shopping/${id}/status`, shoppingA, { csrf_token, status: 'in_cart' }));
    expect(pantryQty(), "A's pantry quantity must not move").toBe('500 g');
    expect(shoppingStatus(), "A's shopping item must keep its status").toBe('needed');

    // Sanity guard: A performs the same three payloads on the same ids.
    expect(await postForm(a.page, `/groceries/pantry/${pantryA}/consume`, { csrf_token: aToken, amount: '100', unit: 'g' })).toBe(303);
    expect(await postForm(a.page, `/groceries/pantry/${pantryA}/adjust`, { csrf_token: aToken, amount: '100', unit: 'g' })).toBe(303);
    expect(await postForm(a.page, `/groceries/shopping/${shoppingA}/status`, { csrf_token: aToken, status: 'in_cart' })).toBe(303);
    expect(shoppingStatus()).toBe('in_cart');
    await a.context.close();
  });

  test('T-0.2.7 household B cannot edit, delete or plan household A recipes', async ({ page, browser }) => {
    const recipe = (csrf, title) => [
      ['csrf_token', csrf], ['title', title], ['servings', '4'], ['instructions', 'Mix.'],
      ['ingredient_name', unique('Oats')], ['ingredient_amount', '200'], ['ingredient_unit', 'g'], ['ingredient_optional', 'false'],
    ];
    const titleA = unique('A recipe');
    const a = await signedIn(browser, PERSONAS.owner);
    const aToken = await csrfToken(a.page, '/meals');
    expect(await postPairs(a.page, '/meals/recipes', recipe(aToken, titleA))).toBe(303);
    const recipeA = psql(`SELECT id FROM nestova.recipe WHERE title = '${titleA}' LIMIT 1;`).trim();

    await login(page, OWNER_B);
    const csrf_token = await csrfToken(page, '/meals');
    const titleB = unique('B recipe');
    expect(await postPairs(page, '/meals/recipes', recipe(csrf_token, titleB))).toBe(303);
    const recipeB = psql(`SELECT id FROM nestova.recipe WHERE title = '${titleB}' LIMIT 1;`).trim();

    const hijack = recipe(csrf_token, 'Hijacked recipe');
    const editForeign = await postPairs(page, `/meals/recipes/${recipeA}`, hijack);
    const editMissing = await postPairs(page, '/meals/recipes/00000000-0000-0000-0000-000000000000', hijack);
    expectRefusedLikeMissing("edit A's recipe", { foreign: editForeign, missing: editMissing });
    expectRefusedLikeMissing("delete A's recipe",
      await probeForeignId(page, (id) => `/meals/recipes/${id}/delete`, recipeA, { csrf_token }));

    const date = '2026-12-01';
    const plan = (id) => ({ csrf_token, date, meal: 'dinner', recipe_id: id, servings: '4' });
    expectRefusedLikeMissing("plan A's recipe into B's week", {
      foreign: await postForm(page, '/meals/plan', plan(recipeA)),
      missing: await postForm(page, '/meals/plan', plan('00000000-0000-0000-0000-000000000000')),
    });

    expect(psql(`SELECT title FROM nestova.recipe WHERE id = '${recipeA}';`).trim(), "A's recipe keeps its title").toBe(titleA);
    expect(psql(`SELECT count(*) FROM nestova.meal_plan_entry WHERE recipe_id = '${recipeA}';`).trim(),
      "A's recipe must not appear in any plan").toBe('0');

    // Sanity guards: B plans its own recipe with the same payload, and A edits
    // and deletes its own recipe with the same payloads B was refused.
    expect(await postForm(page, '/meals/plan', plan(recipeB)), "sanity: B plans B's recipe").toBe(303);
    expect(await postPairs(a.page, `/meals/recipes/${recipeA}`, recipe(aToken, `${titleA} v2`))).toBe(303);
    expect(await postForm(a.page, `/meals/recipes/${recipeA}/delete`, { csrf_token: aToken })).toBe(303);
    await a.context.close();
  });

  test('T-0.2.8 household B cannot edit or deactivate household A subscriptions', async ({ page, browser }) => {
    const nameA = unique('A streaming');
    const fields = (csrf, name, amount) => ({
      csrf_token: csrf, name, amount, currency: 'USD', cycle: 'monthly', next_renewal_on: '2026-12-15', reminder_lead_days: '3',
    });
    const a = await signedIn(browser, PERSONAS.owner);
    const aToken = await csrfToken(a.page, '/subscriptions');
    expect(await postForm(a.page, '/subscriptions', fields(aToken, nameA, '10.00'))).toBe(303);
    const subA = psql(`SELECT id FROM nestova.subscription WHERE name = '${nameA}' LIMIT 1;`).trim();
    const row = () => psql(`SELECT name || '|' || amount_cents || '|' || active FROM nestova.subscription WHERE id = '${subA}';`).trim();
    const before = row();

    await login(page, OWNER_B);
    const csrf_token = await csrfToken(page, '/subscriptions');
    expectRefusedLikeMissing("edit A's subscription",
      await probeForeignId(page, (id) => `/subscriptions/${id}`, subA, fields(csrf_token, 'Hijacked', '0.01')));
    expectRefusedLikeMissing("deactivate A's subscription",
      await probeForeignId(page, (id) => `/subscriptions/${id}/deactivate`, subA, { csrf_token }));
    expect(row(), "A's subscription must be untouched").toBe(before);

    // Sanity guard: A edits and deactivates the same id with the same payloads.
    expect(await postForm(a.page, `/subscriptions/${subA}`, fields(aToken, nameA, '12.00'))).toBe(303);
    expect(await postForm(a.page, `/subscriptions/${subA}/deactivate`, { csrf_token: aToken })).toBe(303);
    await a.context.close();
  });

  test("T-0.2.10 household A cannot revoke household B's kiosk device", async ({ page, browser }) => {
    // A household has at most one active kiosk device
    // (kiosk_device_household_active_uniq), and an earlier spec may already
    // have activated one for B. Retire it first, as activating a new device
    // through the app does (revokeActiveDevices in the kiosk adapter).
    const seedDevice = (household, name) => psql(`
      UPDATE nestova.kiosk_device SET revoked_at = now()
       WHERE household_id = '${household}' AND revoked_at IS NULL;
      INSERT INTO nestova.kiosk_device (id, household_id, token_hash, name, created_at)
      VALUES (gen_random_uuid(), '${household}', md5(random()::text || clock_timestamp()::text), '${name}', now())
      RETURNING id;`).trim();
    const deviceB = seedDevice(b.householdId, unique('B hallway screen'));
    const revokedAt = () => psql(`SELECT coalesce(revoked_at::text, 'live') FROM nestova.kiosk_device WHERE id = '${deviceB}';`).trim();

    await login(page, PERSONAS.owner);
    const csrf_token = await csrfToken(page, '/settings');
    expectRefusedLikeMissing("revoke B's kiosk device",
      await probeForeignId(page, (id) => `/settings/kiosk/${id}/revoke`, deviceB, { csrf_token }));
    expect(revokedAt(), "B's device must stay live").toBe('live');

    // Sanity guard: B's own owner revokes the same device with the same payload.
    const bSide = await signedIn(browser, OWNER_B);
    const bToken = await csrfToken(bSide.page, '/settings');
    expect(await postForm(bSide.page, `/settings/kiosk/${deviceB}/revoke`, { csrf_token: bToken }),
      "sanity: B revokes B's device").toBe(303);
    expect(revokedAt()).not.toBe('live');
    await bSide.context.close();
  });
});
