const { test, expect } = require('@playwright/test');

// E2E coverage for Meals & Recipes (NES-62): recipe box, weekly planner, finder.
// storageState makes every test start logged in; bodies just goto('/meals').
//
// Robust scoping (learned the hard way):
// - The CREATE form is uniquely form[action="/meals/recipes"] (edit posts to
//   /meals/recipes/{id}, delete to /.../delete), so we never disambiguate the
//   shared "e.g. Pancakes" title placeholder across add + edit forms.
// - Section/card scoping anchors on heading/title TEXT then walks to the
//   enclosing element. The earlier `locator('section',{has:getByRole('heading')})`
//   hit an Alpine-hydration race where the heading role wasn't resolvable yet.
// - Every /meals POST returns HX-Redirect /meals, so after a submit we assert on
//   the re-rendered page (auto-retried by expect()).

function uniqueTitle(prefix) {
  return `${prefix} ${Date.now()}`;
}

const addForm = (page) => page.locator('form[action="/meals/recipes"]');
const plannerSection = (page) => page.getByText('Weekly planner').locator('xpath=ancestor::section[1]');
const finderSection = (page) => page.getByText('What can I make?').locator('xpath=ancestor::section[1]');

// recipeCard finds the recipe-box card for a title (a bordered div that also
// carries the per-recipe Delete button — distinguishing it from planner slots).
function recipeCard(page, title) {
  return page
    .locator('div.rounded-control')
    .filter({ has: page.getByText(title, { exact: true }) })
    .filter({ has: page.getByRole('button', { name: 'Delete' }) })
    .first();
}

// addRecipe opens the "Add a recipe" disclosure, fills the create form (one
// seeded ingredient line), submits, and waits for the new card to render.
async function addRecipe(page, { title, servings = '4', instructions = 'Mix and cook.', ingredient = 'flour', amount = '2', unit = 'count' } = {}) {
  await page.locator('summary', { hasText: 'Add a recipe' }).click();
  const f = addForm(page);
  await f.locator('input[name="title"]').fill(title);
  await f.locator('input[name="servings"]').fill(servings);
  await f.locator('textarea[name="instructions"]').fill(instructions);
  await f.locator('input[name="ingredient_name"]').first().fill(ingredient);
  await f.locator('input[name="ingredient_amount"]').first().fill(amount);
  await f.locator('select[name="ingredient_unit"]').first().selectOption(unit);
  await f.getByRole('button', { name: 'Add recipe' }).click();
  await expect(recipeCard(page, title)).toBeVisible();
}

test('renders the recipe box, weekly planner, and finder panel', async ({ page }) => {
  await page.goto('/meals');

  await expect(page.getByRole('heading', { name: 'Meals & Recipes', level: 1 })).toBeVisible();
  // Use the heading role: "Recipe box" also appears as a substring in the page
  // subtitle ("Keep your recipe box…"), so getByText would be ambiguous.
  await expect(page.getByRole('heading', { name: 'Recipe box' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Weekly planner' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'What can I make?' })).toBeVisible();

  await expect(plannerSection(page).getByRole('button', { name: 'Assign' })).toBeVisible();
  await expect(plannerSection(page).getByRole('button', { name: 'Generate grocery list' })).toBeVisible();
  await expect(finderSection(page).getByRole('button', { name: 'Use my pantry' })).toBeVisible();
  await expect(finderSection(page).getByRole('button', { name: 'Find' })).toBeVisible();
});

test('creates a recipe and it appears in the box with servings + ingredient', async ({ page }) => {
  await page.goto('/meals');

  const title = uniqueTitle('E2E Created');
  await addRecipe(page, { title, servings: '3', ingredient: 'eggs', amount: '4' });

  const card = recipeCard(page, title);
  await expect(card.getByText('Serves 3')).toBeVisible();
  await expect(card.getByText(/eggs/i)).toBeVisible();
});

test('edits a recipe and the box reflects the update', async ({ page }) => {
  await page.goto('/meals');

  const title = uniqueTitle('E2E Edit');
  await addRecipe(page, { title, servings: '2' });

  const card = recipeCard(page, title);
  await card.locator('summary', { hasText: 'Edit' }).click();

  // The edit form is the only one in the card with a servings input + a
  // "Save changes" button (the sibling form is Delete), so locate directly.
  await card.locator('input[name="servings"]').fill('8');
  await card.getByRole('button', { name: 'Save changes' }).click();

  await expect(recipeCard(page, title).getByText('Serves 8')).toBeVisible();
});

test('deletes a recipe and it disappears from the box', async ({ page }) => {
  await page.goto('/meals');

  const title = uniqueTitle('E2E Delete');
  await addRecipe(page, { title });

  await recipeCard(page, title).getByRole('button', { name: 'Delete' }).click();
  await expect(page.getByText(title, { exact: true })).toHaveCount(0);
});

test('assigns a recipe to a planner slot and then clears it', async ({ page }) => {
  await page.goto('/meals');

  const title = uniqueTitle('E2E Planned');
  await addRecipe(page, { title });

  const assign = page.locator('form[action="/meals/plan"]');
  const firstDay = await assign.locator('select[name="date"] option').first().getAttribute('value');
  await assign.locator('select[name="date"]').selectOption(firstDay);
  await assign.locator('select[name="meal"]').selectOption('breakfast');
  // Select the recipe by the option whose text contains our title (label format
  // may include serving info), then submit by its value.
  const recipeValue = await assign.locator('select[name="recipe_id"] option', { hasText: title }).first().getAttribute('value');
  await assign.locator('select[name="recipe_id"]').selectOption(recipeValue);
  await assign.locator('input[name="servings"]').fill('2');
  await assign.getByRole('button', { name: 'Assign' }).click();

  // The planner grid now has a slot showing the recipe with a "clear" control.
  const filledSlot = () =>
    plannerSection(page)
      .locator('div.rounded-control')
      .filter({ has: page.getByText(title, { exact: true }) })
      .filter({ has: page.getByRole('button', { name: 'clear' }) });
  await expect(filledSlot().first()).toBeVisible();

  await filledSlot().first().getByRole('button', { name: 'clear' }).click();
  await expect(filledSlot()).toHaveCount(0);
});

test('finder runs against the pantry and renders a results region', async ({ page }) => {
  await page.goto('/meals');

  await finderSection(page).getByRole('button', { name: 'Use my pantry' }).click();

  const fs = finderSection(page);
  await expect(
    fs.getByText('No recipes match those ingredients yet.').or(fs.getByText(/% match/)).first(),
  ).toBeVisible();
});

test('finder runs against ad-hoc ingredients and echoes the query', async ({ page }) => {
  await page.goto('/meals');

  const title = uniqueTitle('E2E Finder');
  await addRecipe(page, { title, ingredient: 'flour', amount: '1' });

  const fs = finderSection(page);
  await fs.locator('input[name="ingredients"]').fill('flour, eggs, milk');
  await fs.getByRole('button', { name: 'Find' }).click();

  const fs2 = finderSection(page);
  await expect(
    fs2.getByText('No recipes match those ingredients yet.').or(fs2.getByText(/% match/)).first(),
  ).toBeVisible();
  await expect(fs2.locator('input[name="ingredients"]')).toHaveValue('flour, eggs, milk');
});
