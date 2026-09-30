// Field registry and probes for the §0.5 / §0.6 / §0.7 fuzz specs.
//
// §0.5 says "apply to every text field", so the text fields are described once
// here as data — where each one is submitted, what a complete valid payload
// looks like, where it is stored and where it renders — and each checklist ID
// becomes one data-driven test that walks the whole registry. A new text field
// is a new registry entry, not a new copy of every test.
//
// Every payload is COMPLETE apart from the field under test, for the reason the
// checklist's false-pass caution gives: an incomplete payload is refused for
// the wrong reason and the negative test goes green anyway.
const { psql } = require('../tests/db');
const { randomInt } = require('node:crypto');

const OWNER_NAME = 'Owner A';

function householdA() {
  return psql("SELECT id FROM identity.household WHERE name = 'Household A' LIMIT 1;").trim();
}

function memberId(displayName) {
  return psql(`SELECT id FROM identity.member WHERE display_name = '${displayName}' LIMIT 1;`).trim();
}

// uniqueMarker returns a plain alphanumeric token. Every probe value embeds one,
// so its row can be found with a LIKE that needs no SQL escaping whatever else
// the value contains.
function uniqueMarker(label) {
  return `${label}${Date.now()}${randomInt(1_000_000)}`;
}

// isoDate formats a local date the way a date input submits it.
function isoDate(daysAhead = 0) {
  const d = new Date();
  d.setDate(d.getDate() + daysAhead);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

// storedValues returns every stored value of column in table that contains
// marker, byte-exact: the values travel through psql as base64, so emoji, RTL
// controls and newlines survive the trip back into the test unchanged. The
// base64 is unwrapped in SQL — encode() breaks it every 76 characters, which
// would otherwise read as one row per line.
function storedValues(table, column, marker) {
  const out = psql(
    `SELECT translate(encode(convert_to(${column}, 'UTF8'), 'base64'), E'\\n', '') FROM ${table}
      WHERE ${column} LIKE '%${marker}%' ORDER BY created_at;`,
  ).trim();
  if (!out) return [];
  return out.split('\n').map((b64) => Buffer.from(b64, 'base64').toString('utf8'));
}

// csrfFor reads the session's CSRF token from a page that renders a form. The
// token is session-scoped, so one read serves every POST in a test.
async function csrfFor(page, path = '/settings') {
  await page.goto(path);
  return page.locator('input[name="csrf_token"]').first().inputValue();
}

// submit POSTs form pairs from inside the page's session and returns the
// status plus the start of the body. Pairs (not an object) so repeated keys —
// a recipe's ingredient lines — are expressible. A manual redirect is reported
// as 303, exactly as helpers.postForm does.
async function submit(page, path, pairs) {
  return page.evaluate(async ({ path, pairs }) => {
    const res = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams(pairs).toString(),
      redirect: 'manual',
    });
    if (res.type === 'opaqueredirect') return { status: 303, body: '' };
    return { status: res.status, body: (await res.text()).slice(0, 4000) };
  }, { path, pairs });
}

function taskPairs(title) {
  return [
    ['title', title], ['category', 'chore'], ['freq', 'daily'], ['interval', '1'],
    ['rotation_policy', 'fixed'], ['photo_policy', 'none'], ['points', '5'],
    ['lead_time_days', '0'], ['pool', memberId(OWNER_NAME)],
  ];
}

function rewardPairs(name) {
  return [['name', name], ['cost_points', '5'], ['quantity_available', '1']];
}

// withField returns pairs with key's value replaced by v.
function withField(pairs, key, v) {
  return pairs.map(([k, old]) => [k, k === key ? v : old]);
}

function recipePairs(title, ingredient) {
  return [
    ['title', title],
    ['servings', '2'],
    ['instructions', 'Mix.'],
    ['ingredient_name', ingredient],
    ['ingredient_amount', '1'],
    ['ingredient_unit', 'g'],
    ['ingredient_optional', 'false'],
  ];
}

// materializeInstance gives the recurring task whose title carries marker a
// pending instance due today. /tasks lists instances, and the background
// scheduler that creates them runs only every few minutes, so without this the
// new title would not render anywhere inside the test.
function materializeInstance(marker) {
  psql(`
    INSERT INTO nestova.task_instance
      (id, recurring_task_id, household_id, assignee_id, due_on, status, kind, created_at, updated_at)
    SELECT gen_random_uuid(), t.id, t.household_id, '${memberId(OWNER_NAME)}', current_date,
           'pending', 'scheduled', now(), now()
      FROM nestova.recurring_task t
     WHERE t.title LIKE '%${marker}%'
       AND NOT EXISTS (SELECT 1 FROM nestova.task_instance i WHERE i.recurring_task_id = t.id);`);
}

// TEXT_FIELDS is the §0.5 / Appendix A.1 field list, as far as a running
// server can reach it. Each entry:
//   name      — the field as the checklist names it
//   path      — the create route
//   pairs(v)  — a complete, valid payload with v in the field under test
//   accepted  — the status a valid create returns
//   table/column — where the value is stored
//   normalize — what the app is expected to store for a raw value (defaults
//               to trimming); ingredients are canonicalised to lower case
//   views     — pages that render the stored value (empty: none reachable)
//   beforeView — optional; seeds whatever a view needs to show the value
//   cleanup   — optional; removes rows whose presence would disturb later
//               tests (a member shows in the sidebar of every page)
//
// Two A.1 fields are not in the registry because no running server reaches
// them; see UNREACHABLE_TEXT_FIELDS.
const TEXT_FIELDS = [
  {
    name: 'task title',
    path: '/tasks',
    pairs: taskPairs,
    accepted: 303,
    maxLength: 200,
    table: 'nestova.recurring_task',
    column: 'title',
    views: ['/tasks'],
    beforeView: materializeInstance,
  },
  {
    name: 'reward name',
    path: '/admin/rewards',
    pairs: rewardPairs,
    accepted: 303,
    maxLength: 200,
    table: 'nestova.reward',
    column: 'name',
    views: ['/rewards', '/admin/rewards'],
  },
  {
    name: 'reward description',
    path: '/admin/rewards',
    pairs: (v) => [...rewardPairs(uniqueMarker('Described ')), ['description', v]],
    accepted: 303,
    maxLength: 1000,
    table: 'nestova.reward',
    column: 'description',
    views: ['/rewards', '/admin/rewards'],
  },
  {
    name: 'album name',
    path: '/albums',
    pairs: (v) => [['name', v], ['rotation_seconds', '8']],
    accepted: 303,
    maxLength: 200,
    table: 'nestova.album',
    column: 'name',
    views: ['/photos'],
  },
  {
    name: 'recipe name',
    path: '/meals/recipes',
    pairs: (v) => recipePairs(v, 'flour'),
    accepted: 303,
    maxLength: 200,
    table: 'nestova.recipe',
    column: 'title',
    views: ['/meals'],
  },
  {
    name: 'recipe ingredient',
    path: '/meals/recipes',
    pairs: (v) => recipePairs(uniqueMarker('Ingredient host '), v),
    accepted: 303,
    maxLength: 200,
    table: 'nestova.ingredient',
    column: 'canonical_name',
    normalize: (v) => v.trim().toLowerCase(),
    views: ['/meals'],
  },
  {
    name: 'subscription name',
    path: '/subscriptions',
    pairs: (v) => [
      ['name', v], ['amount', '9.99'], ['currency', 'USD'], ['cycle', 'monthly'],
      ['next_renewal_on', isoDate(30)], ['reminder_lead_days', '3'],
    ],
    accepted: 303,
    maxLength: 200,
    table: 'nestova.subscription',
    column: 'name',
    views: ['/subscriptions'],
  },
  {
    name: 'member display name',
    path: '/members',
    pairs: (v) => [['display_name', v], ['role', 'child']],
    accepted: 303,
    maxLength: 100,
    table: 'identity.member',
    column: 'display_name',
    views: ['/'],
    cleanup: (marker) => psql(`DELETE FROM identity.member WHERE display_name LIKE '%${marker}%';`),
  },
  {
    name: 'shopping item name',
    path: '/groceries/shopping',
    pairs: (v) => [['name', v], ['amount', '1'], ['unit', 'count']],
    accepted: 303,
    maxLength: 200,
    table: 'nestova.shopping_list_item',
    column: 'name',
    views: ['/groceries'],
  },
  {
    // The name is stored on the activation code and copied to the device on
    // activation; no page renders a code's name, so there is no view to check.
    name: 'kiosk device name',
    path: '/settings/kiosk/generate',
    pairs: (v) => [['name', v]],
    accepted: 200,
    maxLength: 200,
    table: 'nestova.kiosk_activation_code',
    column: 'name',
    views: [],
  },
];

const UNREACHABLE_TEXT_FIELDS = {
  'household name': 'set only by POST /onboarding, which refuses once household A exists, so a '
    + 'refusal cannot be told apart from ErrHouseholdExists (onboarding.go:148 has no length cap)',
  'passkey nickname': 'POST /settings/webauthn/{id}/rename is only registered when PUBLIC_BASE_URL '
    + 'is set (cmd/server/main.go:732), which the checklist server does not set; the A.1 passkey '
    + 'test in 03-input-validation covers it against NESTOVA_WEBAUTHN_BASE_URL instead',
};

function firstValue(sql) {
  return psql(sql).trim().split('\n')[0] || '';
}

function taskNumber(expr) {
  return (marker) => firstValue(
    `SELECT ${expr} FROM nestova.recurring_task WHERE title LIKE '%${marker}%';`,
  );
}

function rewardNumber(column) {
  return (marker) => firstValue(
    `SELECT ${column} FROM nestova.reward WHERE name LIKE '%${marker}%';`,
  );
}

function taskNumberPairs(key) {
  return (v, marker) => withField(taskPairs(`Numeric ${marker}`), key, v);
}

function rewardNumberPairs(key) {
  return (v, marker) => withField(rewardPairs(`Numeric ${marker}`), key, v);
}

// NUMERIC_FIELDS is the §0.6 field list. Each entry:
//   name       — the field as the checklist names it
//   kind       — 'integer' or 'decimal', what the field is meant to hold
//   path/pairs — the route and a complete payload with v in the field; the
//                marker names the row so it can be read back
//   accepted   — the status a valid submission returns
//   sample     — a valid value, when the default '3' is not one (a PIN)
//   scale      — stored units per entered unit (subscription amounts are cents)
//   stored     — reads the stored number back as text, or null where the value
//                is not readable (a PIN is stored hashed)
//
// The PIN target is a member seeded by the caller: setting a PIN on a shared
// persona would gate every later chore completion behind it (NES-166).
function numericFields(pinTargetId) {
  return [
    {
      name: 'task points', kind: 'integer', path: '/tasks', pairs: taskNumberPairs('points'),
      accepted: 303, scale: 1, stored: taskNumber('points'),
    },
    {
      name: 'cadence interval', kind: 'integer', path: '/tasks', pairs: taskNumberPairs('interval'),
      accepted: 303, scale: 1, stored: taskNumber("cadence->>'Interval'"),
    },
    {
      name: 'task lead time', kind: 'integer', path: '/tasks', pairs: taskNumberPairs('lead_time_days'),
      accepted: 303, scale: 1, stored: taskNumber('lead_time_days'),
    },
    {
      name: 'reward cost', kind: 'integer', path: '/admin/rewards', pairs: rewardNumberPairs('cost_points'),
      accepted: 303, scale: 1, stored: rewardNumber('cost_points'),
    },
    {
      name: 'reward quantity', kind: 'integer', path: '/admin/rewards', pairs: rewardNumberPairs('quantity_available'),
      accepted: 303, scale: 1, stored: rewardNumber('quantity_available'),
    },
    {
      name: 'subscription cost',
      kind: 'decimal',
      path: '/subscriptions',
      pairs: (v, marker) => [
        ['name', `Numeric ${marker}`], ['amount', v], ['currency', 'USD'], ['cycle', 'monthly'],
        ['next_renewal_on', isoDate(30)], ['reminder_lead_days', '3'],
      ],
      accepted: 303,
      scale: 100,
      stored: (marker) => firstValue(
        `SELECT amount_cents FROM nestova.subscription WHERE name LIKE '%${marker}%';`,
      ),
    },
    {
      name: 'pantry quantity',
      kind: 'decimal',
      path: '/groceries/pantry',
      pairs: (v, marker) => [['name', `numeric ${marker}`], ['amount', v], ['unit', 'count']],
      accepted: 303,
      scale: 1,
      stored: (marker) => firstValue(
        `SELECT p.quantity::text FROM nestova.pantry_item p
           JOIN nestova.ingredient i ON i.id = p.ingredient_id
          WHERE i.canonical_name LIKE '%${marker}%';`,
      ),
    },
    {
      name: 'member PIN',
      kind: 'integer',
      path: `/settings/members/${pinTargetId}/pin`,
      pairs: (v) => [['pin', v]],
      sample: '4321',
      accepted: 200,
      scale: 1,
      stored: null,
    },
  ];
}

// submitNumber submits v in field and returns { status, body, stored }.
async function submitNumber(page, csrf_token, field, v) {
  const marker = uniqueMarker('num');
  const res = await submit(page, field.path, [['csrf_token', csrf_token], ...field.pairs(v, marker)]);
  return { ...res, stored: field.stored ? field.stored(marker) : null };
}

function renderedFields() {
  return TEXT_FIELDS.filter((f) => f.views.length > 0);
}

// createWith submits value in field and returns { status, body, stored }, where
// stored is every row the marker finds afterwards. An accepted value is made
// renderable (see beforeView) before returning.
async function createWith(page, csrf_token, field, value, marker) {
  const res = await submit(page, field.path, [['csrf_token', csrf_token], ...field.pairs(value)]);
  if (res.status === field.accepted && field.beforeView) field.beforeView(marker);
  return { ...res, stored: storedValues(field.table, field.column, marker) };
}

function expectedStored(field, value) {
  return field.normalize ? field.normalize(value) : value.trim();
}

function cleanupField(field, marker) {
  if (field.cleanup) field.cleanup(marker);
}

// ---------------------------------------------------------------------------
// In-page probes. Each is passed to page.evaluate, so it must be
// self-contained: no closures over this module.
// ---------------------------------------------------------------------------

// pageShows reports whether text is anywhere on the page a user can reach it:
// in a text node, a form control's value, or an attribute (a member name is
// rendered into title and aria-label).
function pageShows(text) {
  if (document.body.textContent.includes(text)) return true;
  for (const el of document.querySelectorAll('*')) {
    if ((el.value !== undefined && String(el.value).includes(text))) return true;
    for (const attr of el.attributes) if (attr.value.includes(text)) return true;
  }
  return false;
}

// reversedNeighbours finds every text node containing needle and reports the
// text that FOLLOWS it in the same bidi paragraph if that text is laid out
// right-to-left — the visible symptom of a U+202E override leaking out of the
// field it was typed into. A block container (or a blockified flex item)
// starts a new paragraph, which is where an unterminated override stops.
function reversedNeighbours(needle) {
  const paragraphOf = (node) => {
    let el = node.parentElement;
    while (el && el !== document.body && getComputedStyle(el).display === 'inline') el = el.parentElement;
    return el;
  };
  const charRect = (textNode, i) => {
    const range = document.createRange();
    range.setStart(textNode, i);
    range.setEnd(textNode, i + 1);
    return range.getBoundingClientRect();
  };
  const hits = [];
  const all = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  for (let n = all.nextNode(); n; n = all.nextNode()) if (n.data.includes(needle)) hits.push(n);

  const reversed = [];
  for (const hit of hits) {
    const paragraph = paragraphOf(hit);
    const walker = document.createTreeWalker(paragraph, NodeFilter.SHOW_TEXT);
    walker.currentNode = hit;
    for (let t = walker.nextNode(); t; t = walker.nextNode()) {
      if (paragraphOf(t) !== paragraph) continue;
      const first = t.data.search(/\S/);
      const last = t.data.length - 1 - [...t.data].reverse().join('').search(/\S/);
      if (first < 0 || last - first < 1) continue;
      const a = charRect(t, first);
      const b = charRect(t, last);
      if (!a.width || !b.width) continue;
      const sameLine = Math.abs(a.top - b.top) < a.height / 2;
      if (sameLine && a.left > b.left) reversed.push(t.data.trim().slice(0, 40));
    }
  }
  return { hits: hits.length, reversed };
}

// overflowingElements reports every element whose own text contains word and
// whose visible box pushes past the viewport or makes a scroll container
// scroll sideways — "no horizontal scroll" measured, not eyeballed. An
// ancestor that clips (overflow hidden, as a truncating label does) bounds the
// visible box: clipped text is not a horizontal scroll.
function overflowingElements(word) {
  const viewport = document.documentElement.clientWidth;
  const out = [];
  for (const el of document.querySelectorAll('body *')) {
    const own = [...el.childNodes].some((c) => c.nodeType === Node.TEXT_NODE && c.data.includes(word));
    if (!own) continue;
    const rect = el.getBoundingClientRect();
    if (!rect.width || !rect.height) continue;
    let visibleRight = rect.right;
    for (let a = el.parentElement; a && a !== document.documentElement; a = a.parentElement) {
      const overflowX = getComputedStyle(a).overflowX;
      if (overflowX === 'auto' || overflowX === 'scroll') {
        if (a.scrollWidth > a.clientWidth + 1) out.push(`${el.tagName} scrolls its container sideways`);
      }
      if (overflowX !== 'visible') visibleRight = Math.min(visibleRight, a.getBoundingClientRect().right);
    }
    if (visibleRight > viewport + 1) out.push(`${el.tagName} right edge ${Math.round(visibleRight)} > viewport ${viewport}`);
  }
  return out;
}

// elementsWithAttribute counts elements carrying an attribute by name — an
// injected event-handler attribute is exactly an element that has one.
function elementsWithAttribute(name) {
  return document.querySelectorAll(`[${name}]`).length;
}

module.exports = {
  TEXT_FIELDS,
  UNREACHABLE_TEXT_FIELDS,
  numericFields,
  submitNumber,
  renderedFields,
  createWith,
  expectedStored,
  cleanupField,
  pageShows,
  reversedNeighbours,
  overflowingElements,
  elementsWithAttribute,
  csrfFor,
  submit,
  withField,
  storedValues,
  uniqueMarker,
  householdA,
  memberId,
  isoDate,
};
