package adapter

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Postgres SQLSTATE codes this adapter reacts to, mirroring media/adapter's
// own errors.go convention.
const (
	foreignKeyViolation = "23503"
	uniqueViolation     = "23505"
)

// federation_instance_link's constraint names (migration 00038).
// householdFK is the auto-named inline column FK on household_id; a
// violation means householdID itself does not exist. householdUniq is the
// explicitly named UNIQUE(household_id) constraint; a violation means the
// household already has a link. baseURLUniq is the unique index on
// lower(base_url); a violation means the (normalized) url is already bound
// to a different household.
const (
	instanceLinkHouseholdFK   = "federation_instance_link_household_id_fkey"
	instanceLinkHouseholdUniq = "federation_instance_link_household_id_uniq"
	instanceLinkBaseURLUniq   = "federation_instance_link_base_url_uniq"
)

// mapCreateError translates a Create failure into its domain sentinel, or
// wraps err unchanged when it is not a recognized violation.
func mapCreateError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}
	switch pgErr.Code {
	case foreignKeyViolation:
		if pgErr.ConstraintName == instanceLinkHouseholdFK {
			return household.ErrHouseholdNotFound
		}
	case uniqueViolation:
		switch pgErr.ConstraintName {
		case instanceLinkHouseholdUniq:
			return domain.ErrHouseholdAlreadyBound
		case instanceLinkBaseURLUniq:
			return domain.ErrInstanceAlreadyBound
		}
	}
	return nil
}
