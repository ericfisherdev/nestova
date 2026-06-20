package adapter

// Postgres SQLSTATE codes the subscription adapter maps to domain sentinels.
const foreignKeyViolation = "23503"

// FK constraint names on the subscription table (00014). The household FK is an
// inline column reference, so Postgres auto-names it <table>_<column>_fkey; the
// payer FK is the explicitly named composite tenant constraint.
const (
	// subscriptionHouseholdFK is the auto-named FK subscription.household_id ->
	// household(id); a violation means the household does not exist.
	subscriptionHouseholdFK = "subscription_household_id_fkey"
	// subscriptionPayerFK is the named composite FK subscription -> member; a
	// violation means the payer is not a member of the household.
	subscriptionPayerFK = "subscription_payer_fk"
)
