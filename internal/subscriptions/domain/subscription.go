package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Subscription errors.
var (
	// ErrSubscriptionNotFound is returned when a subscription does not exist.
	ErrSubscriptionNotFound = errors.New("subscriptions: subscription not found")
	// ErrInvalidSubscription is returned by Validate for a malformed subscription.
	ErrInvalidSubscription = errors.New("subscriptions: invalid subscription")
)

// Subscription is a recurring household expense billed on a Cycle. Amount is the
// per-cycle cost (strictly positive). NextRenewalOn is the date the next charge
// is expected; the renewal scheduler rolls it forward by Cycle. PayerID is the
// optional member the cost is attributed to; it is nilled (not deleted) when the
// member is removed, preserving the subscription. ReminderLeadDays is how many
// days before NextRenewalOn a renewal reminder is emitted. Inactive
// subscriptions are retained but excluded from active listings, the cost
// rollup, and renewal runs.
type Subscription struct {
	ID          SubscriptionID
	HouseholdID household.HouseholdID
	Name        string
	Amount      household.Money
	Cycle       Cycle
	// NextRenewalOn is the calendar date of the next expected charge. It is a
	// date-only value (midnight in its location); Validate rejects a time
	// component because the next_renewal_on column is a SQL date.
	NextRenewalOn    time.Time
	PayerID          *household.MemberID
	Category         string
	ReminderLeadDays int
	Active           bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Validate reports whether the subscription is well-formed, wrapping
// ErrInvalidSubscription (or the underlying value-object error) with detail.
func (s Subscription) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("%w: name must not be blank", ErrInvalidSubscription)
	}
	if err := s.Amount.Validate(); err != nil {
		return err
	}
	// A subscription's per-cycle cost is strictly positive. Money alone permits
	// zero (a meaningful amount in other contexts), so tighten it here to match
	// the amount_cents > 0 CHECK in the migration and keep domain and schema
	// aligned.
	if s.Amount.Cents <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrInvalidSubscription)
	}
	if !s.Cycle.Valid() {
		return fmt.Errorf("%w: unknown cycle %q", ErrInvalidSubscription, s.Cycle)
	}
	if s.NextRenewalOn.IsZero() {
		return fmt.Errorf("%w: next renewal date is required", ErrInvalidSubscription)
	}
	// next_renewal_on is a SQL date, so any time-of-day would be silently
	// truncated on write (and could drift across time zones). Require a
	// date-only value — midnight in its location — so the domain owns the
	// invariant the column enforces. Checking the clock components is
	// zone-agnostic: midnight has a zero clock in every location.
	if h, m, sec := s.NextRenewalOn.Clock(); h != 0 || m != 0 || sec != 0 || s.NextRenewalOn.Nanosecond() != 0 {
		return fmt.Errorf("%w: next renewal date must be a date-only value (midnight), got %s", ErrInvalidSubscription, s.NextRenewalOn)
	}
	if s.ReminderLeadDays < 0 {
		return fmt.Errorf("%w: reminder lead days must be non-negative, got %d", ErrInvalidSubscription, s.ReminderLeadDays)
	}
	return nil
}

// SubscriptionRepository persists subscriptions.
//
// Persistence contracts (the caller sets identity and valid field values; the
// store sets timestamps):
//   - Create expects a validated Subscription with ID and HouseholdID; it
//     populates CreatedAt/UpdatedAt.
//   - Update expects an existing ID and rewrites the mutable fields (Name,
//     Amount, Cycle, NextRenewalOn, PayerID, Category, ReminderLeadDays,
//     Active); it refreshes UpdatedAt.
//   - Deactivate sets Active=false and refreshes UpdatedAt without otherwise
//     altering the row.
//
// Error contracts:
//   - Get, Update, and Deactivate return ErrSubscriptionNotFound when the id is
//     unknown.
//   - A Create or Update whose HouseholdID is unknown returns
//     household.ErrHouseholdNotFound; an unknown PayerID returns
//     household.ErrMemberNotFound (both mapped from the tenant FK violations by
//     the adapter).
//   - ListActiveByHousehold and ListDueForRenewal return an empty slice (not an
//     error) when nothing matches.
type SubscriptionRepository interface {
	Create(ctx context.Context, sub *Subscription) error
	Get(ctx context.Context, id SubscriptionID) (*Subscription, error)
	Update(ctx context.Context, sub *Subscription) error
	Deactivate(ctx context.Context, id SubscriptionID) error
	ListActiveByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*Subscription, error)
	// ListDueForRenewal returns active subscriptions whose NextRenewalOn falls
	// within ReminderLeadDays of asOf (NextRenewalOn - ReminderLeadDays <= asOf).
	// Custom-cycle subscriptions are excluded. asOf is injected so the query is
	// deterministic and testable.
	ListDueForRenewal(ctx context.Context, asOf time.Time) ([]*Subscription, error)
}
