package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

const (
	// foreignKeyViolation is the PostgreSQL SQLSTATE for a foreign-key violation.
	foreignKeyViolation = "23503"
	// quietHoursHouseholdFK is the auto-named FK
	// notification_quiet_hours.household_id -> identity.household(id); a
	// violation means householdID does not exist.
	quietHoursHouseholdFK = "notification_quiet_hours_household_id_fkey"
)

// QuietHoursRepository is the pgx-backed implementation of
// domain.QuietHoursReader and domain.QuietHoursWriter, over notify's own
// notification_quiet_hours table (NSTR-115) — quiet hours rehomed here
// from the household context, keyed by the identity household id, once
// household itself stopped owning a row nestova could attach columns to.
type QuietHoursRepository struct {
	pool *pgxpool.Pool
}

// Compile-time assurance the adapter satisfies both ports.
var (
	_ domain.QuietHoursReader = (*QuietHoursRepository)(nil)
	_ domain.QuietHoursWriter = (*QuietHoursRepository)(nil)
)

// NewQuietHoursRepository constructs the repository with an injected pgx pool.
func NewQuietHoursRepository(pool *pgxpool.Pool) *QuietHoursRepository {
	if pool == nil {
		panic("adapter: NewQuietHoursRepository requires a non-nil pool")
	}
	return &QuietHoursRepository{pool: pool}
}

// GetQuietHours returns householdID's quiet-hours window. A household with
// no stored row has quiet hours disabled (both bounds nil) — there is no
// not-found sentinel here (see domain.QuietHoursReader's own doc).
func (r *QuietHoursRepository) GetQuietHours(ctx context.Context, householdID household.HouseholdID) (domain.QuietHours, error) {
	const q = `
		SELECT quiet_hours_start, quiet_hours_end
		  FROM notification_quiet_hours
		 WHERE household_id = $1`

	var start, end pgtype.Time
	err := r.pool.QueryRow(ctx, q, householdID.String()).Scan(&start, &end)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.QuietHours{HouseholdID: householdID}, nil
		}
		return domain.QuietHours{}, fmt.Errorf("get quiet hours: %w", err)
	}
	return domain.QuietHours{
		HouseholdID: householdID,
		Start:       pgTimeToDuration(start),
		End:         pgTimeToDuration(end),
	}, nil
}

// SetQuietHours upserts householdID's quiet-hours window. Passing nil for
// both start and end disables quiet hours. Returns an error when exactly
// one of start/end is nil — domain.QuietHours' own doc states both nil
// means disabled, so a half-set pair has no defined meaning. Returns
// household.ErrHouseholdNotFound when householdID does not exist.
func (r *QuietHoursRepository) SetQuietHours(ctx context.Context, householdID household.HouseholdID, start, end *time.Duration) error {
	if (start == nil) != (end == nil) {
		return fmt.Errorf("set quiet hours: start and end must both be set or both be nil")
	}
	const q = `
		INSERT INTO notification_quiet_hours (household_id, quiet_hours_start, quiet_hours_end)
		VALUES ($1, $2, $3)
		ON CONFLICT (household_id) DO UPDATE
		   SET quiet_hours_start = $2, quiet_hours_end = $3, updated_at = now()`

	_, err := r.pool.Exec(ctx, q, householdID.String(), durationToPgTime(start), durationToPgTime(end))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == quietHoursHouseholdFK {
			return household.ErrHouseholdNotFound
		}
		return fmt.Errorf("set quiet hours: %w", err)
	}
	return nil
}

// durationToPgTime converts a duration-since-midnight to the pgtype.Time
// wire representation Postgres' time column expects, or an invalid
// (NULL-bound) pgtype.Time when d is nil.
func durationToPgTime(d *time.Duration) pgtype.Time {
	if d == nil {
		return pgtype.Time{}
	}
	return pgtype.Time{Microseconds: d.Microseconds(), Valid: true}
}

// pgTimeToDuration converts a scanned pgtype.Time back to a
// duration-since-midnight, or nil when the column was NULL.
func pgTimeToDuration(t pgtype.Time) *time.Duration {
	if !t.Valid {
		return nil
	}
	d := time.Duration(t.Microseconds) * time.Microsecond
	return &d
}
