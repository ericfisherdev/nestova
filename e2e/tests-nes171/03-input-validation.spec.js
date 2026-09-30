// §0.5 / §0.6 input fuzz — what can a user type that the system cannot use?
//
// Appendix A of the checklist predicts these will find gaps: the schema has 90
// `text` columns and no length CHECK on any user field, and domain validation
// is trim-then-non-empty with no upper bound. Each test states the safeguard it
// expects, so a failure is a filed defect rather than a surprise.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql, seedMemberInA } = require('../tests/db');
const fuzz = require('./helpers-fuzz');

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

// ---------------------------------------------------------------------------
// §0.5 applied to every text field (Appendix A.1's list).
//
// One test per checklist ID, each walking fuzz.TEXT_FIELDS, so every field gets
// the same treatment and a failure names the fields that broke. Household name
// and passkey nickname are listed in fuzz.UNREACHABLE_TEXT_FIELDS with the
// reason a running server cannot reach them.
// ---------------------------------------------------------------------------

// A sweep submits every field and may load a page per field, which outruns the
// config's 45s default under slowMo.
const SWEEP_TIMEOUT = 180_000;

// sweep runs probe(field) for every field in fields and returns the failures
// it reports, prefixed with the field name. probe returns a string (or list of
// strings) describing what went wrong, or nothing.
async function sweep(fields, probe) {
  const failures = [];
  for (const field of fields) {
    const problems = [].concat((await probe(field)) || []);
    failures.push(...problems.map((p) => `${field.name}: ${p}`));
  }
  return failures;
}

function describeRefusal(res) {
  return `${res.status} ${res.body.replace(/\s+/g, ' ').slice(0, 100)}`;
}

// sanityAccepts proves the route takes a fully valid value for field. It backs
// the unmarked "sanity:" test, so a broken fill cannot hide inside a [!] test.
async function sanityAccepts(page, csrf_token, field) {
  const marker = fuzz.uniqueMarker('sane');
  const res = await fuzz.createWith(page, csrf_token, field, `Valid ${marker}`, marker);
  fuzz.cleanupField(field, marker);
  return res.status === field.accepted && res.stored.length === 1
    ? null
    : `a VALID value was not accepted (${describeRefusal(res)}, ${res.stored.length} rows)`;
}

test.describe('§0.5 every text field', () => {
  test.beforeEach(async ({ page }) => {
    test.setTimeout(SWEEP_TIMEOUT);
    await login(page, PERSONAS.owner);
  });

  test('T-0.5.4 leading and trailing whitespace is trimmed before save', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    const failures = await sweep(fuzz.TEXT_FIELDS, async (field) => {
      const marker = fuzz.uniqueMarker('trim');
      const value = `  \t Trim ${marker} \t  `;
      const res = await fuzz.createWith(page, csrf_token, field, value, marker);
      fuzz.cleanupField(field, marker);
      if (res.status !== field.accepted) return `refused a valid padded value: ${describeRefusal(res)}`;
      const want = fuzz.expectedStored(field, value);
      if (res.stored[0] !== want) return `stored ${JSON.stringify(res.stored[0])}, want ${JSON.stringify(want)}`;
      return null;
    });
    expect(failures, 'fields that stored surrounding whitespace').toEqual([]);
  });

  test('sanity: every text field accepts a valid value', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    const failures = await sweep(fuzz.TEXT_FIELDS, (field) => sanityAccepts(page, csrf_token, field));
    expect(failures, 'fields that refused a valid value, which would mask the [!] tests below').toEqual([]);
  });

  test('T-0.5.5 script tags in a text field are escaped, never executed', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    const failures = await sweep(fuzz.renderedFields(), async (field) => {
      const marker = fuzz.uniqueMarker('xss');
      const value = `<script>window.__nes171xss=1</script>${marker}`;
      const res = await fuzz.createWith(page, csrf_token, field, value, marker);
      if (res.status !== field.accepted) {
        fuzz.cleanupField(field, marker);
        return `refused the payload, so its rendering was never exercised: ${describeRefusal(res)}`;
      }
      const shown = fuzz.expectedStored(field, value);
      const problems = [];
      for (const view of field.views) {
        await page.goto(view);
        if (!(await page.evaluate(fuzz.pageShows, shown))) problems.push(`${view} does not show the value as text, so nothing was checked`);
        if (await page.evaluate(() => window.__nes171xss === 1)) problems.push(`${view} executed the stored script`);
      }
      fuzz.cleanupField(field, marker);
      return problems;
    });
    expect(failures, 'fields whose stored script executed or was not rendered').toEqual([]);
  });

  test('T-0.5.6 attribute-context injection is escaped, never parsed into an attribute', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    const handler = 'onmouseover';
    const failures = await sweep(fuzz.renderedFields(), async (field) => {
      const marker = fuzz.uniqueMarker('attr');
      const value = `Attr ${marker}" ${handler}="window.__nes171attr=1" data-x="`;
      const res = await fuzz.createWith(page, csrf_token, field, value, marker);
      if (res.status !== field.accepted) {
        fuzz.cleanupField(field, marker);
        return `refused the payload, so its rendering was never exercised: ${describeRefusal(res)}`;
      }
      const shown = fuzz.expectedStored(field, value);
      const problems = [];
      for (const view of field.views) {
        await page.goto(view);
        if (!(await page.evaluate(fuzz.pageShows, shown))) problems.push(`${view} does not show the value, so nothing was checked`);
        const injected = await page.evaluate(fuzz.elementsWithAttribute, handler);
        if (injected > 0) problems.push(`${view} has ${injected} element(s) with an injected ${handler}`);
        if (await page.evaluate(() => window.__nes171attr === 1)) problems.push(`${view} ran the injected handler`);
      }
      fuzz.cleanupField(field, marker);
      return problems;
    });
    expect(failures, 'fields whose value escaped its attribute').toEqual([]);
  });

  test('T-0.5.7 SQL-ish input is stored as literal text', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    const failures = await sweep(fuzz.TEXT_FIELDS, async (field) => {
      const marker = fuzz.uniqueMarker('sql');
      const value = `'; DROP TABLE member;-- ${marker}`;
      const res = await fuzz.createWith(page, csrf_token, field, value, marker);
      fuzz.cleanupField(field, marker);
      if (res.status !== field.accepted) return `refused a legal value: ${describeRefusal(res)}`;
      const want = fuzz.expectedStored(field, value);
      if (res.stored[0] !== want) return `stored ${JSON.stringify(res.stored[0])}, want ${JSON.stringify(want)}`;
      return null;
    });
    expect(failures, 'fields that did not store the SQL-ish text literally').toEqual([]);
    expect(
      psql("SELECT to_regclass('identity.member') IS NOT NULL;").trim(),
      'the member table must still exist',
    ).toBe('t');
  });

  test('T-0.5.9 an RTL override (U+202E) does not reverse the surrounding UI', async ({ page }) => {
    // Prove the detector first: an unterminated override inside an inline
    // span DOES reverse the text after it, and the detector must see that.
    await page.goto('/');
    await page.evaluate(() => {
      const probe = document.createElement('div');
      probe.innerHTML = '<span>\u202Econtrol</span> surrounding text';
      document.body.prepend(probe);
    });
    const control = await page.evaluate(fuzz.reversedNeighbours, '\u202Econtrol');
    expect(control.reversed, 'the reversal detector must catch a known leak').toEqual(['surrounding text']);

    const csrf_token = await fuzz.csrfFor(page);
    const failures = await sweep(fuzz.renderedFields(), async (field) => {
      const marker = fuzz.uniqueMarker('rtl');
      const value = `\u202E${marker}`;
      const res = await fuzz.createWith(page, csrf_token, field, value, marker);
      if (res.status !== field.accepted) {
        fuzz.cleanupField(field, marker);
        return `refused the value, so its rendering was never exercised: ${describeRefusal(res)}`;
      }
      const problems = [];
      for (const view of field.views) {
        await page.goto(view);
        const { hits, reversed } = await page.evaluate(fuzz.reversedNeighbours, fuzz.expectedStored(field, value));
        if (hits === 0) problems.push(`${view} does not render the value, so nothing was checked`);
        if (reversed.length) problems.push(`${view} reverses the text after it: ${JSON.stringify(reversed.slice(0, 3))}`);
      }
      fuzz.cleanupField(field, marker);
      return problems;
    });
    expect(failures, 'fields whose RTL override leaked into the surrounding UI').toEqual([]);
  });

  test('T-0.5.10 a null byte is refused or stripped, never a 500', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    const failures = await sweep(fuzz.TEXT_FIELDS, async (field) => {
      const marker = fuzz.uniqueMarker('nul');
      const res = await fuzz.createWith(page, csrf_token, field, `Nul\u0000byte ${marker}`, marker);
      fuzz.cleanupField(field, marker);
      if (res.status >= 500) return `returned ${res.status}`;
      if (res.stored.some((v) => v.includes('\u0000'))) return 'stored the NUL byte';
      return null;
    });
    expect(failures, 'fields that could not handle a NUL byte').toEqual([]);
  });

  test('T-0.5.11 a 4-byte ZWJ emoji sequence survives a save/load round trip', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    const family = '\u{1F468}‍\u{1F469}‍\u{1F467}‍\u{1F466}';
    const failures = await sweep(fuzz.TEXT_FIELDS, async (field) => {
      const marker = fuzz.uniqueMarker('zwj');
      const value = `Family ${family} ${marker}`;
      const res = await fuzz.createWith(page, csrf_token, field, value, marker);
      if (res.status !== field.accepted) {
        fuzz.cleanupField(field, marker);
        return `refused the value: ${describeRefusal(res)}`;
      }
      const want = fuzz.expectedStored(field, value);
      const problems = [];
      if (res.stored[0] !== want) problems.push(`stored ${JSON.stringify(res.stored[0])}`);
      for (const view of field.views) {
        await page.goto(view);
        if (!(await page.evaluate(fuzz.pageShows, want))) problems.push(`${view} does not show the sequence intact`);
      }
      fuzz.cleanupField(field, marker);
      return problems;
    });
    expect(failures, 'fields that damaged the emoji sequence').toEqual([]);
  });

  test('T-0.5.12 [!] a 500-character single word wraps without a horizontal scroll', async ({ page }) => {
    test.fail(true, 'DEFECT: album, recipe and subscription names render in unwrapped <p> elements that push the page sideways, and a member name scrolls the sidebar');
    const csrf_token = await fuzz.csrfFor(page);
    const failures = await sweep(fuzz.renderedFields(), async (field) => {
      const marker = fuzz.uniqueMarker('word');
      // Where a field caps its length (the task title, NES-172) the longest
      // word it accepts is the case that matters.
      const length = Math.min(500, field.maxLength || 500);
      const word = marker + 'w'.repeat(length - marker.length);
      const res = await fuzz.createWith(page, csrf_token, field, word, marker);
      if (res.status !== field.accepted) {
        fuzz.cleanupField(field, marker);
        return `refused a ${length}-character word: ${describeRefusal(res)}`;
      }
      const problems = [];
      for (const view of field.views) {
        await page.goto(view);
        const overflow = await page.evaluate(fuzz.overflowingElements, fuzz.expectedStored(field, word));
        if (overflow.length) problems.push(`${view}: ${overflow.slice(0, 3).join('; ')}`);
      }
      fuzz.cleanupField(field, marker);
      return problems;
    });
    expect(failures, 'fields whose long word broke the layout').toEqual([]);
  });

  test('T-0.5.13 a duplicate member display name is refused with a clear message', async ({ page }) => {
    const member = fuzz.TEXT_FIELDS.find((f) => f.name === 'member display name');
    const csrf_token = await fuzz.csrfFor(page);
    const marker = fuzz.uniqueMarker('dup');
    const displayName = `Twin ${marker}`;
    try {
      const first = await fuzz.createWith(page, csrf_token, member, displayName, marker);
      expect(first.status, 'the first member with this name must be accepted').toBe(303);

      // The second attempt goes through the real form, so the message is
      // asserted where the user reads it.
      await page.goto('/members/new');
      await page.fill('input[name="display_name"]', displayName);
      await page.selectOption('select[name="role"]', 'child');
      const [response] = await Promise.all([
        page.waitForResponse((r) => r.url().endsWith('/members') && r.request().method() === 'POST'),
        page.locator('form[action="/members"] button[type="submit"]').click(),
      ]);
      expect(response.status(), 'ErrDuplicateMember must map to 409').toBe(409);
      await expect(page.getByText('A member with that name already exists in your household.')).toBeVisible();
      expect(fuzz.storedValues(member.table, member.column, marker), 'only one member may exist').toHaveLength(1);
    } finally {
      fuzz.cleanupField(member, marker);
    }
  });

  test('T-0.5.3/A.1 [!] every text field refuses 10,000 characters', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    const failures = await sweep(fuzz.TEXT_FIELDS, async (field) => {
      const marker = fuzz.uniqueMarker('huge');
      const huge = marker + 'a'.repeat(10_000 - marker.length);
      const res = await fuzz.createWith(page, csrf_token, field, huge, marker);
      fuzz.cleanupField(field, marker);
      if (res.status >= 500) return `returned ${res.status}`;
      if (res.status < 400 || res.stored.length) return `accepted (${res.status}) and stored ${res.stored.map((v) => v.length)} chars`;
      return null;
    });
    expect(failures, 'text fields without a length cap').toEqual([]);
  });
});

// The passkey nickname is A.1's one field that lives behind WebAuthn, which the
// server only wires when PUBLIC_BASE_URL is set. Like 25-passkeys, this test
// targets a second server on the same database named by
// NESTOVA_WEBAUTHN_BASE_URL, and skips when there is none. Renaming needs no
// ceremony — only a credential row — so the row is seeded.
test.describe('A.1 passkey nickname', () => {
  const webauthnBaseURL = process.env.NESTOVA_WEBAUTHN_BASE_URL || '';

  test('T-0.5.3/A.1 [!] a passkey nickname refuses 10,000 characters', async ({ browser }) => {
    test.skip(!webauthnBaseURL, 'needs NESTOVA_WEBAUTHN_BASE_URL: a server on this database started with PUBLIC_BASE_URL set to its own origin');

    // A seeded member: a credential on a shared persona would put a passkey
    // step-up in front of every later password login.
    const persona = { email: 'fuzz-passkey@test.local', password: PERSONAS.owner.password };
    const memberId = seedMemberInA({
      displayName: 'Fuzz passkey', email: persona.email, role: 'adult', copyHashFrom: PERSONAS.owner.email,
    });
    const context = await browser.newContext({ baseURL: webauthnBaseURL });
    const page = await context.newPage();
    // Signed in before the credential exists, so the login itself is plain.
    // Not helpers.login: on a WebAuthn-wired server "Sign in with passkey" is
    // the first button matching has-text("Sign in").
    await page.goto('/login');
    await page.fill('input[name="email"]', persona.email);
    await page.fill('input[name="password"]', persona.password);
    await page.click('button[type="submit"]:has-text("Sign in")');
    await page.waitForURL((u) => new URL(u).pathname === '/', { timeout: 15_000 });
    const credentialId = psql(`
      INSERT INTO identity.member_credential (id, household_id, member_id, credential_id, public_key, nickname, user_handle)
      SELECT gen_random_uuid(), household_id, id, gen_random_bytes(32), '\\x00'::bytea, 'Seeded key', gen_random_bytes(32)
        FROM identity.member WHERE id = '${memberId}'
      RETURNING id;`).trim();
    const nickname = () => psql(`SELECT length(nickname) FROM identity.member_credential WHERE id = '${credentialId}';`).trim();

    try {
      const csrf_token = await fuzz.csrfFor(page, '/settings');
      const rename = (value) => fuzz.submit(page, `/settings/webauthn/${credentialId}/rename`, [['csrf_token', csrf_token], ['nickname', value]]);

      expect((await rename('Kitchen tablet')).status, 'a normal nickname must be accepted').toBe(303);
      expect(nickname(), 'the normal nickname must be stored').toBe(String('Kitchen tablet'.length));

      const res = await rename('n'.repeat(10_000));
      expect(res.status, 'an oversized nickname must not be a server error').toBeLessThan(500);
      expect(res.status, 'an oversized nickname must be refused').toBeGreaterThanOrEqual(400);
      expect(nickname(), 'the stored nickname must be unchanged').toBe(String('Kitchen tablet'.length));
    } finally {
      psql(`DELETE FROM identity.member_credential WHERE id = '${credentialId}';`);
      await context.close();
    }
  });
});

// ---------------------------------------------------------------------------
// A.2 — the password length is unbounded, so an arbitrarily large input is
// hashed with argon2id.
// ---------------------------------------------------------------------------
test.describe('A.2 password length', () => {
  test('A.2 [!] a 1 MB password is refused before it is hashed', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const csrf_token = await fuzz.csrfFor(page, '/members/new');
    const member = (label) => {
      const marker = fuzz.uniqueMarker(label);
      return { marker, fields: { display_name: `Pw ${marker}`, role: 'adult', email: `${marker}@fuzz.local` } };
    };

    const sane = member('pwok');
    const saneStarted = Date.now();
    const saneRes = await fuzz.submit(page, '/members', Object.entries({ csrf_token, ...sane.fields, password: 'long-enough-1' }));
    const saneElapsed = Date.now() - saneStarted;
    psql(`DELETE FROM identity.member WHERE display_name LIKE '%${sane.marker}%';`);
    expect(saneRes.status, 'a normal password must be accepted').toBe(303);

    const huge = member('pwhuge');
    const started = Date.now();
    const res = await fuzz.submit(page, '/members', Object.entries({ csrf_token, ...huge.fields, password: 'p'.repeat(1_000_000) }));
    const elapsed = Date.now() - started;
    const created = psql(`SELECT count(*) FROM identity.member WHERE display_name LIKE '%${huge.marker}%';`).trim();
    psql(`DELETE FROM identity.member WHERE display_name LIKE '%${huge.marker}%';`);
    test.info().annotations.push({ type: 'evidence', description: `1 MB password -> ${res.status} in ${elapsed} ms (13-char password: ${saneElapsed} ms), members created: ${created}` });

    expect(res.status, 'an oversized password must be refused').toBeGreaterThanOrEqual(400);
    expect(res.status, 'an oversized password must not be a server error').toBeLessThan(500);
    expect(created, 'no member may be created with it').toBe('0');
  });
});

// ---------------------------------------------------------------------------
// §0.6 applied to every numeric field, plus Appendix A.4's upper bounds.
// ---------------------------------------------------------------------------

// pinTarget is a member seeded for the PIN cases, so no shared persona ends up
// with a PIN that would gate their chore completions (NES-166).
function pinTarget() {
  return seedMemberInA({
    displayName: 'Fuzz PIN target',
    email: 'fuzz-pin-target@test.local',
    role: 'child',
    copyHashFrom: PERSONAS.owner.email,
  });
}

function isRefusal(res) {
  return res.status >= 400 && res.status < 500;
}

// numericSanity proves each field accepts a plain valid number in this test,
// and that the value read back is the one entered — so the stored-value
// assertions below are reading the right column.
async function numericSanity(page, csrf_token, fields) {
  return sweep(fields, async (field) => {
    const valid = field.sample || '3';
    const res = await fuzz.submitNumber(page, csrf_token, field, valid);
    if (res.status !== field.accepted) return `a VALID ${valid} was not accepted: ${describeRefusal(res)}`;
    if (field.stored && Number(res.stored) !== Number(valid) * field.scale) return `a valid ${valid} was stored as ${res.stored}`;
    return null;
  });
}

// expectRefused reports v unless field refuses it with a 4xx and stores nothing.
async function expectRefused(page, csrf_token, field, v) {
  const res = await fuzz.submitNumber(page, csrf_token, field, v);
  if (isRefusal(res) && !res.stored) return null;
  return `${JSON.stringify(v)} -> ${describeRefusal(res)}${res.stored ? `, stored ${res.stored}` : ''}`;
}

test.describe('§0.6 every numeric field', () => {
  let fields;

  test.beforeEach(async ({ page }) => {
    test.setTimeout(SWEEP_TIMEOUT);
    fields = fuzz.numericFields(pinTarget());
    await login(page, PERSONAS.owner);
  });

  test('sanity: every numeric field accepts a valid number', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    expect(await numericSanity(page, csrf_token, fields), 'fields that refused a valid number, which would mask the [!] tests below').toEqual([]);
  });

  test('T-0.6.5 a fraction where an integer is expected is refused, consistently', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    const integers = fields.filter((f) => f.kind === 'integer');
    expect(await numericSanity(page, csrf_token, integers), 'fields that refused a valid integer').toEqual([]);

    // "Consistently" is the point: every integer field must give the same
    // answer, and the answer is a refusal, not a silent truncation to 1.
    const failures = await sweep(integers, (field) => expectRefused(page, csrf_token, field, '1.5'));
    expect(failures, 'integer fields that did not refuse 1.5').toEqual([]);
  });

  test('T-0.6.6 [!] scientific notation is refused', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);

    const failures = await sweep(fields, (field) => expectRefused(page, csrf_token, field, '1e10'));
    expect(failures, 'fields that accepted 1e10').toEqual([]);
  });

  test('T-0.6.7 leading +, leading zeros and thousands separators are refused or read exactly', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    const readable = fields.filter((f) => f.stored);
    expect(await numericSanity(page, csrf_token, readable), 'fields that refused a valid number').toEqual([]);

    // Either answer is safe; misreading is not. "1,000" must never become 1,
    // and "0007" must never become anything but 7.
    const cases = [['+5', 5], ['0007', 7], ['1,000', 1000]];
    const failures = await sweep(readable, async (field) => {
      const problems = [];
      for (const [v, want] of cases) {
        const res = await fuzz.submitNumber(page, csrf_token, field, v);
        if (res.status === field.accepted) {
          if (Number(res.stored) !== want * field.scale) problems.push(`${v} accepted but stored as ${res.stored}`);
        } else if (!isRefusal(res) || res.stored) {
          problems.push(`${v} -> ${describeRefusal(res)}`);
        }
      }
      return problems;
    });
    expect(failures, 'fields that misread a formatted number').toEqual([]);
  });

  test('T-0.6.8 Infinity and NaN are refused, and -0 behaves exactly as 0', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    expect(await numericSanity(page, csrf_token, fields), 'fields that refused a valid number').toEqual([]);

    const failures = await sweep(fields, async (field) => {
      const problems = [];
      for (const v of ['Infinity', '-Infinity', 'NaN']) {
        const refused = await expectRefused(page, csrf_token, field, v);
        if (refused) problems.push(refused);
      }
      const zero = await fuzz.submitNumber(page, csrf_token, field, '0');
      const negZero = await fuzz.submitNumber(page, csrf_token, field, '-0');
      if ((zero.status === field.accepted) !== (negZero.status === field.accepted) || isRefusal(zero) !== isRefusal(negZero)) {
        problems.push(`0 -> ${zero.status} but -0 -> ${negZero.status}`);
      }
      if (negZero.stored && negZero.stored !== zero.stored) problems.push(`-0 stored as ${negZero.stored}, 0 as ${zero.stored}`);
      return problems;
    });
    expect(failures, 'fields that mishandled Infinity, NaN or -0').toEqual([]);
  });

  test('T-0.6.9 [!] a sub-cent amount is refused and money errors read as sentences', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    const cost = fields.find((f) => f.name === 'subscription cost');

    const failures = [];
    const subCent = await fuzz.submitNumber(page, csrf_token, cost, '9.999');
    if (!isRefusal(subCent)) failures.push(`9.999 -> ${describeRefusal(subCent)}, stored ${subCent.stored} cents`);

    // ErrInvalidMoney: the currency is the one money field a user can type
    // freely. Its refusal must be a sentence, not a wrapped Go error chain.
    const marker = fuzz.uniqueMarker('cur');
    const badCurrency = await fuzz.submit(page, '/subscriptions', [
      ['csrf_token', csrf_token],
      ...fuzz.withField(cost.pairs('9.99', marker), 'currency', 'usdx'),
    ]);
    if (!isRefusal(badCurrency)) failures.push(`currency usdx -> ${describeRefusal(badCurrency)}`);
    else if (/\b(household|subscriptions):|%!|got "/.test(badCurrency.body)) failures.push(`currency usdx reads as a Go error: ${badCurrency.body.trim()}`);

    // ErrCurrencyMismatch: two currencies at once must show a readable rollup,
    // not an error page.
    const euroMarker = fuzz.uniqueMarker('eur');
    try {
      const euro = await fuzz.submit(page, '/subscriptions', [
        ['csrf_token', csrf_token],
        ...fuzz.withField(cost.pairs('5.00', euroMarker), 'currency', 'EUR'),
      ]);
      expect(euro.status, 'a EUR subscription must be accepted').toBe(303);
      const view = await page.goto('/subscriptions');
      expect(view.status(), 'mixed currencies must not break the page').toBe(200);
      await expect(page.locator('[data-testid="monthly-rollup"]')).toHaveText(/Mixed currencies/);
    } finally {
      psql(`UPDATE nestova.subscription SET active = false WHERE name LIKE '%${euroMarker}%';`);
    }

    expect(failures, 'money input that was not refused readably').toEqual([]);
  });

  test('A.4/T-0.6.4 [!] integer form fields past the int4 range are refused, not a 500', async ({ page }) => {
    const csrf_token = await fuzz.csrfFor(page);
    const byName = (n) => fields.find((f) => f.name === n);
    const int4Fields = ['task points', 'task lead time', 'reward cost', 'reward quantity'].map(byName);

    const failures = await sweep(int4Fields, async (field) => {
      const problems = [];
      for (const v of ['2147483648', '9223372036854775807']) {
        const refused = await expectRefused(page, csrf_token, field, v);
        if (refused) problems.push(refused);
      }
      return problems;
    });

    expect(failures, 'int4 fields without an upper bound').toEqual([]);
  });

  // Kept apart from the int4 sweep above because test.fail accepts any failure:
  // the pantry and subscription bounds are separate defects from NES-191.
  test('A.4/T-0.6.4 [!] pantry and subscription amounts have an upper bound', async ({ page }) => {
    test.fail(true, 'DEFECT (A.4): a huge weekly subscription is accepted and then 500s /subscriptions; pantry quantity has no bound');
    const csrf_token = await fuzz.csrfFor(page);
    const byName = (n) => fields.find((f) => f.name === n);
    const cost = byName('subscription cost');
    const pantry = byName('pantry quantity');
    const failures = [];

    const hugePantry = await expectRefused(page, csrf_token, pantry, '1e300');
    if (hugePantry) failures.push(`pantry quantity: ${hugePantry}`);

    // At the parser's own ceiling the cents conversion wraps: the value is
    // refused, but only because it came out negative.
    const ceiling = await fuzz.submitNumber(page, csrf_token, cost, '92233720368547758');
    if (/-\d{6,}/.test(ceiling.body)) failures.push(`subscription cost: 92233720368547758 wraps negative: ${ceiling.body.trim()}`);

    const weeklyMarker = fuzz.uniqueMarker('weekly');
    try {
      const weekly = await fuzz.submit(page, '/subscriptions', [
        ['csrf_token', csrf_token],
        ...fuzz.withField(cost.pairs('50000000000000000', weeklyMarker), 'cycle', 'weekly'),
      ]);
      if (!isRefusal(weekly)) failures.push(`subscription cost: a 5e16 weekly amount -> ${describeRefusal(weekly)}`);
      const view = await page.goto('/subscriptions');
      if (view.status() !== 200) failures.push(`subscription cost: /subscriptions then returns ${view.status()} for the whole household`);
    } finally {
      psql(`UPDATE nestova.subscription SET active = false WHERE name LIKE '%${weeklyMarker}%';`);
    }

    expect(failures, 'numeric input without a safe upper bound').toEqual([]);
  });
});
