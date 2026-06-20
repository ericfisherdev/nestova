package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

// SubscriptionInput is the validated, parsed form data for creating or editing a
// subscription. The web layer parses raw form values into this; the service
// builds and validates the domain entity.
type SubscriptionInput struct {
	Name             string
	Amount           household.Money
	Cycle            domain.Cycle
	NextRenewalOn    time.Time
	PayerID          *household.MemberID
	Category         string
	ReminderLeadDays int
}

// SubscriptionService manages a household's subscriptions (create, edit,
// deactivate). The monthly cost rollup lives in CostService.
type SubscriptionService struct {
	repo domain.SubscriptionRepository
}

// NewSubscriptionService constructs the service with an injected repository.
func NewSubscriptionService(repo domain.SubscriptionRepository) (*SubscriptionService, error) {
	if repo == nil {
		return nil, errors.New("app: NewSubscriptionService requires a non-nil repository")
	}
	return &SubscriptionService{repo: repo}, nil
}

// Add creates a new active subscription for the household and returns its id.
func (s *SubscriptionService) Add(ctx context.Context, householdID household.HouseholdID, in SubscriptionInput) (domain.SubscriptionID, error) {
	sub := &domain.Subscription{
		ID:               domain.NewSubscriptionID(),
		HouseholdID:      householdID,
		Name:             in.Name,
		Amount:           in.Amount,
		Cycle:            in.Cycle,
		NextRenewalOn:    in.NextRenewalOn,
		PayerID:          in.PayerID,
		Category:         in.Category,
		ReminderLeadDays: in.ReminderLeadDays,
		Active:           true,
	}
	if err := sub.Validate(); err != nil {
		return domain.SubscriptionID{}, err
	}
	if err := s.repo.Create(ctx, sub); err != nil {
		return domain.SubscriptionID{}, fmt.Errorf("add subscription: %w", err)
	}
	return sub.ID, nil
}

// Edit rewrites a subscription's mutable fields after verifying it belongs to
// householdID. It returns domain.ErrSubscriptionNotFound when the id is unknown
// or belongs to another household (the existence of another tenant's row is not
// leaked).
func (s *SubscriptionService) Edit(ctx context.Context, householdID household.HouseholdID, id domain.SubscriptionID, in SubscriptionInput) error {
	sub, err := s.ownedSubscription(ctx, householdID, id)
	if err != nil {
		return err
	}
	sub.Name = in.Name
	sub.Amount = in.Amount
	sub.Cycle = in.Cycle
	sub.NextRenewalOn = in.NextRenewalOn
	sub.PayerID = in.PayerID
	sub.Category = in.Category
	sub.ReminderLeadDays = in.ReminderLeadDays
	if err := sub.Validate(); err != nil {
		return err
	}
	if err := s.repo.Update(ctx, sub); err != nil {
		return fmt.Errorf("edit subscription: %w", err)
	}
	return nil
}

// Deactivate marks a subscription inactive after verifying it belongs to
// householdID. It returns domain.ErrSubscriptionNotFound when the id is unknown
// or belongs to another household.
func (s *SubscriptionService) Deactivate(ctx context.Context, householdID household.HouseholdID, id domain.SubscriptionID) error {
	if _, err := s.ownedSubscription(ctx, householdID, id); err != nil {
		return err
	}
	return s.repo.Deactivate(ctx, id)
}

// ownedSubscription fetches a subscription and confirms it belongs to
// householdID, returning domain.ErrSubscriptionNotFound otherwise so a tenant
// cannot probe or mutate another household's subscriptions by id.
func (s *SubscriptionService) ownedSubscription(ctx context.Context, householdID household.HouseholdID, id domain.SubscriptionID) (*domain.Subscription, error) {
	sub, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub.HouseholdID != householdID {
		return nil, domain.ErrSubscriptionNotFound
	}
	return sub, nil
}

// ListActive returns the household's active subscriptions.
func (s *SubscriptionService) ListActive(ctx context.Context, householdID household.HouseholdID) ([]*domain.Subscription, error) {
	return s.repo.ListActiveByHousehold(ctx, householdID)
}
