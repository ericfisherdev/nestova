package adapter

const (
	// foreignKeyViolation is the PostgreSQL SQLSTATE for a foreign-key violation.
	foreignKeyViolation = "23503"

	// trackedItemHouseholdFK is the auto-named FK tracked_item.household_id ->
	// household.id; a violation means the household does not exist.
	trackedItemHouseholdFK = "tracked_item_household_id_fkey"
	// usageEventTrackedItemFK is the named composite FK usage_event ->
	// tracked_item; a violation means the tracked item does not exist (in the
	// event's household).
	usageEventTrackedItemFK = "usage_event_tracked_item_fk"
	// usageEventMemberFK is the named composite FK usage_event -> member; a
	// violation means the member does not exist in the event's household.
	usageEventMemberFK = "usage_event_member_fk"
	// restockPredictionTrackedItemFK is the auto-named FK
	// restock_prediction.tracked_item_id -> tracked_item.id; a violation means
	// the tracked item does not exist.
	restockPredictionTrackedItemFK = "restock_prediction_tracked_item_id_fkey"
	// pantryItemHouseholdFK is the auto-named FK pantry_item.household_id ->
	// household.id; a violation means the household does not exist.
	pantryItemHouseholdFK = "pantry_item_household_id_fkey"
	// pantryItemIngredientFK is the auto-named FK pantry_item.ingredient_id ->
	// ingredient.id; a violation means the ingredient does not exist.
	pantryItemIngredientFK = "pantry_item_ingredient_id_fkey"
)
