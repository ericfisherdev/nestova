// §0.3 Role escalation — a child must be refused every parent action, and a
// non-owner adult must be refused the two owner-only ones.
//
// Each route is driven by DIRECT POST as well as by GET, because hiding a
// control in the UI is not authorization: the checklist's T-0.3.4 is precisely
// the case where the button is absent but the endpoint still answers.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, csrfToken, postForm } = require('./helpers');
const { psql, seedMemberInA } = require('../tests/db');

const TS = Date.now();

function rewardCount(name) {
  return psql(`SELECT count(*) FROM nestova.reward WHERE name = '${name}';`).trim();
}

// Parent-gated GET pages: a child must get 403, not a rendered page.
const PARENT_PAGES = ['/admin/rewards', '/admin/rewards/new', '/trades/history'];

// Parent-gated POSTs. Tokens are minted from a page the child CAN load, so a
// refusal is an authorization decision and not a CSRF rejection.
const PARENT_POSTS = [
  { path: '/admin/rewards', fields: { name: 'Child probe', cost_points: '5' } },
  { path: '/settings/kiosk/generate', fields: { name: 'Child kiosk' } },
];

test.describe('§0.3 role escalation', () => {
  test('T-0.3.1 child is refused parent pages', async ({ page }) => {
    await login(page, PERSONAS.child);
    const failures = [];
    for (const path of PARENT_PAGES) {
      const res = await page.goto(path);
      if (res.status() !== 403) failures.push(`GET ${path} -> ${res.status()}`);
    }
    expect(failures, 'parent-only pages a child could load').toEqual([]);
  });

  test('T-0.3.4 child is refused parent POSTs made directly', async ({ page }) => {
    await login(page, PERSONAS.child);
    // /settings is open to every role, so it is a valid source of a token.
    await page.goto('/settings');
    const token = await page.locator('input[name="csrf_token"]').first().inputValue();

    const failures = [];
    for (const { path, fields } of PARENT_POSTS) {
      const status = await postForm(page, path, { ...fields, csrf_token: token });
      if (status !== 403) failures.push(`POST ${path} -> ${status}`);
    }
    expect(failures, 'parent-only POSTs a child could perform').toEqual([]);
  });

  test('T-0.3.2 non-owner adult is refused MFA reset', async ({ page }) => {
    await login(page, PERSONAS.adult);
    await page.goto('/settings');
    const token = await page.locator('input[name="csrf_token"]').first().inputValue();
    const status = await postForm(page, '/settings/mfa/reset', {
      csrf_token: token, member_id: '00000000-0000-0000-0000-000000000000', password: PERSONAS.adult.password,
    });
    expect(status, 'MFA reset is owner-only').toBe(403);
  });

  test('T-0.3.3 non-owner adult is refused quiet hours', async ({ page }) => {
    await login(page, PERSONAS.adult);
    await page.goto('/settings');
    const token = await page.locator('input[name="csrf_token"]').first().inputValue();
    const status = await postForm(page, '/settings/notify/quiet-hours', {
      csrf_token: token, start: '22:00', end: '07:00',
    });
    expect(status, 'quiet hours are owner-only').toBe(403);
  });

  test('T-0.3.1b parent CAN reach the pages a child cannot', async ({ page }) => {
    // The mirror of T-0.3.1: proves the 403s above are about role, not a
    // route that is simply broken for everyone.
    await login(page, PERSONAS.owner);
    const failures = [];
    for (const path of PARENT_PAGES) {
      const res = await page.goto(path);
      if (res.status() !== 200) failures.push(`GET ${path} -> ${res.status()}`);
    }
    expect(failures, 'parent pages the owner could not load').toEqual([]);
  });

  test('T-0.3.5 a role demoted mid-session takes effect on the very next request', async ({ page }) => {
    // A seeded member, because the demotion is irreversible for the persona.
    const email = `demoted-${TS}@test.local`;
    const memberId = seedMemberInA({
      displayName: `Demoted ${TS}`, email, role: 'adult', copyHashFrom: PERSONAS.owner.email,
    });
    await login(page, { email, password: PERSONAS.owner.password });

    // Sanity guard: as an adult, the page loads and the full payload is accepted.
    expect((await page.goto('/admin/rewards')).status(), 'an adult reaches the reward admin').toBe(200);
    const csrf_token = await csrfToken(page, '/settings');
    const allowed = `Before demotion ${TS}`;
    expect(await postForm(page, '/admin/rewards', { csrf_token, name: allowed, cost_points: '5' })).toBe(303);
    expect(rewardCount(allowed)).toBe('1');

    // The demotion happens out of band — another tab, another device, psql —
    // while this session stays signed in.
    psql(`UPDATE identity.member SET role = 'child', updated_at = now() WHERE id = '${memberId}';`);

    expect((await page.goto('/admin/rewards')).status(), 'the demoted session must lose the page').toBe(403);
    const refused = `After demotion ${TS}`;
    expect(await postForm(page, '/admin/rewards', { csrf_token, name: refused, cost_points: '5' }),
      'the demoted session must lose the write').toBe(403);
    expect(rewardCount(refused), 'no reward may be created after the demotion').toBe('0');
  });

  test('T-0.3.6 a forged role in a form field, query string or cookie is ignored', async ({ page, context }) => {
    await login(page, PERSONAS.child);
    const csrf_token = await csrfToken(page, '/settings');
    const baseURL = new URL(page.url()).origin;
    await context.addCookies(['role', 'member_role', 'is_admin', 'is_parent'].map((name) => ({
      name, value: name.startsWith('is_') ? 'true' : 'owner', url: baseURL,
    })));

    const forged = `Forged role ${TS}`;
    const status = await postForm(page, '/admin/rewards?role=owner', {
      csrf_token, name: forged, cost_points: '5',
      role: 'owner', member_role: 'owner', is_admin: 'true', is_parent: 'true',
    });
    expect(status, 'a child with a forged role must still be refused').toBe(403);
    expect(rewardCount(forged), 'the forged request must write nothing').toBe('0');
    expect((await page.goto('/admin/rewards?role=owner')).status(), 'a forged role in the query string is ignored').toBe(403);

    // Sanity guard: the same payload from a real owner is accepted, so the
    // child's 403 was the role check and not a malformed request.
    await context.clearCookies();
    await login(page, PERSONAS.owner);
    const ownerToken = await csrfToken(page, '/settings');
    const real = `Real owner ${TS}`;
    expect(await postForm(page, '/admin/rewards', { csrf_token: ownerToken, name: real, cost_points: '5' })).toBe(303);
    expect(rewardCount(real)).toBe('1');
  });
});
