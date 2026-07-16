package main

import (
	"context"
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// errTestSeedFailure is a sentinel used only to exercise seedHouseholdRewards'
// error propagation path.
var errTestSeedFailure = errors.New("seed failure")

// recordingRewardCreator is a minimal rewardCreator fake that records every
// reward it is asked to persist, so seedHouseholdRewards is testable without
// a database (NES-126 AC4).
type recordingRewardCreator struct {
	createCalls []*tasksdomain.Reward
	// failAfter, when > 0, makes CreateReward fail starting at the
	// (1-indexed) call number given, so the "stop on first error" behaviour
	// can be verified.
	failAfter int
}

func (r *recordingRewardCreator) CreateReward(_ context.Context, reward *tasksdomain.Reward) error {
	r.createCalls = append(r.createCalls, reward)
	if r.failAfter > 0 && len(r.createCalls) >= r.failAfter {
		return errTestSeedFailure
	}
	return nil
}

// Compile-time assertion.
var _ rewardCreator = (*recordingRewardCreator)(nil)

// TestSeedHouseholdRewards_CreatesAllSeeds verifies that every household
// onboarding seeds the full example reward catalogue as active, household-
// scoped rewards (NES-126 AC4).
func TestSeedHouseholdRewards_CreatesAllSeeds(t *testing.T) {
	hhID := household.NewHouseholdID()
	repo := &recordingRewardCreator{}

	if err := seedHouseholdRewards(t.Context(), repo, hhID); err != nil {
		t.Fatalf("seedHouseholdRewards: unexpected error: %v", err)
	}

	if len(repo.createCalls) != len(householdRewardSeeds) {
		t.Fatalf("created %d rewards, want %d", len(repo.createCalls), len(householdRewardSeeds))
	}

	seenNames := make(map[string]bool, len(repo.createCalls))
	for i, created := range repo.createCalls {
		seed := householdRewardSeeds[i]
		if created.Name != seed.name {
			t.Errorf("reward[%d].Name = %q, want %q", i, created.Name, seed.name)
		}
		if created.Description != seed.description {
			t.Errorf("reward[%d].Description = %q, want %q", i, created.Description, seed.description)
		}
		if created.CostPoints != seed.costPoints {
			t.Errorf("reward[%d].CostPoints = %d, want %d", i, created.CostPoints, seed.costPoints)
		}
		if created.HouseholdID != hhID {
			t.Errorf("reward[%d].HouseholdID = %v, want %v", i, created.HouseholdID, hhID)
		}
		if !created.Active {
			t.Errorf("reward[%d].Active = false, want true", i)
		}
		if created.QuantityAvailable != nil {
			t.Errorf("reward[%d].QuantityAvailable = %v, want nil (unlimited)", i, created.QuantityAvailable)
		}
		if created.ImageRef == nil || *created.ImageRef != seed.imageRef {
			t.Errorf("reward[%d].ImageRef = %v, want %q", i, created.ImageRef, seed.imageRef)
		}
		seenNames[created.Name] = true
	}
	if len(seenNames) != len(householdRewardSeeds) {
		t.Errorf("seeded reward names are not unique: %d distinct names for %d seeds", len(seenNames), len(householdRewardSeeds))
	}
}

// TestSeedHouseholdRewards_PropagatesRepositoryError verifies that a failure
// partway through seeding is returned to the caller (so the enclosing
// onboarding transaction rolls back) rather than being swallowed.
func TestSeedHouseholdRewards_PropagatesRepositoryError(t *testing.T) {
	repo := &recordingRewardCreator{failAfter: 2}

	err := seedHouseholdRewards(t.Context(), repo, household.NewHouseholdID())
	if err == nil {
		t.Fatal("seedHouseholdRewards: expected an error, got nil")
	}
	if len(repo.createCalls) != 2 {
		t.Errorf("CreateReward called %d times before stopping, want 2", len(repo.createCalls))
	}
}

// TestHouseholdRewardSeeds_AtLeastThree verifies the ticket's minimum seed
// count (3-4 example rewards).
func TestHouseholdRewardSeeds_AtLeastThree(t *testing.T) {
	if got := len(householdRewardSeeds); got < 3 {
		t.Errorf("len(householdRewardSeeds) = %d, want at least 3", got)
	}
}
