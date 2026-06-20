package adapter

// Postgres SQLSTATE codes the calendar adapter maps to domain sentinels.
const foreignKeyViolation = "23503"

// FK constraint names on the calendar_account table (00016). The household FK is
// an inline column reference, so Postgres auto-names it <table>_<column>_fkey;
// the member FK is the explicitly named composite tenant constraint.
const (
	// calendarAccountHouseholdFK is the auto-named FK calendar_account.household_id
	// -> household(id); a violation means the household does not exist.
	calendarAccountHouseholdFK = "calendar_account_household_id_fkey"
	// calendarAccountMemberFK is the named composite FK calendar_account -> member;
	// a violation means the member is not part of the household.
	calendarAccountMemberFK = "calendar_account_member_fk"
)
