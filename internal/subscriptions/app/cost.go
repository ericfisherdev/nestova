package app

import (
	"context"
	"errors"
	"fmt"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

// defaultRollupCurrency is the currency of a zero rollup when a household has no
// active normalizable subscriptions. The app has no per-household currency yet,
// so the rollup reports its total in the currency of the household's own active
// subscriptions and falls back to this only for the empty case.
const defaultRollupCurrency = "USD"

// activeSubscriptionLister is the slice of the subscription repository the cost
// rollup depends on (ISP): just the active-by-household listing. The pgx
// SubscriptionRepository satisfies it.
type activeSubscriptionLister interface {
	ListActiveByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*domain.Subscription, error)
}

// CostService computes the household's monthly-normalized subscription cost. The
// normalization is pure domain logic, so it lives here over the repository port
// rather than in SQL, keeping the cycle math in one place (domain.NormalizeMonthly).
type CostService struct {
	subs activeSubscriptionLister
}

// NewCostService constructs the service with an injected subscription lister.
func NewCostService(subs activeSubscriptionLister) *CostService {
	if subs == nil {
		panic("app: NewCostService requires a non-nil subscription lister")
	}
	return &CostService{subs: subs}
}

// MonthlyCost returns the household's total monthly-normalized subscription cost.
// It sums each active subscription's amount normalized to a monthly figure;
// custom-cycle subscriptions contribute nothing (domain.ErrUnsupportedCycle is
// treated as a zero contribution). When the household has no active normalizable
// subscriptions it returns a zero Money in defaultRollupCurrency. It returns
// household.ErrCurrencyMismatch when the active subscriptions span more than one
// currency, since a mixed-currency total is not meaningful.
func (s *CostService) MonthlyCost(ctx context.Context, householdID household.HouseholdID) (household.Money, error) {
	subs, err := s.subs.ListActiveByHousehold(ctx, householdID)
	if err != nil {
		return household.Money{}, fmt.Errorf("monthly cost: %w", err)
	}

	var (
		total household.Money
		have  bool
	)
	for _, sub := range subs {
		monthly, err := domain.NormalizeMonthly(sub.Amount, sub.Cycle)
		if err != nil {
			if errors.Is(err, domain.ErrUnsupportedCycle) {
				continue // custom cycle: excluded from the rollup
			}
			return household.Money{}, fmt.Errorf("monthly cost: %w", err)
		}
		if !have {
			total, have = monthly, true
			continue
		}
		total, err = total.Add(monthly) // surfaces household.ErrCurrencyMismatch on mixed currencies
		if err != nil {
			return household.Money{}, fmt.Errorf("monthly cost: %w", err)
		}
	}
	if !have {
		return household.NewMoney(0, defaultRollupCurrency)
	}
	return total, nil
}
