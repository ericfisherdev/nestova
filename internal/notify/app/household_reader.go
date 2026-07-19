package app

import (
	"context"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// householdReader is the narrow read port routing.go and settings.go need
// onto household.HouseholdRepository — just enough to fetch a household's
// quiet-hours window (ISP: neither needs the rest of that much larger
// interface). Any HouseholdRepository implementation — in particular
// householdadapter.PostgresRepository — satisfies this structurally,
// without either interface embedding the other.
type householdReader interface {
	GetHousehold(ctx context.Context, id household.HouseholdID) (*household.Household, error)
}
