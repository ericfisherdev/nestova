package adapter

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// foreignKeyViolation is the Postgres SQLSTATE for a foreign-key violation.
const foreignKeyViolation = "23503"

// Inline household_id column references on kiosk_device and
// kiosk_activation_code; Postgres auto-names them <table>_<column>_fkey.
const (
	kioskDeviceHouseholdFK         = "kiosk_device_household_id_fkey"
	kioskActivationCodeHouseholdFK = "kiosk_activation_code_household_id_fkey"
)

// mapFKViolation maps a kiosk FK violation to its domain sentinel, or nil
// when err is not a recognized FK violation.
func mapFKViolation(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != foreignKeyViolation {
		return nil
	}
	switch pgErr.ConstraintName {
	case kioskDeviceHouseholdFK, kioskActivationCodeHouseholdFK:
		return household.ErrHouseholdNotFound
	default:
		return nil
	}
}

// row is the read surface shared by pgx.Row and pgx.Rows for scan helpers.
type row interface {
	Scan(dest ...any) error
}
