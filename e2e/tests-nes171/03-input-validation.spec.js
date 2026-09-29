// §0.5 / §0.6 input fuzz — what can a user type that the system cannot use?
//
// Appendix A of the checklist predicts these will find gaps: the schema has 90
// `text` columns and no length CHECK on any user field, and domain validation
// is trim-then-non-empty with no upper bound. Each test states the safeguard it
// expects, so a failure is a filed defect rather than a surprise.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql } = require('../tests/db');

const HUGE = 'A'.repeat(10_000);

// A COMPLETE, valid new-task payload. Every field the handler requires is
// present, so when a test overrides one field the 422 it asserts is caused by
// that field and not by an unrelated missing one. Getting this wrong produces
// false passes: an incomplete payload is rejected for the wrong reason and the
// test still goes green.
function validTask(overrides = {}) {
  const ownerId = psql(
    "SELECT id FROM identity.member WHERE display_name = 'Owner A' LIMIT 1;",
  ).trim();
  return {
    title: 'Valid probe',
    category: 'chore',
    freq: 'daily',
    interval: '1',
    rotation_policy: 'fixed',
    photo_policy: 'none',
    points: '5',
    lead_time_days: '0',
    pool: ownerId,
    ...overrides,
  };
}

async function tokenFrom(page, path) {
  await page.goto(path);
  return page.locator('input[name="csrf_token"]').first().inputValue();
}

test.describe('§0.5 text input safeguards', () => {
  test.beforeEach(async ({ page }) => {
    await login(page, PERSONAS.owner);
  });

  test('T-0.5.1 empty task title is refused', async ({ page }) => {
    const csrf_token = await tokenFrom(page, '/tasks/new');
    const status = await postForm(page, '/tasks', {
      csrf_token, ...validTask({ title: '' }),
    });
    expect(status, 'an empty title must be refused').toBe(422);
  });

  test('T-0.5.2 whitespace-only task title is refused', async ({ page }) => {
    const csrf_token = await tokenFrom(page, '/tasks/new');
    const status = await postForm(page, '/tasks', {
      csrf_token, ...validTask({ title: '   \t  ' }),
    });
    expect(status, 'a whitespace-only title must be refused, not stored blank').toBe(422);
  });

  test('T-0.5.3 [!] a 10,000-character task title is refused', async ({ page }) => {
    const csrf_token = await tokenFrom(page, '/tasks/new');
    const status = await postForm(page, '/tasks', {
      csrf_token, ...validTask({ title: HUGE }),
    });
    // Expected safeguard: a bounded title. Appendix A predicts none exists.
    expect(status, 'an unbounded title must not be accepted').toBe(422);
  });

  test('T-0.5.5 script tags in a title are escaped, never executed', async ({ page }) => {
    const csrf_token = await tokenFrom(page, '/tasks/new');
    const title = '<script>window.__xss = 1</script>';
    await postForm(page, '/tasks', { csrf_token, ...validTask({ title }) });

    await page.goto('/tasks');
    const executed = await page.evaluate(() => window.__xss === 1);
    expect(executed, 'stored script must not execute on render').toBe(false);
  });

  test('T-0.5.8 unicode in a title round-trips intact', async ({ page }) => {
    const csrf_token = await tokenFrom(page, '/tasks/new');
    const title = 'Ｕnicode 家事 مهمة 👨‍👩‍👧‍👦';
    const status = await postForm(page, '/tasks', { csrf_token, ...validTask({ title }) });
    expect(status, 'valid unicode must be accepted').toBe(303);

    const stored = psql(
      `SELECT title FROM nestova.recurring_task WHERE title LIKE 'Ｕnicode%' LIMIT 1;`,
    ).trim();
    expect(stored, 'unicode must survive the round trip byte-for-byte').toBe(title);
  });
});

test.describe('§0.6 numeric input safeguards', () => {
  test.beforeEach(async ({ page }) => {
    await login(page, PERSONAS.owner);
  });

  test('T-0.6.1 interval of zero is refused', async ({ page }) => {
    const csrf_token = await tokenFrom(page, '/tasks/new');
    const status = await postForm(page, '/tasks', {
      csrf_token, ...validTask({ title: 'Zero interval', interval: '0' }),
    });
    expect(status).toBe(422);
  });

  test('T-0.6.2 negative interval is refused', async ({ page }) => {
    const csrf_token = await tokenFrom(page, '/tasks/new');
    const status = await postForm(page, '/tasks', {
      csrf_token, ...validTask({ title: 'Negative interval', interval: '-1' }),
    });
    expect(status).toBe(422);
  });

  test('T-0.6.4 int64-boundary intervals are refused, not wrapped', async ({ page }) => {
    const csrf_token = await tokenFrom(page, '/tasks/new');
    // MaxInt64 itself is parseable but absurd as a cadence; MaxInt64+1 does not
    // fit at all. Neither may be silently accepted or wrapped to a small number.
    const cases = ['9223372036854775807', '9223372036854775808', '99999999999999999999'];
    const accepted = [];
    for (const interval of cases) {
      const status = await postForm(page, '/tasks', {
        csrf_token, ...validTask({ title: `Overflow ${interval.slice(0, 6)}`, interval }),
      });
      if (status !== 422) accepted.push(`${interval} -> ${status}`);
    }
    expect(accepted, 'int64-boundary intervals that were not refused').toEqual([]);
  });

  test('T-0.6.3 non-numeric interval is refused', async ({ page }) => {
    const csrf_token = await tokenFrom(page, '/tasks/new');
    const status = await postForm(page, '/tasks', {
      csrf_token, ...validTask({ title: 'Text interval', interval: 'seven' }),
    });
    expect(status).toBe(422);
  });
});
