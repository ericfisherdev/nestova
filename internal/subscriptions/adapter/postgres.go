package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

// SubscriptionRepository is the pgx-backed domain.SubscriptionRepository. UUIDs
// are passed and scanned as text, matching the household and tracking adapters
// (no pgx UUID codec registration required).
type SubscriptionRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.SubscriptionRepository = (*SubscriptionRepository)(nil)

// NewSubscriptionRepository constructs the repository with an injected query
// executor (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewSubscriptionRepository(dbtx db.TX) *SubscriptionRepository {
	if dbtx == nil {
		panic("adapter: NewSubscriptionRepository requires a non-nil db.TX")
	}
	return &SubscriptionRepository{dbtx: dbtx}
}

// Create inserts a subscription and populates its timestamps. It maps FK
// violations to household.ErrHouseholdNotFound (unknown household) and
// household.ErrMemberNotFound (unknown payer in the household).
func (r *SubscriptionRepository) Create(ctx context.Context, sub *domain.Subscription) error {
	if sub == nil {
		return errors.New("adapter: create subscription: nil subscription")
	}
	const q = `
		INSERT INTO subscription
			(id, household_id, name, amount_cents, currency, cycle, next_renewal_on,
			 payer_id, category, reminder_lead_days, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING created_at, updated_at`
	err := r.dbtx.QueryRow(ctx, q,
		sub.ID.String(), sub.HouseholdID.String(), sub.Name, sub.Amount.Cents,
		sub.Amount.Currency, sub.Cycle.String(), sub.NextRenewalOn,
		payerArg(sub.PayerID), sub.Category, sub.ReminderLeadDays, sub.Active,
	).Scan(&sub.CreatedAt, &sub.UpdatedAt)
	if err != nil {
		if mapped := mapFKViolation(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("create subscription: %w", err)
	}
	return nil
}

// Get returns the subscription, or domain.ErrSubscriptionNotFound.
func (r *SubscriptionRepository) Get(ctx context.Context, id domain.SubscriptionID) (*domain.Subscription, error) {
	const q = selectColumns + ` WHERE id = $1`
	sub, err := scanSubscription(r.dbtx.QueryRow(ctx, q, id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrSubscriptionNotFound
		}
		return nil, fmt.Errorf("get subscription: %w", err)
	}
	return sub, nil
}

// Update rewrites the subscription's mutable fields and refreshes updated_at.
// household_id is immutable and not rewritten. It returns
// domain.ErrSubscriptionNotFound when the id is unknown and
// household.ErrMemberNotFound when the new payer is not in the household.
func (r *SubscriptionRepository) Update(ctx context.Context, sub *domain.Subscription) error {
	if sub == nil {
		return errors.New("adapter: update subscription: nil subscription")
	}
	const q = `
		UPDATE subscription
		   SET name = $2, amount_cents = $3, currency = $4, cycle = $5,
		       next_renewal_on = $6, payer_id = $7, category = $8,
		       reminder_lead_days = $9, active = $10, updated_at = now()
		 WHERE id = $1
		RETURNING updated_at`
	err := r.dbtx.QueryRow(ctx, q,
		sub.ID.String(), sub.Name, sub.Amount.Cents, sub.Amount.Currency,
		sub.Cycle.String(), sub.NextRenewalOn, payerArg(sub.PayerID),
		sub.Category, sub.ReminderLeadDays, sub.Active,
	).Scan(&sub.UpdatedAt)
	if err != nil {
		if mapped := mapFKViolation(err); mapped != nil {
			return mapped
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrSubscriptionNotFound
		}
		return fmt.Errorf("update subscription: %w", err)
	}
	return nil
}

// Deactivate sets active=false and refreshes updated_at. It returns
// domain.ErrSubscriptionNotFound when the id is unknown (no row updated).
func (r *SubscriptionRepository) Deactivate(ctx context.Context, id domain.SubscriptionID) error {
	const q = `UPDATE subscription SET active = false, updated_at = now() WHERE id = $1`
	tag, err := r.dbtx.Exec(ctx, q, id.String())
	if err != nil {
		return fmt.Errorf("deactivate subscription: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrSubscriptionNotFound
	}
	return nil
}

// ListActiveByHousehold returns the household's active subscriptions ordered by
// next renewal then name.
func (r *SubscriptionRepository) ListActiveByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*domain.Subscription, error) {
	const q = selectColumns + `
		WHERE household_id = $1 AND active = true
		ORDER BY next_renewal_on, name, id`
	return r.querySubscriptions(ctx, "list active subscriptions", q, householdID.String())
}

// ListDueForRenewal returns active, non-custom subscriptions whose next renewal
// falls within their reminder lead window of asOf
// (next_renewal_on - reminder_lead_days <= the UTC calendar date of asOf). asOf
// is injected so the query is deterministic. The date is taken in UTC via
// AT TIME ZONE 'UTC' so the comparison does not depend on the database session's
// timezone, matching the date-only (UTC midnight) convention of next_renewal_on.
func (r *SubscriptionRepository) ListDueForRenewal(ctx context.Context, asOf time.Time) ([]*domain.Subscription, error) {
	const q = selectColumns + `
		WHERE active = true
		  AND cycle <> 'custom'
		  AND next_renewal_on - reminder_lead_days <= ($1 AT TIME ZONE 'UTC')::date
		ORDER BY next_renewal_on, id`
	return r.querySubscriptions(ctx, "list due-for-renewal subscriptions", q, asOf)
}

// selectColumns is the shared column list for subscription reads, in scan order.
const selectColumns = `
	SELECT id, household_id, name, amount_cents, currency, cycle, next_renewal_on,
	       payer_id, category, reminder_lead_days, active, created_at, updated_at
	  FROM subscription`

func (r *SubscriptionRepository) querySubscriptions(ctx context.Context, op, q string, args ...any) ([]*domain.Subscription, error) {
	rows, err := r.dbtx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()

	subs := make([]*domain.Subscription, 0)
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", op, err)
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return subs, nil
}

// row abstracts pgx.Row and pgx.Rows for the shared scan helper.
type row interface {
	Scan(dest ...any) error
}

func scanSubscription(r row) (*domain.Subscription, error) {
	var (
		sub                    domain.Subscription
		idStr, hhStr, cycleStr string
		currency               string
		amountCents            int64
		payerIDStr             *string
	)
	if err := r.Scan(
		&idStr, &hhStr, &sub.Name, &amountCents, &currency, &cycleStr,
		&sub.NextRenewalOn, &payerIDStr, &sub.Category, &sub.ReminderLeadDays,
		&sub.Active, &sub.CreatedAt, &sub.UpdatedAt,
	); err != nil {
		return nil, err
	}

	id, err := domain.ParseSubscriptionID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan subscription: %w", err)
	}
	hhID, err := household.ParseHouseholdID(hhStr)
	if err != nil {
		return nil, fmt.Errorf("scan subscription: %w", err)
	}
	cycle, err := domain.ParseCycle(cycleStr)
	if err != nil {
		return nil, fmt.Errorf("scan subscription: %w", err)
	}
	amount, err := household.NewMoney(amountCents, currency)
	if err != nil {
		return nil, fmt.Errorf("scan subscription: %w", err)
	}
	sub.ID, sub.HouseholdID, sub.Cycle, sub.Amount = id, hhID, cycle, amount

	if payerIDStr != nil {
		pid, err := household.ParseMemberID(*payerIDStr)
		if err != nil {
			return nil, fmt.Errorf("scan subscription: %w", err)
		}
		sub.PayerID = &pid
	}
	return &sub, nil
}

// payerArg renders an optional payer id as a text query argument (NULL when nil).
func payerArg(id *household.MemberID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

// mapFKViolation maps a subscription FK violation to its domain sentinel, or nil
// when err is not a recognized FK violation.
func mapFKViolation(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != foreignKeyViolation {
		return nil
	}
	switch pgErr.ConstraintName {
	case subscriptionHouseholdFK:
		return household.ErrHouseholdNotFound
	case subscriptionPayerFK:
		return household.ErrMemberNotFound
	default:
		return nil
	}
}
