package app_test

import (
	"context"
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// fakeRewardCatalogManager — in-memory implementation of app.RewardCatalogManager
// ---------------------------------------------------------------------------

// fakeRewardCatalogManager is a configurable fake that covers all branches of
// RewardAdminService without a database. Its fields allow per-test injection
// of sentinel errors and a pre-set reward record for GetReward/UpdateReward.
type fakeRewardCatalogManager struct {
	// reward is returned by GetReward when getErr is nil.
	reward *domain.Reward
	// getErr overrides GetReward's return when non-nil.
	getErr error
	// createErr overrides CreateReward's return when non-nil.
	createErr error
	// updateErr overrides UpdateReward's return when non-nil.
	updateErr error
	// archiveErr overrides ArchiveReward's return when non-nil.
	archiveErr error
	// createCalls and updateCalls record every reward passed to CreateReward
	// and UpdateReward respectively.
	createCalls []*domain.Reward
	updateCalls []*domain.Reward
	// archiveCalls counts ArchiveReward invocations.
	archiveCalls int
}

func (f *fakeRewardCatalogManager) CreateReward(_ context.Context, r *domain.Reward) error {
	f.createCalls = append(f.createCalls, r)
	return f.createErr
}

func (f *fakeRewardCatalogManager) GetReward(
	_ context.Context,
	_ household.HouseholdID,
	_ domain.RewardID,
) (*domain.Reward, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.reward, nil
}

func (f *fakeRewardCatalogManager) UpdateReward(_ context.Context, r *domain.Reward) error {
	f.updateCalls = append(f.updateCalls, r)
	return f.updateErr
}

func (f *fakeRewardCatalogManager) ArchiveReward(
	_ context.Context,
	_ household.HouseholdID,
	_ domain.RewardID,
) error {
	f.archiveCalls++
	return f.archiveErr
}

// Compile-time assertion.
var _ app.RewardCatalogManager = (*fakeRewardCatalogManager)(nil)

// ---------------------------------------------------------------------------
// RewardAdminService constructor validation
// ---------------------------------------------------------------------------

func TestNewRewardAdminService_NilRepo_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewRewardAdminService(nil repo) did not panic")
		}
	}()
	app.NewRewardAdminService(nil, newTestLogger())
}

func TestNewRewardAdminService_NilLogger_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewRewardAdminService(nil logger) did not panic")
		}
	}()
	app.NewRewardAdminService(&fakeRewardCatalogManager{}, nil)
}

// ---------------------------------------------------------------------------
// RewardAdminService.Create — validation (NES-126 AC1)
// ---------------------------------------------------------------------------

func TestRewardAdminService_Create_Validation(t *testing.T) {
	negativeQty := -1

	tests := []struct {
		name              string
		rewardName        string
		costPoints        int
		quantityAvailable *int
		wantErr           error
	}{
		{name: "empty name", rewardName: "  ", costPoints: 10, wantErr: domain.ErrInvalidRewardName},
		{name: "zero cost", rewardName: "Toy", costPoints: 0, wantErr: domain.ErrInvalidRewardCost},
		{name: "negative cost", rewardName: "Toy", costPoints: -5, wantErr: domain.ErrInvalidRewardCost},
		{name: "negative quantity", rewardName: "Toy", costPoints: 10, quantityAvailable: &negativeQty, wantErr: domain.ErrInvalidRewardQuantity},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRewardCatalogManager{}
			svc := app.NewRewardAdminService(repo, newTestLogger())

			_, err := svc.Create(t.Context(), household.NewHouseholdID(), tt.rewardName, "", tt.costPoints, nil, tt.quantityAvailable)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Create() error = %v, want %v", err, tt.wantErr)
			}
			if len(repo.createCalls) != 0 {
				t.Errorf("CreateReward called %d times on validation failure, want 0", len(repo.createCalls))
			}
		})
	}
}

// TestRewardAdminService_Create_Success verifies that a valid submission
// persists an Active reward stamped with a new id and the household.
func TestRewardAdminService_Create_Success(t *testing.T) {
	hhID := household.NewHouseholdID()
	repo := &fakeRewardCatalogManager{}
	svc := app.NewRewardAdminService(repo, newTestLogger())

	imageRef := "🎮"
	quantity := 5
	reward, err := svc.Create(t.Context(), hhID, "  Extra screen time  ", " 30 minutes ", 20, &imageRef, &quantity)
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if reward.Name != "Extra screen time" {
		t.Errorf("Name = %q, want trimmed %q", reward.Name, "Extra screen time")
	}
	if reward.Description != "30 minutes" {
		t.Errorf("Description = %q, want trimmed %q", reward.Description, "30 minutes")
	}
	if !reward.Active {
		t.Error("Active = false, want true")
	}
	if reward.HouseholdID != hhID {
		t.Errorf("HouseholdID = %v, want %v", reward.HouseholdID, hhID)
	}
	if len(repo.createCalls) != 1 {
		t.Fatalf("CreateReward called %d times, want 1", len(repo.createCalls))
	}
}

// TestRewardAdminService_Create_RepositoryError verifies that a generic
// (non-sentinel) failure from CreateReward is wrapped and propagated rather
// than swallowed.
func TestRewardAdminService_Create_RepositoryError(t *testing.T) {
	wantErr := errors.New("insert failed")
	repo := &fakeRewardCatalogManager{createErr: wantErr}
	svc := app.NewRewardAdminService(repo, newTestLogger())

	_, err := svc.Create(t.Context(), household.NewHouseholdID(), "Toy", "", 10, nil, nil)
	if !errors.Is(err, wantErr) {
		t.Errorf("Create() error = %v, want wrapped %v", err, wantErr)
	}
}

// ---------------------------------------------------------------------------
// RewardAdminService.Update
// ---------------------------------------------------------------------------

func TestRewardAdminService_Update_NotFound(t *testing.T) {
	repo := &fakeRewardCatalogManager{getErr: domain.ErrRewardNotFound}
	svc := app.NewRewardAdminService(repo, newTestLogger())

	_, err := svc.Update(t.Context(), household.NewHouseholdID(), domain.NewRewardID(), "New name", "", 10, nil, nil)
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("Update(unknown reward) = %v, want ErrRewardNotFound", err)
	}
	if len(repo.updateCalls) != 0 {
		t.Errorf("UpdateReward called %d times for an unknown reward, want 0", len(repo.updateCalls))
	}
}

// TestRewardAdminService_Update_GetRewardRepositoryError verifies that a
// generic (non-ErrRewardNotFound) failure from GetReward is wrapped and
// propagated, and that UpdateReward is never reached.
func TestRewardAdminService_Update_GetRewardRepositoryError(t *testing.T) {
	wantErr := errors.New("read failed")
	repo := &fakeRewardCatalogManager{getErr: wantErr}
	svc := app.NewRewardAdminService(repo, newTestLogger())

	_, err := svc.Update(t.Context(), household.NewHouseholdID(), domain.NewRewardID(), "New name", "", 10, nil, nil)
	if !errors.Is(err, wantErr) {
		t.Errorf("Update() error = %v, want wrapped %v", err, wantErr)
	}
	if errors.Is(err, domain.ErrRewardNotFound) {
		t.Error("Update() wrongly reports ErrRewardNotFound for a generic GetReward failure")
	}
	if len(repo.updateCalls) != 0 {
		t.Errorf("UpdateReward called %d times after a GetReward failure, want 0", len(repo.updateCalls))
	}
}

// TestRewardAdminService_Update_RepositoryError verifies that a generic
// (non-sentinel) failure from UpdateReward is wrapped and propagated.
func TestRewardAdminService_Update_RepositoryError(t *testing.T) {
	hhID := household.NewHouseholdID()
	existing := &domain.Reward{
		ID: domain.NewRewardID(), HouseholdID: hhID, Name: "Original", CostPoints: 10, Active: true,
	}
	wantErr := errors.New("write failed")
	repo := &fakeRewardCatalogManager{reward: existing, updateErr: wantErr}
	svc := app.NewRewardAdminService(repo, newTestLogger())

	_, err := svc.Update(t.Context(), hhID, existing.ID, "New name", "", 15, nil, nil)
	if !errors.Is(err, wantErr) {
		t.Errorf("Update() error = %v, want wrapped %v", err, wantErr)
	}
}

func TestRewardAdminService_Update_ValidationFailureLeavesRewardUnchanged(t *testing.T) {
	hhID := household.NewHouseholdID()
	existing := &domain.Reward{
		ID: domain.NewRewardID(), HouseholdID: hhID, Name: "Original", CostPoints: 10, Active: true,
	}
	repo := &fakeRewardCatalogManager{reward: existing}
	svc := app.NewRewardAdminService(repo, newTestLogger())

	_, err := svc.Update(t.Context(), hhID, existing.ID, "", "", 10, nil, nil)
	if !errors.Is(err, domain.ErrInvalidRewardName) {
		t.Errorf("Update(empty name) = %v, want ErrInvalidRewardName", err)
	}
	if len(repo.updateCalls) != 0 {
		t.Errorf("UpdateReward called %d times on validation failure, want 0", len(repo.updateCalls))
	}
}

// TestRewardAdminService_Update_Success verifies that a valid submission
// applies the new field values onto the fetched reward, preserving Active,
// ID, and HouseholdID from the existing record.
func TestRewardAdminService_Update_Success(t *testing.T) {
	hhID := household.NewHouseholdID()
	existing := &domain.Reward{
		ID: domain.NewRewardID(), HouseholdID: hhID, Name: "Original", CostPoints: 10, Active: true,
	}
	repo := &fakeRewardCatalogManager{reward: existing}
	svc := app.NewRewardAdminService(repo, newTestLogger())

	updated, err := svc.Update(t.Context(), hhID, existing.ID, "New name", "New description", 25, nil, nil)
	if err != nil {
		t.Fatalf("Update: unexpected error: %v", err)
	}
	if updated.Name != "New name" {
		t.Errorf("Name = %q, want %q", updated.Name, "New name")
	}
	if updated.CostPoints != 25 {
		t.Errorf("CostPoints = %d, want 25", updated.CostPoints)
	}
	if updated.ID != existing.ID {
		t.Errorf("ID = %v, want unchanged %v", updated.ID, existing.ID)
	}
	if !updated.Active {
		t.Error("Active = false, want preserved true")
	}
	if len(repo.updateCalls) != 1 {
		t.Fatalf("UpdateReward called %d times, want 1", len(repo.updateCalls))
	}
}

// ---------------------------------------------------------------------------
// RewardAdminService.Archive
// ---------------------------------------------------------------------------

func TestRewardAdminService_Archive_Success(t *testing.T) {
	repo := &fakeRewardCatalogManager{}
	svc := app.NewRewardAdminService(repo, newTestLogger())

	if err := svc.Archive(t.Context(), household.NewHouseholdID(), domain.NewRewardID()); err != nil {
		t.Fatalf("Archive: unexpected error: %v", err)
	}
	if repo.archiveCalls != 1 {
		t.Errorf("ArchiveReward called %d times, want 1", repo.archiveCalls)
	}
}

func TestRewardAdminService_Archive_NotFound(t *testing.T) {
	repo := &fakeRewardCatalogManager{archiveErr: domain.ErrRewardNotFound}
	svc := app.NewRewardAdminService(repo, newTestLogger())

	err := svc.Archive(t.Context(), household.NewHouseholdID(), domain.NewRewardID())
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("Archive(unknown reward) = %v, want ErrRewardNotFound", err)
	}
}
