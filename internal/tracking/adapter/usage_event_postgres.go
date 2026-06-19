package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// UsageEventRepository is the pgx-backed domain.UsageEventRepository.
type UsageEventRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.UsageEventRepository = (*UsageEventRepository)(nil)

// NewUsageEventRepository constructs the repository with an injected query
// executor (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewUsageEventRepository(dbtx db.TX) *UsageEventRepository {
	if dbtx == nil {
		panic("adapter: NewUsageEventRepository requires a non-nil db.TX")
	}
	return &UsageEventRepository{dbtx: dbtx}
}

// Append inserts a usage event and populates CreatedAt. It maps FK violations to
// domain.ErrTrackedItemNotFound (unknown item) and household.ErrMemberNotFound
// (unknown member in the household).
func (r *UsageEventRepository) Append(ctx context.Context, event *domain.UsageEvent) error {
	if event == nil {
		return errors.New("adapter: append usage event: nil event")
	}
	const q = `
		INSERT INTO usage_event (id, household_id, tracked_item_id, type, occurred_at, member_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at`

	var memberIDStr *string
	if event.MemberID != nil {
		s := event.MemberID.String()
		memberIDStr = &s
	}

	err := r.dbtx.QueryRow(ctx, q,
		event.ID.String(), event.HouseholdID.String(), event.TrackedItemID.String(),
		event.Type.String(), event.OccurredAt, memberIDStr,
	).Scan(&event.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			switch pgErr.ConstraintName {
			case usageEventTrackedItemFK:
				return domain.ErrTrackedItemNotFound
			case usageEventMemberFK:
				return household.ErrMemberNotFound
			}
		}
		return fmt.Errorf("append usage event: %w", err)
	}
	return nil
}

// ListDepletionEvents returns the item's depletion events ordered by occurrence
// (ascending), or an empty slice when there are none.
func (r *UsageEventRepository) ListDepletionEvents(ctx context.Context, trackedItemID domain.TrackedItemID) ([]*domain.UsageEvent, error) {
	// type is the literal 'depleted' (== domain.UsageDepleted) on purpose, not a
	// bind parameter: the partial index usage_event_depleted_item_occurred_idx has
	// predicate `WHERE type = 'depleted'`, and the planner only matches a partial
	// index when the query carries the same literal predicate. A `type = $2`
	// parameter would defeat the index and force a scan.
	const q = `
		SELECT id, household_id, tracked_item_id, type, occurred_at, member_id, created_at
		FROM usage_event
		WHERE tracked_item_id = $1 AND type = 'depleted'
		ORDER BY occurred_at, id`
	rows, err := r.dbtx.Query(ctx, q, trackedItemID.String())
	if err != nil {
		return nil, fmt.Errorf("list depletion events: %w", err)
	}
	defer rows.Close()

	events := make([]*domain.UsageEvent, 0)
	for rows.Next() {
		event, err := scanUsageEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("list depletion events: scan: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list depletion events: %w", err)
	}
	return events, nil
}

func scanUsageEvent(r row) (*domain.UsageEvent, error) {
	var (
		event                      domain.UsageEvent
		idStr, hhStr, itemStr, typ string
		memberIDStr                *string
	)
	if err := r.Scan(&idStr, &hhStr, &itemStr, &typ, &event.OccurredAt, &memberIDStr, &event.CreatedAt); err != nil {
		return nil, err
	}
	id, err := domain.ParseUsageEventID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan usage event: %w", err)
	}
	hhID, err := household.ParseHouseholdID(hhStr)
	if err != nil {
		return nil, fmt.Errorf("scan usage event: %w", err)
	}
	itemID, err := domain.ParseTrackedItemID(itemStr)
	if err != nil {
		return nil, fmt.Errorf("scan usage event: %w", err)
	}
	usageType, err := domain.ParseUsageType(typ)
	if err != nil {
		return nil, fmt.Errorf("scan usage event: %w", err)
	}
	event.ID, event.HouseholdID, event.TrackedItemID, event.Type = id, hhID, itemID, usageType

	if memberIDStr != nil {
		mid, err := household.ParseMemberID(*memberIDStr)
		if err != nil {
			return nil, fmt.Errorf("scan usage event: %w", err)
		}
		event.MemberID = &mid
	}
	return &event, nil
}
