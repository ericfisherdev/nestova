package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// RewardCatalogManager is the persistence port for the parent-only reward
// admin CRUD flows (NES-126): create, edit, and archive operations on the
// reward catalogue. Kept separate from [RewardRedeemer] (ISP) — the member-
// facing redemption flow (NES-37) needs none of these methods, and the admin
// flow needs none of RedeemWithDebit.
type RewardCatalogManager interface {
	// CreateReward persists a new reward. See
	// [domain.RewardRepository.CreateReward]'s contract.
	CreateReward(ctx context.Context, r *domain.Reward) error

	// GetReward returns the reward with the given id within the household.
	// Returns [domain.ErrRewardNotFound] when id is unknown or belongs to
	// another household.
	GetReward(ctx context.Context, householdID household.HouseholdID, id domain.RewardID) (*domain.Reward, error)

	// UpdateReward persists changes to an existing reward's editable fields.
	// Returns [domain.ErrRewardNotFound] when r.ID is unknown or belongs to
	// another household.
	UpdateReward(ctx context.Context, r *domain.Reward) error

	// ArchiveReward sets Active = false on the reward. Returns
	// [domain.ErrRewardNotFound] when id is unknown or belongs to another
	// household.
	ArchiveReward(ctx context.Context, householdID household.HouseholdID, id domain.RewardID) error
}

// RewardAdminService orchestrates the parent-only reward catalogue admin
// use-cases (NES-126): creating, editing, and archiving rewards. Field
// validation (name required, cost positive, quantity non-negative) is
// centralised here so the HTTP handler never has to duplicate — or drift
// from — the catalogue's invariants.
//
// Dependencies are injected via the constructor so the service is testable
// with fakes (hermetic tests) and wired to Postgres at the composition root.
type RewardAdminService struct {
	repo   RewardCatalogManager
	logger *slog.Logger
}

// NewRewardAdminService constructs a RewardAdminService with the injected
// dependencies. Panics if any dependency is nil so misconfigured composition
// roots fail at startup rather than at the first HTTP request.
func NewRewardAdminService(repo RewardCatalogManager, logger *slog.Logger) *RewardAdminService {
	if repo == nil {
		panic("app: NewRewardAdminService requires a non-nil RewardCatalogManager")
	}
	if logger == nil {
		panic("app: NewRewardAdminService requires a non-nil logger")
	}
	return &RewardAdminService{repo: repo, logger: logger}
}

// Create validates the submitted catalogue fields and persists a new reward
// as Active in the household.
//
// Error contracts:
//   - Returns [domain.ErrInvalidRewardName] when name is empty after trimming.
//   - Returns [domain.ErrInvalidRewardCost] when costPoints is not positive.
//   - Returns [domain.ErrInvalidRewardQuantity] when quantityAvailable is
//     non-nil and negative.
//   - Propagates unexpected repository errors unchanged.
func (s *RewardAdminService) Create(
	ctx context.Context,
	householdID household.HouseholdID,
	name, description string,
	costPoints int,
	imageRef *string,
	quantityAvailable *int,
) (*domain.Reward, error) {
	name = strings.TrimSpace(name)
	if err := validateRewardFields(name, costPoints, quantityAvailable); err != nil {
		return nil, err
	}

	reward := &domain.Reward{
		ID:                domain.NewRewardID(),
		HouseholdID:       householdID,
		Name:              name,
		Description:       strings.TrimSpace(description),
		CostPoints:        costPoints,
		ImageRef:          imageRef,
		QuantityAvailable: quantityAvailable,
		Active:            true,
	}
	if err := s.repo.CreateReward(ctx, reward); err != nil {
		return nil, fmt.Errorf("create reward: %w", err)
	}

	s.logger.InfoContext(ctx, "reward created",
		"household_id", householdID.String(),
		"reward_id", reward.ID.String(),
		"cost_points", reward.CostPoints,
	)
	return reward, nil
}

// Update fetches the existing reward, applies the submitted catalogue field
// values, validates them, and persists the change. Active is left untouched —
// archiving is a separate action (see [RewardAdminService.Archive]).
//
// Error contracts:
//   - Returns [domain.ErrRewardNotFound] when id is unknown or belongs to
//     another household.
//   - Returns [domain.ErrInvalidRewardName], [domain.ErrInvalidRewardCost], or
//     [domain.ErrInvalidRewardQuantity] on a local validation failure — the
//     existing reward is left unchanged.
//   - Propagates unexpected repository errors unchanged.
func (s *RewardAdminService) Update(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RewardID,
	name, description string,
	costPoints int,
	imageRef *string,
	quantityAvailable *int,
) (*domain.Reward, error) {
	name = strings.TrimSpace(name)
	if err := validateRewardFields(name, costPoints, quantityAvailable); err != nil {
		return nil, err
	}

	existing, err := s.repo.GetReward(ctx, householdID, id)
	if err != nil {
		if errors.Is(err, domain.ErrRewardNotFound) {
			return nil, domain.ErrRewardNotFound
		}
		return nil, fmt.Errorf("update reward: get existing: %w", err)
	}

	existing.Name = name
	existing.Description = strings.TrimSpace(description)
	existing.CostPoints = costPoints
	existing.ImageRef = imageRef
	existing.QuantityAvailable = quantityAvailable

	if err := s.repo.UpdateReward(ctx, existing); err != nil {
		if errors.Is(err, domain.ErrRewardNotFound) {
			return nil, domain.ErrRewardNotFound
		}
		return nil, fmt.Errorf("update reward: %w", err)
	}

	s.logger.InfoContext(ctx, "reward updated",
		"household_id", householdID.String(),
		"reward_id", id.String(),
	)
	return existing, nil
}

// Archive retires the reward from the storefront (Active = false) without
// touching its redemption history.
//
// Error contracts:
//   - Returns [domain.ErrRewardNotFound] when id is unknown or belongs to
//     another household.
//   - Propagates unexpected repository errors unchanged.
func (s *RewardAdminService) Archive(ctx context.Context, householdID household.HouseholdID, id domain.RewardID) error {
	if err := s.repo.ArchiveReward(ctx, householdID, id); err != nil {
		if errors.Is(err, domain.ErrRewardNotFound) {
			return domain.ErrRewardNotFound
		}
		return fmt.Errorf("archive reward: %w", err)
	}

	s.logger.InfoContext(ctx, "reward archived",
		"household_id", householdID.String(),
		"reward_id", id.String(),
	)
	return nil
}

// validateRewardFields enforces the catalogue invariants shared by Create and
// Update, mirroring the reward table's own CHECK constraints (cost_points > 0,
// quantity_available IS NULL OR quantity_available >= 0) so a violation is
// caught before it ever reaches the database.
func validateRewardFields(name string, costPoints int, quantityAvailable *int) error {
	if name == "" {
		return domain.ErrInvalidRewardName
	}
	if costPoints <= 0 {
		return domain.ErrInvalidRewardCost
	}
	if quantityAvailable != nil && *quantityAvailable < 0 {
		return domain.ErrInvalidRewardQuantity
	}
	return nil
}
