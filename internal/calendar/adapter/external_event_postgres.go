package adapter

import (
	"context"
	"fmt"
	"time"

	"github.com/ericfisherdev/nestova/internal/calendar/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// ExternalEventRepository is the pgx-backed domain.ExternalEventRepository for
// the external-event cache.
type ExternalEventRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.ExternalEventRepository = (*ExternalEventRepository)(nil)

// NewExternalEventRepository constructs the repository with an injected query
// executor (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewExternalEventRepository(dbtx db.TX) *ExternalEventRepository {
	if dbtx == nil {
		panic("adapter: NewExternalEventRepository requires a non-nil db.TX")
	}
	return &ExternalEventRepository{dbtx: dbtx}
}

// UpsertByExternalID inserts or replaces the cache row for
// (calendar_account_id, external_id). It is idempotent and preserves the
// existing row's id on conflict.
func (r *ExternalEventRepository) UpsertByExternalID(ctx context.Context, event *domain.ExternalEvent) error {
	const q = `
		INSERT INTO external_event
			(id, calendar_account_id, external_id, title, starts_at, ends_at, all_day, color)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (calendar_account_id, external_id) DO UPDATE
		   SET title = EXCLUDED.title, starts_at = EXCLUDED.starts_at,
		       ends_at = EXCLUDED.ends_at, all_day = EXCLUDED.all_day,
		       color = EXCLUDED.color, updated_at = now()`
	if _, err := r.dbtx.Exec(ctx, q,
		event.ID.String(), event.CalendarAccountID.String(), event.ExternalID, event.Title,
		event.StartsAt, event.EndsAt, event.AllDay, colorArg(event.Color),
	); err != nil {
		return fmt.Errorf("upsert external event: %w", err)
	}
	return nil
}

// DeleteByExternalID removes the cached event for a provider event id. It is a
// no-op (no error) when the event is not cached.
func (r *ExternalEventRepository) DeleteByExternalID(ctx context.Context, accountID domain.CalendarAccountID, externalID string) error {
	const q = `DELETE FROM external_event WHERE calendar_account_id = $1 AND external_id = $2`
	if _, err := r.dbtx.Exec(ctx, q, accountID.String(), externalID); err != nil {
		return fmt.Errorf("delete external event: %w", err)
	}
	return nil
}

// ListByHouseholdRange returns the household's cached events whose start falls in
// [from, to], across the household's calendar accounts, ordered by start time.
func (r *ExternalEventRepository) ListByHouseholdRange(ctx context.Context, householdID household.HouseholdID, from, to time.Time) ([]*domain.ExternalEvent, error) {
	const q = `
		SELECT e.id, e.calendar_account_id, e.external_id, e.title, e.starts_at,
		       e.ends_at, e.all_day, e.color, e.updated_at
		  FROM external_event e
		  JOIN calendar_account a ON a.id = e.calendar_account_id
		 WHERE a.household_id = $1 AND e.starts_at >= $2 AND e.starts_at <= $3
		 ORDER BY e.starts_at, e.id`
	rows, err := r.dbtx.Query(ctx, q, householdID.String(), from, to)
	if err != nil {
		return nil, fmt.Errorf("list external events by range: %w", err)
	}
	defer rows.Close()

	events := make([]*domain.ExternalEvent, 0)
	for rows.Next() {
		event, err := scanExternalEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("list external events by range: scan: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list external events by range: %w", err)
	}
	return events, nil
}

func scanExternalEvent(r row) (*domain.ExternalEvent, error) {
	var (
		event               domain.ExternalEvent
		idStr, accountIDStr string
		color               *string
	)
	if err := r.Scan(
		&idStr, &accountIDStr, &event.ExternalID, &event.Title, &event.StartsAt,
		&event.EndsAt, &event.AllDay, &color, &event.UpdatedAt,
	); err != nil {
		return nil, err
	}
	id, err := domain.ParseExternalEventID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan external event: %w", err)
	}
	accountID, err := domain.ParseCalendarAccountID(accountIDStr)
	if err != nil {
		return nil, fmt.Errorf("scan external event: %w", err)
	}
	event.ID, event.CalendarAccountID = id, accountID
	if color != nil {
		event.Color = *color
	}
	return &event, nil
}

// colorArg renders an optional provider color id as a query argument; an empty
// string is stored as NULL.
func colorArg(color string) *string {
	if color == "" {
		return nil
	}
	return &color
}
