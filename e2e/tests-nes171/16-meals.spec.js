// §8 Meals: the recipe box, the finder, and the week planner.
//
// The recipe form's ingredient lines are Alpine-driven repeatables, so the
// negative cases post directly — assembling N lines through the DOM would test
// Alpine, not the server. The happy paths stay on the UI.
//
// What the checklist calls "generate a week" does not exist: the planner
// assigns one (date, meal) slot at a time, and /meals/plan/generate turns the
// PLANNED week into shopping-list lines. The §8.2 cases about generation are
// written against that route, which is the feature that is actually built.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, csrfToken, postForm } = require('./helpers');
const { psql } = require('../tests/db');

const TS = Date.now();
const name = (label) => `${label} ${TS}`;

// mondayOf returns the ISO week start for today, in the app's own date format.
// The planner's grid is keyed by date, and its "generate" button carries the
// week start, so tests have to agree with it.
function weekStart() {
  const d = new Date();
  const day = (d.getDay() + 6) % 7; // Monday = 0
  d.setDate(d.getDate() - day);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

function isoDate(offsetDays = 0) {
  const [y, m, day] = weekStart().split('-').map(Number);
  const d = new Date(y, m - 1, day + offsetDays);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

function recipeId(title) {
  return psql(`SELECT id FROM nestova.recipe WHERE title = '${title}' LIMIT 1;`).trim();
}

function ingredientCount(title) {
  return Number(psql(`
    SELECT count(*) FROM nestova.recipe_ingredient ri
      JOIN nestova.recipe r ON r.id = ri.recipe_id
     WHERE r.title = '${title}';
  `).trim());
}

function planEntries() {
  return Number(psql(`SELECT count(*) FROM nestova.meal_plan_entry;`).trim());
}

function mealPlanShoppingLines() {
  return Number(psql(
    `SELECT count(*) FROM nestova.shopping_list_item WHERE source = 'meal_plan';`,
  ).trim());
}

// createRecipe posts a recipe with N ingredient lines. Repeated form keys are
// how the handler reads them (r.Form["ingredient_name"]), which URLSearchParams
// reproduces faithfully.
async function createRecipe(page, { title, servings = 4, ingredients = [] }) {
  const token = await csrfToken(page, '/meals');
  const fields = [
    ['csrf_token', token],
    ['title', title],
    ['servings', String(servings)],
    ['instructions', 'Combine and serve.'],
  ];
  for (const ing of ingredients) {
    fields.push(['ingredient_name', ing.name]);
    fields.push(['ingredient_amount', String(ing.amount)]);
    fields.push(['ingredient_unit', ing.unit]);
    fields.push(['ingredient_optional', 'false']);
  }
  return page.evaluate(async (pairs) => {
    const body = new URLSearchParams(pairs).toString();
    const res = await fetch('/meals/recipes', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body,
      redirect: 'manual',
    });
    return res.type === 'opaqueredirect' ? 303 : res.status;
  }, fields);
}

test.describe('§8.1 happy paths', () => {
  test('T-8.1.1 a recipe is added, edited, and deleted', async ({ page }) => {
    await login(page, PERSONAS.owner);
    await page.goto('/meals');

    const title = name('Pancakes');
    await page.getByText('Add a recipe').click();
    const form = page.locator('form[action="/meals/recipes"]');
    await form.locator('input[name="title"]').fill(title);
    await form.locator('input[name="servings"]').fill('4');
    await form.locator('input[name="ingredient_name"]').first().fill(name('Flour'));
    await form.locator('input[name="ingredient_amount"]').first().fill('300');
    await form.locator('select[name="ingredient_unit"]').first().selectOption('g');
    await form.getByRole('button', { name: 'Add recipe' }).click();

    // The title also appears as an <option> in the planner's recipe select, so
    // scope the assertion to the recipe box rather than the whole page.
    const recipeBox = page.locator('section, div').filter({ hasText: 'Recipe box' }).last();
    await expect(recipeBox.getByText(title, { exact: true }).first()).toBeVisible();

    // Edit: same form shape, posted to the recipe's own action.
    const id = recipeId(title);
    const edited = `${title} v2`;
    const token = await csrfToken(page, '/meals');
    expect(
      await postForm(page, `/meals/recipes/${id}`, {
        csrf_token: token,
        title: edited,
        servings: '6',
        instructions: 'Edited.',
        ingredient_name: name('Flour'),
        ingredient_amount: '400',
        ingredient_unit: 'g',
      }),
    ).toBe(303);
    await page.goto('/meals');
    await expect(page.getByText(edited, { exact: true }).first()).toBeVisible();

    // Addressed by the card's own delete form: filtering divs by text picks up
    // ancestors that do not contain the button.
    await page.locator(`form[action="/meals/recipes/${id}/delete"] button`).click();
    await page.waitForURL(/\/meals$/);
    await expect.poll(() => recipeId(edited)).toBe('');
  });

  test('T-8.1.2 the finder returns matches for what is on hand', async ({ page }) => {
    await login(page, PERSONAS.owner);

    // A recipe whose only ingredient is something the pantry will hold.
    const ingredient = name('Lentils');
    const title = name('Lentil soup');
    expect(
      await createRecipe(page, { title, ingredients: [{ name: ingredient, amount: 200, unit: 'g' }] }),
    ).toBe(303);

    const token = await csrfToken(page, '/groceries');
    expect(
      await postForm(page, '/groceries/pantry', {
        csrf_token: token,
        name: ingredient,
        amount: '500',
        unit: 'g',
      }),
    ).toBe(303);

    await page.goto('/meals');
    await page.getByRole('button', { name: 'Use my pantry' }).click();
    await expect(page.getByText(title, { exact: true }).first()).toBeVisible();
  });

  test('T-8.1.3 a meal is assigned to a slot and cleared again', async ({ page }) => {
    await login(page, PERSONAS.owner);

    const title = name('Assigned stew');
    expect(await createRecipe(page, { title, ingredients: [] })).toBe(303);
    const id = recipeId(title);
    const date = isoDate(1);
    const token = await csrfToken(page, '/meals');

    expect(
      await postForm(page, '/meals/plan', {
        csrf_token: token,
        date,
        meal: 'dinner',
        recipe_id: id,
        servings: '2',
      }),
    ).toBe(303);
    expect(
      Number(psql(`SELECT count(*) FROM nestova.meal_plan_entry WHERE plan_date = '${date}' AND meal = 'dinner';`).trim()),
    ).toBe(1);

    expect(
      await postForm(page, '/meals/plan/clear', { csrf_token: token, date, meal: 'dinner' }),
    ).toBe(303);
    expect(
      Number(psql(`SELECT count(*) FROM nestova.meal_plan_entry WHERE plan_date = '${date}' AND meal = 'dinner';`).trim()),
    ).toBe(0);
  });

  test("T-8.1.4 a planned week's ingredients reach the shopping list", async ({ page }) => {
    await login(page, PERSONAS.owner);

    const ingredient = name('Chickpeas');
    const title = name('Chana masala');
    expect(
      await createRecipe(page, { title, servings: 2, ingredients: [{ name: ingredient, amount: 400, unit: 'g' }] }),
    ).toBe(303);

    const token = await csrfToken(page, '/meals');
    expect(
      await postForm(page, '/meals/plan', {
        csrf_token: token,
        date: isoDate(2),
        meal: 'dinner',
        recipe_id: recipeId(title),
        servings: '4',
      }),
    ).toBe(303);

    expect(
      await postForm(page, '/meals/plan/generate', { csrf_token: token, week_start: weekStart() }),
    ).toBe(303);

    // Doubling the servings doubles the line: 400 g for 2 becomes 800 g for 4.
    const line = psql(`
      SELECT s.quantity::float8 || ' ' || s.unit
        FROM nestova.shopping_list_item s
        JOIN nestova.ingredient i ON i.id = s.ingredient_id
       WHERE lower(i.canonical_name) = lower('${ingredient}') AND s.source = 'meal_plan'
       LIMIT 1;
    `).trim();
    expect(line).toBe('800 g');
  });
});

test.describe('§8.2 recipe and planner edge cases', () => {
  test('T-8.2.1 a recipe with no ingredients is accepted', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const title = name('Toast');

    // Recipe.Validate requires a title, positive servings and a valid source —
    // it does not require any ingredient line, so an empty recipe is a legal
    // note-to-self rather than an error.
    expect(await createRecipe(page, { title, ingredients: [] })).toBe(303);
    expect(recipeId(title)).not.toBe('');
    expect(ingredientCount(title)).toBe(0);
  });

  test('T-8.2.2 a recipe with 200 ingredients is stored and rendered', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const title = name('Everything soup');
    const ingredients = Array.from({ length: 200 }, (_, i) => ({
      name: `${name('Filler')} ${i}`,
      amount: 1,
      unit: 'g',
    }));

    expect(await createRecipe(page, { title, ingredients })).toBe(303);
    expect(ingredientCount(title)).toBe(200);

    await page.goto('/meals');
    await expect(page.getByText(title, { exact: true }).first()).toBeVisible();
  });

  test('T-8.2.3 the same ingredient twice in one recipe is refused, not a 500', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const title = name('Double garlic');
    const ingredient = name('Garlic');

    const status = await createRecipe(page, {
      title,
      ingredients: [
        { name: ingredient, amount: 2, unit: 'count' },
        { name: ingredient, amount: 3, unit: 'count' },
      ],
    });

    // recipe_ingredient's primary key is (recipe_id, ingredient_id), so the
    // second line collides. Either the service merges the two lines or it
    // refuses the recipe with a 4xx — a 500 means the collision reached the
    // caller as a raw database error.
    expect(status).not.toBe(500);
    if (status === 303) {
      expect(ingredientCount(title)).toBe(1);
    } else {
      expect(status).toBe(400);
      expect(recipeId(title)).toBe('');
    }
  });

  test('T-8.2.4 deleting a planned recipe takes its plan entry with it', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const title = name('Doomed casserole');
    expect(await createRecipe(page, { title, ingredients: [] })).toBe(303);
    const id = recipeId(title);
    const date = isoDate(3);
    const token = await csrfToken(page, '/meals');

    expect(
      await postForm(page, '/meals/plan', {
        csrf_token: token,
        date,
        meal: 'lunch',
        recipe_id: id,
        servings: '2',
      }),
    ).toBe(303);

    expect(await postForm(page, `/meals/recipes/${id}/delete`, { csrf_token: token })).toBe(303);

    // meal_plan_entry_recipe_fk is ON DELETE CASCADE: the slot empties rather
    // than keeping a row pointing at a recipe that no longer exists.
    expect(
      Number(psql(`SELECT count(*) FROM nestova.meal_plan_entry WHERE recipe_id = '${id}';`).trim()),
    ).toBe(0);
  });

  test('T-8.2.5 generating from a part-filled week is fine', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const title = name('Single day');
    expect(
      await createRecipe(page, { title, ingredients: [{ name: name('Rice'), amount: 100, unit: 'g' }] }),
    ).toBe(303);
    const token = await csrfToken(page, '/meals');
    expect(
      await postForm(page, '/meals/plan', {
        csrf_token: token,
        date: isoDate(4),
        meal: 'dinner',
        recipe_id: recipeId(title),
        servings: '2',
      }),
    ).toBe(303);

    // One planned day out of seven: the other six contribute nothing and the
    // generation still succeeds.
    expect(
      await postForm(page, '/meals/plan/generate', { csrf_token: token, week_start: weekStart() }),
    ).toBe(303);
  });

  test('T-8.2.6 generating from an empty week succeeds and adds nothing', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const token = await csrfToken(page, '/meals');

    // A week far enough out that nothing this suite plans falls inside it.
    const before = mealPlanShoppingLines();
    expect(
      await postForm(page, '/meals/plan/generate', { csrf_token: token, week_start: isoDate(140) }),
    ).toBe(303);
    expect(mealPlanShoppingLines()).toBe(before);
  });

  test('T-8.2.7 generating twice does not duplicate the shopping lines', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const ingredient = name('Coconut milk');
    const title = name('Curry');
    expect(
      await createRecipe(page, { title, servings: 2, ingredients: [{ name: ingredient, amount: 1, unit: 'l' }] }),
    ).toBe(303);
    const token = await csrfToken(page, '/meals');
    expect(
      await postForm(page, '/meals/plan', {
        csrf_token: token,
        date: isoDate(5),
        meal: 'dinner',
        recipe_id: recipeId(title),
        servings: '2',
      }),
    ).toBe(303);

    const linesFor = () => Number(psql(`
      SELECT count(*) FROM nestova.shopping_list_item s
        JOIN nestova.ingredient i ON i.id = s.ingredient_id
       WHERE lower(i.canonical_name) = lower('${ingredient}');
    `).trim());

    expect(
      await postForm(page, '/meals/plan/generate', { csrf_token: token, week_start: weekStart() }),
    ).toBe(303);
    const afterFirst = linesFor();
    expect(afterFirst).toBe(1);

    expect(
      await postForm(page, '/meals/plan/generate', { csrf_token: token, week_start: weekStart() }),
    ).toBe(303);
    expect(linesFor()).toBe(1);
  });

  test('T-8.2.8 assigning a second meal to one slot replaces the first', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const first = name('Slot one');
    const second = name('Slot two');
    expect(await createRecipe(page, { title: first, ingredients: [] })).toBe(303);
    expect(await createRecipe(page, { title: second, ingredients: [] })).toBe(303);

    const date = isoDate(6);
    const token = await csrfToken(page, '/meals');
    const assign = (id) =>
      postForm(page, '/meals/plan', {
        csrf_token: token,
        date,
        meal: 'breakfast',
        recipe_id: id,
        servings: '2',
      });

    expect(await assign(recipeId(first))).toBe(303);
    expect(await assign(recipeId(second))).toBe(303);

    // meal_plan_entry_slot_uniq is UNIQUE (household, date, meal), and the
    // planner upserts: the slot holds exactly one entry, the newest.
    const held = psql(`
      SELECT r.title FROM nestova.meal_plan_entry e
        JOIN nestova.recipe r ON r.id = e.recipe_id
       WHERE e.plan_date = '${date}' AND e.meal = 'breakfast';
    `).trim();
    expect(held).toBe(second);
  });

  test('T-8.2.9 a plan entry for an invalid date is refused', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const title = name('Never planned');
    expect(await createRecipe(page, { title, ingredients: [] })).toBe(303);
    const token = await csrfToken(page, '/meals');

    for (const date of ['not-a-date', '2026-13-45', '']) {
      expect(
        await postForm(page, '/meals/plan', {
          csrf_token: token,
          date,
          meal: 'dinner',
          recipe_id: recipeId(title),
          servings: '2',
        }),
      ).toBe(400);
    }
  });

  test('T-8.2.10 the finder with an empty pantry returns an empty result, not an error', async ({
    page,
  }) => {
    await login(page, PERSONAS.owner);

    // Emptied for this test only: the finder's contract is about zero on-hand
    // ingredients, which is otherwise unreachable on a shared household.
    psql('DELETE FROM nestova.pantry_item;');

    await page.goto('/meals');
    await page.getByRole('button', { name: 'Use my pantry' }).click();
    await expect(page).toHaveURL(/\/meals/);
    await expect(page.getByRole('heading', { name: 'Meals', level: 1 })).toBeVisible();
  });
});

test.describe('§8.3 idempotence', () => {
  test('T-8.3.1 clearing an already-empty slot succeeds', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const token = await csrfToken(page, '/meals');
    const date = isoDate(0);

    const before = planEntries();
    // ClearMeal swallows ErrMealPlanEntryNotFound on purpose: the goal is an
    // empty slot, and it already is one.
    expect(
      await postForm(page, '/meals/plan/clear', { csrf_token: token, date, meal: 'snack' }),
    ).toBe(303);
    expect(
      await postForm(page, '/meals/plan/clear', { csrf_token: token, date, meal: 'snack' }),
    ).toBe(303);
    expect(planEntries()).toBe(before);
  });

  test('T-8.3.2 planning to shopping twice adds no duplicate lines', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const token = await csrfToken(page, '/meals');

    const before = mealPlanShoppingLines();
    expect(
      await postForm(page, '/meals/plan/generate', { csrf_token: token, week_start: weekStart() }),
    ).toBe(303);
    const afterFirst = mealPlanShoppingLines();
    expect(
      await postForm(page, '/meals/plan/generate', { csrf_token: token, week_start: weekStart() }),
    ).toBe(303);

    // AddMealPlanIfAbsent is what makes the second run a no-op.
    expect(mealPlanShoppingLines()).toBe(afterFirst);
    expect(afterFirst).toBeGreaterThanOrEqual(before);
  });
});
