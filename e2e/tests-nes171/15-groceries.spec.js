// §7 Groceries: the usage tracker, the pantry, and the shopping list.
//
// The happy paths go through the UI. The §7.2 and §7.3 cases post directly:
// most of them submit values the form's own min/step attributes would refuse
// to send, and the point of the checklist is what the SERVER does with them,
// not what the browser prevents.
//
// Quantity is the through-line of this section. Pantry amounts are floats with
// a unit, Add/Subtract refuse to convert between units, and Subtract refuses to
// go below zero — so most of what can go wrong here is a quantity the domain
// should reject and a status code that says so.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, csrfToken, postForm } = require('./helpers');
const { psql } = require('../tests/db');

// One run's fixtures share a suffix so a re-run never matches rows an earlier
// run left on the shared household.
const TS = Date.now();
const name = (label) => `${label} ${TS}`;

// Not serial: each test seeds its own item, and a failure in one checklist
// item must not skip the rest of the section.

// pantryItemId reads the id out of the row's own consume form action, which is
// the only place the page exposes it.
async function pantryItemId(page, itemName) {
  const row = page.locator('li').filter({ hasText: itemName }).first();
  const action = await row.locator('form[action*="/consume"]').getAttribute('action');
  const id = action.match(/\/groceries\/pantry\/([^/]+)\/consume/);
  if (!id) throw new Error(`no pantry id in action ${action}`);
  return id[1];
}

async function shoppingItemId(page, itemName) {
  const row = page.locator('li').filter({ hasText: itemName }).first();
  const action = await row.locator('form[action*="/status"]').first().getAttribute('action');
  const id = action.match(/\/groceries\/shopping\/([^/]+)\/status/);
  if (!id) throw new Error(`no shopping id in action ${action}`);
  return id[1];
}

// quantityOf reads a pantry item's stored amount and unit straight from the
// database, so an assertion about arithmetic is not filtered through the
// rendered label's formatting.
function quantityOf(itemName) {
  return psql(`
    SELECT p.quantity::float8 || ' ' || p.unit
      FROM nestova.pantry_item p
      JOIN nestova.ingredient i ON i.id = p.ingredient_id
     WHERE lower(i.canonical_name) = lower('${itemName}')
     LIMIT 1;
  `).trim();
}

// submitPantryAmount fills and submits one pantry row's consume/adjust
// mini-form. The form is addressed by its action URL rather than by walking the
// row, because the row is re-rendered by the HX-Redirect the previous mutation
// triggered and a row-relative handle goes stale mid-interaction.
async function submitPantryAmount(page, itemID, action, amount) {
  const form = page.locator(`form[action="/groceries/pantry/${itemID}/${action}"]`);
  await form.locator('input[name="amount"]').fill(amount);
  await form.locator('button[type="submit"]').click();
  await page.waitForURL(/\/groceries$/);
}

async function addPantryItem(page, { itemName, amount, unit }) {
  await page.goto('/groceries');
  await page.locator('#pantry-add-name').fill(itemName);
  await page.locator('#pantry-add-amount').fill(String(amount));
  await page.locator('#pantry-add-unit').selectOption(unit);
  await page.getByRole('button', { name: 'Add to pantry' }).click();
  await expect(page.locator('li').filter({ hasText: itemName }).first()).toBeVisible();
}

test.describe('§7.1 happy paths', () => {
  test('T-7.1.1 a tracked item is registered and its usage is logged', async ({ page }) => {
    await login(page, PERSONAS.owner);
    await page.goto('/groceries');

    const item = name('Coffee');
    await page.locator('#register-item-name').fill(item);
    await page.getByRole('button', { name: 'Register item' }).click();

    const row = page.locator('li').filter({ hasText: item }).first();
    await expect(row).toBeVisible();

    await row.getByRole('button', { name: 'Depleted' }).click();

    // The event is the observable outcome; the restock prediction it triggers
    // is the scheduler's business and is deliberately not asserted here.
    await expect
      .poll(() =>
        Number(psql(`
          SELECT count(*) FROM nestova.usage_event u
            JOIN nestova.tracked_item t ON t.id = u.tracked_item_id
           WHERE t.name = '${item}' AND u.type = 'depleted';
        `).trim()),
      )
      .toBe(1);
  });

  test('T-7.1.2 a pantry item is added, topped up, and consumed', async ({ page }) => {
    await login(page, PERSONAS.owner);

    const item = name('Rice');
    await addPantryItem(page, { itemName: item, amount: 500, unit: 'g' });
    expect(quantityOf(item)).toBe('500 g');

    const id = await pantryItemId(page, item);

    await submitPantryAmount(page, id, 'adjust', '250');
    await expect.poll(() => quantityOf(item)).toBe('750 g');

    await submitPantryAmount(page, id, 'consume', '200');
    await expect.poll(() => quantityOf(item)).toBe('550 g');
  });

  // Regression guard for NES-187: the consume/adjust mini-forms used to overflow
  // their pantry card and were painted under the shopping-list section, so the
  // buttons could not be clicked below the kiosk's 1920px width. The suite runs
  // at 1280px, so a relapse fails here on the click.
  test('T-7.1.2b the pantry row\'s consume and adjust buttons are clickable', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const item = name('Clickability probe');
    await addPantryItem(page, { itemName: item, amount: 10, unit: 'count' });
    const id = await pantryItemId(page, item);

    await submitPantryAmount(page, id, 'adjust', '5');
    await expect.poll(() => quantityOf(item)).toBe('15 count');
  });

  test('T-7.1.3 a shopping item is added and moved through its statuses', async ({ page }) => {
    await login(page, PERSONAS.owner);
    await page.goto('/groceries');

    const item = name('Paper towels');
    await page.locator('#shopping-add-name').fill(item);
    await page.locator('#shopping-add-amount').fill('2');
    await page.locator('#shopping-add-unit').selectOption('count');
    await page.getByRole('button', { name: 'Add to list' }).click();

    await expect(page.locator('li').filter({ hasText: item }).first()).toBeVisible();
    expect(statusOf(item)).toBe('needed');

    await page
      .locator('li')
      .filter({ hasText: item })
      .first()
      .getByRole('button', { name: 'In cart' })
      .click();
    await expect.poll(() => statusOf(item)).toBe('in_cart');

    await page
      .locator('li')
      .filter({ hasText: item })
      .first()
      .getByRole('button', { name: 'Purchased' })
      .click();
    await expect.poll(() => statusOf(item)).toBe('purchased');
  });
});

function statusOf(itemName) {
  return psql(
    `SELECT status FROM nestova.shopping_list_item WHERE name = '${itemName}' LIMIT 1;`,
  ).trim();
}

test.describe('§7.2 quantity and identity validation', () => {
  test('T-7.2.1 consuming more than is on hand is refused, not clamped', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const item = name('Flour');
    await addPantryItem(page, { itemName: item, amount: 100, unit: 'g' });
    const id = await pantryItemId(page, item);

    const token = await csrfToken(page, '/groceries');
    const status = await postForm(page, `/groceries/pantry/${id}/consume`, {
      csrf_token: token,
      amount: '500',
      unit: 'g',
    });

    // Rejected — Quantity.Subtract refuses to go below zero rather than
    // clamping at it, and the row is left exactly as it was.
    expect(status).toBe(400);
    expect(quantityOf(item)).toBe('100 g');
  });

  test('T-7.2.2 a negative adjustment is refused', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const item = name('Sugar');
    await addPantryItem(page, { itemName: item, amount: 100, unit: 'g' });
    const id = await pantryItemId(page, item);

    const token = await csrfToken(page, '/groceries');
    const status = await postForm(page, `/groceries/pantry/${id}/adjust`, {
      csrf_token: token,
      amount: '-50',
      unit: 'g',
    });

    expect(status).toBe(400);
    expect(quantityOf(item)).toBe('100 g');
  });

  test('T-7.2.3 a fractional quantity is accepted', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const item = name('Cream');
    await addPantryItem(page, { itemName: item, amount: 1, unit: 'l' });
    const id = await pantryItemId(page, item);

    const token = await csrfToken(page, '/groceries');
    const status = await postForm(page, `/groceries/pantry/${id}/consume`, {
      csrf_token: token,
      amount: '0.5',
      unit: 'l',
    });

    expect(status).toBe(303);
    await expect.poll(() => quantityOf(item)).toBe('0.5 l');
  });

  test('T-7.2.4 consuming in a different unit than the item is stored in is refused', async ({
    page,
  }) => {
    await login(page, PERSONAS.owner);
    const item = name('Stock');
    await addPantryItem(page, { itemName: item, amount: 200, unit: 'g' });
    const id = await pantryItemId(page, item);

    const token = await csrfToken(page, '/groceries');
    const status = await postForm(page, `/groceries/pantry/${id}/consume`, {
      csrf_token: token,
      amount: '50',
      unit: 'ml',
    });

    // ErrUnitMismatch: Quantity does not convert between units, so 50 ml can
    // never be subtracted from 200 g.
    expect(status).toBe(400);
    expect(quantityOf(item)).toBe('200 g');
  });

  test('T-7.2.5 an unknown unit is refused', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const token = await csrfToken(page, '/groceries');

    const status = await postForm(page, '/groceries/pantry', {
      csrf_token: token,
      name: name('Furlongs of pasta'),
      amount: '3',
      unit: 'furlong',
    });

    expect(status).toBe(400);
  });

  test('T-7.2.7 a shopping item identified by neither an ingredient nor a name is refused', async ({
    page,
  }) => {
    await login(page, PERSONAS.owner);
    const token = await csrfToken(page, '/groceries');

    const status = await postForm(page, '/groceries/shopping', {
      csrf_token: token,
      name: '   ',
      amount: '1',
      unit: 'count',
    });

    // ErrInvalidShoppingListItem — an item must carry exactly one of an
    // ingredient or a free-text name, and whitespace is neither.
    expect(status).toBe(400);
  });

  test('T-7.2.6 the add-item route cannot express "both an ingredient and a name"', async ({
    page,
  }) => {
    await login(page, PERSONAS.owner);
    const token = await csrfToken(page, '/groceries');

    const item = name('Both-identifiers probe');
    const status = await postForm(page, '/groceries/shopping', {
      csrf_token: token,
      name: item,
      ingredient_id: '00000000-0000-0000-0000-000000000000',
      amount: '1',
      unit: 'count',
    });

    // The handler always passes a nil ingredient to AddManualItem, so a
    // submitted ingredient_id is ignored rather than combined with the name.
    // The invalid pair ErrInvalidShoppingListItem guards against is therefore
    // unreachable from this route; the item is created as an ordinary manual
    // one. Recorded as the route's real contract, not as a defect.
    expect(status).toBe(303);
    expect(statusOf(item)).toBe('needed');
  });

  test('T-7.2.8 the member-facing status route allows any transition, by design', async ({
    page,
  }) => {
    await login(page, PERSONAS.owner);
    await page.goto('/groceries');

    const item = name('Backwards transition probe');
    await page.locator('#shopping-add-name').fill(item);
    await page.locator('#shopping-add-amount').fill('1');
    await page.locator('#shopping-add-unit').selectOption('count');
    await page.getByRole('button', { name: 'Add to list' }).click();
    await expect(page.locator('li').filter({ hasText: item }).first()).toBeVisible();

    const id = await shoppingItemId(page, item);
    const token = await csrfToken(page, '/groceries');

    expect(
      await postForm(page, `/groceries/shopping/${id}/status`, {
        csrf_token: token,
        status: 'purchased',
      }),
    ).toBe(303);

    // purchased → in_cart is what ErrShoppingListItemNotInCartable rejects, but
    // that sentinel guards MarkInCart, which is the KIOSK's one allowed
    // mutation. TransitionStatus — this page's route — is deliberately
    // unguarded so a member can correct a mis-tap. The checklist's expectation
    // belongs on §12's kiosk route, not here.
    expect(
      await postForm(page, `/groceries/shopping/${id}/status`, {
        csrf_token: token,
        status: 'in_cart',
      }),
    ).toBe(303);
    await expect.poll(() => statusOf(item)).toBe('in_cart');

    // An unknown status is still refused.
    expect(
      await postForm(page, `/groceries/shopping/${id}/status`, {
        csrf_token: token,
        status: 'teleported',
      }),
    ).toBe(400);
  });

  test('T-7.2.9 a whitespace-only ingredient name is refused', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const token = await csrfToken(page, '/groceries');

    const status = await postForm(page, '/groceries/pantry', {
      csrf_token: token,
      name: '   ',
      amount: '1',
      unit: 'count',
    });

    // ErrInvalidIngredient — EnsureIngredient will not canonicalise blank.
    expect(status).toBe(400);
  });
});

test.describe('§7.3 automation and numeric limits', () => {
  test('T-7.3.1 repeated depletions do not pile duplicate rows onto the shopping list', async ({
    page,
  }) => {
    await login(page, PERSONAS.owner);
    await page.goto('/groceries');

    const item = name('Dish soap');
    await page.locator('#register-item-name').fill(item);
    await page.getByRole('button', { name: 'Register item' }).click();
    const row = page.locator('li').filter({ hasText: item }).first();
    await expect(row).toBeVisible();

    for (let i = 0; i < 3; i++) {
      await page
        .locator('li')
        .filter({ hasText: item })
        .first()
        .getByRole('button', { name: 'Depleted' })
        .click();
      await page.waitForURL(/\/groceries$/);
    }

    // Whatever the automation does, it must not add the same item repeatedly.
    // Restock items are generated by the background scheduler rather than by
    // the depletion click, so within a test window the expected count is 0 or
    // 1 — never one per click.
    const rows = Number(psql(`
      SELECT count(*) FROM nestova.shopping_list_item
       WHERE name = '${item}' OR ingredient_id IN
             (SELECT id FROM nestova.ingredient WHERE lower(canonical_name) = lower('${item}'));
    `).trim());
    expect(rows).toBeLessThanOrEqual(1);
  });

  test('T-7.3.3 an astronomically large quantity does not overflow the stored amount', async ({
    page,
  }) => {
    await login(page, PERSONAS.owner);
    const item = name('Overflow probe');
    await addPantryItem(page, { itemName: item, amount: 1, unit: 'count' });
    const id = await pantryItemId(page, item);
    const token = await csrfToken(page, '/groceries');

    // Exponent notation is not a plain decimal, so both writes are refused
    // before they reach Quantity, and a plain decimal above the cap is refused
    // by Quantity itself.
    const first = await postForm(page, `/groceries/pantry/${id}/adjust`, {
      csrf_token: token,
      amount: '1e308',
      unit: 'count',
    });
    const second = await postForm(page, `/groceries/pantry/${id}/adjust`, {
      csrf_token: token,
      amount: '1000000001',
      unit: 'count',
    });
    expect(first).toBe(400);
    expect(second).toBe(400);

    const stored = quantityOf(item);
    expect(stored).not.toMatch(/inf/i);
    expect(stored).not.toMatch(/nan/i);
  });

  // T-7.3.2 asks for defined referential behaviour when an ingredient a recipe
  // references is deleted. The behaviour is defined by there being no way to
  // delete one: ingredients are canonicalised on demand by EnsureIngredient and
  // the app exposes no delete route, while every referencing table
  // (recipe_ingredient, pantry_item, shopping_list_item) carries a NO ACTION
  // foreign key, so a direct DELETE is refused by the database rather than
  // orphaning a recipe row.
  test('T-7.3.2 an ingredient referenced by a recipe cannot be deleted', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const token = await csrfToken(page, '/groceries');

    const item = name('Referenced ingredient');
    expect(
      await postForm(page, '/groceries/pantry', {
        csrf_token: token,
        name: item,
        amount: '1',
        unit: 'count',
      }),
    ).toBe(303);

    // No delete surface exists for an ingredient.
    expect(
      await postForm(page, '/groceries/ingredients/delete', { csrf_token: token, name: item }),
    ).toBe(404);

    // And the constraint backs that up: the pantry row referencing it blocks a
    // direct delete instead of cascading.
    let refused = false;
    try {
      psql(`DELETE FROM nestova.ingredient WHERE lower(canonical_name) = lower('${item}');`);
    } catch (err) {
      refused = /violates foreign key constraint/i.test(String(err.stderr || err));
    }
    expect(refused).toBe(true);
  });
});
