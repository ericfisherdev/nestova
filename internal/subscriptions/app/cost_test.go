package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/subscriptions/app"
	"github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

// fakeLister is an in-memory activeSubscriptionLister for hermetic tests.
type fakeLister struct {
	subs []*domain.Subscription
	err  error
}

func (f *fakeLister) ListActiveByHousehold(context.Context, household.HouseholdID) ([]*domain.Subscription, error) {
	return f.subs, f.err
}

func sub(t *testing.T, cents int64, currency string, cycle domain.Cycle) *domain.Subscription {
	t.Helper()
	amount, err := household.NewMoney(cents, currency)
	if err != nil {
		t.Fatalf("NewMoney(%d, %q) error = %v", cents, currency, err)
	}
	return &domain.Subscription{
		ID:            domain.NewSubscriptionID(),
		HouseholdID:   household.NewHouseholdID(),
		Name:          "sub",
		Amount:        amount,
		Cycle:         cycle,
		NextRenewalOn: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Active:        true,
	}
}

func TestMonthlyCostMixedCycles(t *testing.T) {
	// weekly 1000 -> 4333, monthly 1500 -> 1500, yearly 12000 -> 1000. Sum = 6833.
	lister := &fakeLister{subs: []*domain.Subscription{
		sub(t, 1000, "USD", domain.CycleWeekly),
		sub(t, 1500, "USD", domain.CycleMonthly),
		sub(t, 12000, "USD", domain.CycleYearly),
	}}
	got, err := app.NewCostService(lister).MonthlyCost(context.Background(), household.NewHouseholdID())
	if err != nil {
		t.Fatalf("MonthlyCost() error = %v", err)
	}
	if got.Cents != 6833 || got.Currency != "USD" {
		t.Fatalf("MonthlyCost() = %+v, want {6833 USD}", got)
	}
}

func TestMonthlyCostExcludesCustom(t *testing.T) {
	lister := &fakeLister{subs: []*domain.Subscription{
		sub(t, 1500, "USD", domain.CycleMonthly),
		sub(t, 9999, "USD", domain.CycleCustom), // excluded from the rollup
	}}
	got, err := app.NewCostService(lister).MonthlyCost(context.Background(), household.NewHouseholdID())
	if err != nil {
		t.Fatalf("MonthlyCost() error = %v", err)
	}
	if got.Cents != 1500 || got.Currency != "USD" {
		t.Fatalf("MonthlyCost() = %+v, want {1500 USD} (custom excluded)", got)
	}
}

func TestMonthlyCostEmptyIsZero(t *testing.T) {
	got, err := app.NewCostService(&fakeLister{}).MonthlyCost(context.Background(), household.NewHouseholdID())
	if err != nil {
		t.Fatalf("MonthlyCost() error = %v", err)
	}
	if got.Cents != 0 || got.Currency != "USD" {
		t.Fatalf("MonthlyCost() = %+v, want {0 USD}", got)
	}
}

func TestMonthlyCostOnlyCustomIsZero(t *testing.T) {
	lister := &fakeLister{subs: []*domain.Subscription{sub(t, 9999, "USD", domain.CycleCustom)}}
	got, err := app.NewCostService(lister).MonthlyCost(context.Background(), household.NewHouseholdID())
	if err != nil {
		t.Fatalf("MonthlyCost() error = %v", err)
	}
	if got.Cents != 0 || got.Currency != "USD" {
		t.Fatalf("MonthlyCost() = %+v, want {0 USD} (only custom)", got)
	}
}

func TestMonthlyCostMixedCurrencyFails(t *testing.T) {
	lister := &fakeLister{subs: []*domain.Subscription{
		sub(t, 1000, "USD", domain.CycleMonthly),
		sub(t, 1000, "EUR", domain.CycleMonthly),
	}}
	if _, err := app.NewCostService(lister).MonthlyCost(context.Background(), household.NewHouseholdID()); !errors.Is(err, household.ErrCurrencyMismatch) {
		t.Fatalf("MonthlyCost() error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestMonthlyCostPropagatesListError(t *testing.T) {
	wantErr := errors.New("boom")
	if _, err := app.NewCostService(&fakeLister{err: wantErr}).MonthlyCost(context.Background(), household.NewHouseholdID()); !errors.Is(err, wantErr) {
		t.Fatalf("MonthlyCost() error = %v, want wrapped %v", err, wantErr)
	}
}
