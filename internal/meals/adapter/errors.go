package adapter

const (
	// foreignKeyViolation is the PostgreSQL SQLSTATE for a foreign-key violation.
	foreignKeyViolation = "23503"

	// recipeHouseholdFK is the auto-named FK recipe.household_id -> household.id; a
	// violation means the owning household does not exist.
	recipeHouseholdFK = "recipe_household_id_fkey"
	// recipeIngredientRecipeFK is the auto-named FK recipe_ingredient.recipe_id ->
	// recipe.id; a violation means the recipe does not exist.
	recipeIngredientRecipeFK = "recipe_ingredient_recipe_id_fkey"
	// recipeIngredientIngredientFK is the auto-named FK recipe_ingredient.ingredient_id
	// -> ingredient.id; a violation means the catalogue ingredient does not exist.
	recipeIngredientIngredientFK = "recipe_ingredient_ingredient_id_fkey"
	// mealPlanEntryHouseholdFK is the auto-named FK meal_plan_entry.household_id ->
	// household.id; a violation means the household does not exist.
	mealPlanEntryHouseholdFK = "meal_plan_entry_household_id_fkey"
	// mealPlanEntryRecipeFK is the named composite FK meal_plan_entry ->
	// recipe(household_id, id); a violation means the recipe is not a box recipe of
	// the entry's household (so external recipes and other households' recipes are
	// unplannable).
	mealPlanEntryRecipeFK = "meal_plan_entry_recipe_fk"
)
