package domain_test

import (
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	subscriptions "github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

func validSubscription(t *testing.T) subscriptions.Subscription {
	t.Helper()
	amount, err := household.NewMoney(999, "USD")
	if err != nil {
		t.Fatalf("NewMoney() error = %v", err)
	}
	return subscriptions.Subscription{
		ID:               subscriptions.NewSubscriptionID(),
		HouseholdID:      household.HouseholdID{},
		Name:             "Streaming Plus",
		Amount:           amount,
		Cycle:            subscriptions.CycleMonthly,
		NextRenewalOn:    time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Category:         "entertainment",
		ReminderLeadDays: 3,
		Active:           true,
	}
}

func TestSubscriptionValidate(t *testing.T) {
	if err := validSubscription(t).Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestSubscriptionValidateAcceptsCeilingForEveryCycle(t *testing.T) {
	for _, cycle := range []subscriptions.Cycle{subscriptions.CycleWeekly, subscriptions.CycleMonthly, subscriptions.CycleYearly} {
		sub := validSubscription(t)
		sub.Amount = mustMoney(t, subscriptions.MaxAmountCents, "USD")
		sub.Cycle = cycle
		if err := sub.Validate(); err != nil {
			t.Fatalf("Validate() at ceiling for %s error = %v", cycle, err)
		}
		if _, err := subscriptions.NormalizeMonthly(sub.Amount, cycle); err != nil {
			t.Fatalf("NormalizeMonthly() at ceiling for %s error = %v", cycle, err)
		}
	}
}

func TestSubscriptionValidateRejects(t *testing.T) {
	zeroAmount := mustMoney(t, 0, "USD")
	cases := []struct {
		name   string
		mutate func(*subscriptions.Subscription)
		want   error
	}{
		{"blank name", func(s *subscriptions.Subscription) { s.Name = "" }, subscriptions.ErrInvalidSubscription},
		{"whitespace name", func(s *subscriptions.Subscription) { s.Name = "   " }, subscriptions.ErrInvalidSubscription},
		{"zero amount", func(s *subscriptions.Subscription) { s.Amount = zeroAmount }, subscriptions.ErrInvalidSubscription},
		{"invalid money", func(s *subscriptions.Subscription) { s.Amount = household.Money{Cents: -1, Currency: "USD"} }, household.ErrInvalidMoney},
		{"amount above ceiling", func(s *subscriptions.Subscription) {
			s.Amount = mustMoney(t, subscriptions.MaxAmountCents+1, "USD")
		}, subscriptions.ErrAmountTooLarge},
		{"unknown cycle", func(s *subscriptions.Subscription) { s.Cycle = subscriptions.Cycle("daily") }, subscriptions.ErrInvalidSubscription},
		{"zero renewal date", func(s *subscriptions.Subscription) { s.NextRenewalOn = time.Time{} }, subscriptions.ErrInvalidSubscription},
		{"renewal date with time component", func(s *subscriptions.Subscription) {
			s.NextRenewalOn = time.Date(2026, 7, 1, 9, 30, 0, 0, time.UTC)
		}, subscriptions.ErrInvalidSubscription},
		{"negative lead days", func(s *subscriptions.Subscription) { s.ReminderLeadDays = -1 }, subscriptions.ErrInvalidSubscription},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub := validSubscription(t)
			tc.mutate(&sub)
			if err := sub.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() error = %v, want %v", err, tc.want)
			}
		})
	}
}
