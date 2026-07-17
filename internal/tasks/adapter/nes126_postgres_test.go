package adapter_test

import (
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/adapter"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

// seedRewardFull creates and persists a reward with every NES-126 field set,
// for tests that need to round-trip description/image_ref/quantity_available.
func seedRewardFull(
	t *testing.T,
	repo *adapter.RewardPostgresRepository,
	householdID household.HouseholdID,
	name string,
	costPoints int,
	quantityAvailable *int,
) *domain.Reward {
	t.Helper()
	imageRef := "🎮"
	r := &domain.Reward{
		ID:                domain.NewRewardID(),
		HouseholdID:       householdID,
		Name:              name,
		Description:       "A test reward",
		CostPoints:        costPoints,
		ImageRef:          &imageRef,
		QuantityAvailable: quantityAvailable,
		Active:            true,
	}
	if err := repo.CreateReward(testCtx(t), r); err != nil {
		t.Fatalf("seedRewardFull(%q): %v", name, err)
	}
	return r
}

// seedRedemption inserts a reward_redemption row with the given status via
// RewardPostgresRepository.Redeem, which — unlike RedeemWithDebit — performs
// no balance check, making it a convenient seeding vehicle for tests that only
// care about redemption existence/status, not point balances.
func seedRedemption(
	t *testing.T,
	repo *adapter.RewardPostgresRepository,
	householdID household.HouseholdID,
	rewardID domain.RewardID,
	memberID household.MemberID,
	status domain.RedemptionStatus,
) *domain.RewardRedemption {
	t.Helper()
	now := time.Now().UTC()
	redemption := &domain.RewardRedemption{
		ID:          domain.NewRewardRedemptionID(),
		HouseholdID: householdID,
		RewardID:    rewardID,
		MemberID:    memberID,
		Status:      status,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := repo.Redeem(testCtx(t), redemption); err != nil {
		t.Fatalf("seedRedemption: %v", err)
	}
	return redemption
}

func intPtr(n int) *int { return &n }

// ---------------------------------------------------------------------------
// CreateReward — NES-126 field round-trip
// ---------------------------------------------------------------------------

// TestRewardRepository_CreateReward_RoundTripsNewFields verifies that
// Description, ImageRef, and QuantityAvailable persist and are readable back
// via GetReward (NES-126).
func TestRewardRepository_CreateReward_RoundTripsNewFields(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	quantity := 5
	reward := seedRewardFull(t, rewardRepo, h.ID, "Extra screen time", 20, &quantity)

	got, err := rewardRepo.GetReward(testCtx(t), h.ID, reward.ID)
	if err != nil {
		t.Fatalf("GetReward: %v", err)
	}
	if got.Description != "A test reward" {
		t.Errorf("Description = %q, want %q", got.Description, "A test reward")
	}
	if got.ImageRef == nil || *got.ImageRef != "🎮" {
		t.Errorf("ImageRef = %v, want 🎮", got.ImageRef)
	}
	if got.QuantityAvailable == nil || *got.QuantityAvailable != 5 {
		t.Errorf("QuantityAvailable = %v, want 5", got.QuantityAvailable)
	}
}

// TestRewardRepository_CreateReward_NilQuantityMeansUnlimited verifies that a
// nil QuantityAvailable round-trips as nil (unlimited stock).
func TestRewardRepository_CreateReward_NilQuantityMeansUnlimited(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	reward := seedRewardFull(t, rewardRepo, h.ID, "Unlimited reward", 10, nil)

	got, err := rewardRepo.GetReward(testCtx(t), h.ID, reward.ID)
	if err != nil {
		t.Fatalf("GetReward: %v", err)
	}
	if got.QuantityAvailable != nil {
		t.Errorf("QuantityAvailable = %v, want nil", got.QuantityAvailable)
	}
}

// ---------------------------------------------------------------------------
// ListStorefrontRewards — NES-126 AC2/AC3
// ---------------------------------------------------------------------------

// TestRewardRepository_ListStorefrontRewards_ExcludesArchived verifies that an
// archived (Active = false) reward never appears in the storefront listing.
func TestRewardRepository_ListStorefrontRewards_ExcludesArchived(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	active := seedReward(t, rewardRepo, h.ID, "Active reward", 10)
	archived := seedReward(t, rewardRepo, h.ID, "Archived reward", 10)
	if err := rewardRepo.ArchiveReward(testCtx(t), h.ID, archived.ID); err != nil {
		t.Fatalf("ArchiveReward: %v", err)
	}

	got, err := rewardRepo.ListStorefrontRewards(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListStorefrontRewards: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListStorefrontRewards = %d rewards, want 1", len(got))
	}
	if got[0].Reward.ID != active.ID {
		t.Errorf("ListStorefrontRewards[0].ID = %v, want the active reward %v", got[0].Reward.ID, active.ID)
	}
}

// TestRewardRepository_ListStorefrontRewards_UnlimitedStockAlwaysIncluded
// verifies that a reward with a nil QuantityAvailable is always returned,
// regardless of how many times it has been redeemed, with a nil
// RemainingStock.
func TestRewardRepository_ListStorefrontRewards_UnlimitedStockAlwaysIncluded(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedRewardFull(t, rewardRepo, h.ID, "Unlimited reward", 10, nil)
	seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionPending)
	seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionFulfilled)

	got, err := rewardRepo.ListStorefrontRewards(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListStorefrontRewards: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListStorefrontRewards = %d rewards, want 1", len(got))
	}
	if got[0].RemainingStock != nil {
		t.Errorf("RemainingStock = %v, want nil (unlimited)", got[0].RemainingStock)
	}
}

// TestRewardRepository_ListStorefrontRewards_ExcludesOutOfStock verifies that
// a reward with a quantity cap is excluded once its non-cancelled redemption
// count reaches the cap (NES-126 AC2).
func TestRewardRepository_ListStorefrontRewards_ExcludesOutOfStock(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedRewardFull(t, rewardRepo, h.ID, "Limited reward", 10, intPtr(1))
	seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionPending)

	got, err := rewardRepo.ListStorefrontRewards(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListStorefrontRewards: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListStorefrontRewards = %d rewards, want 0 (sold out)", len(got))
	}
}

// TestRewardRepository_ListStorefrontRewards_ComputesRemainingStock verifies
// that RemainingStock reflects QuantityAvailable minus the non-cancelled
// redemption count.
func TestRewardRepository_ListStorefrontRewards_ComputesRemainingStock(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedRewardFull(t, rewardRepo, h.ID, "Five-stock reward", 10, intPtr(5))
	seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionPending)
	seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionFulfilled)

	got, err := rewardRepo.ListStorefrontRewards(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListStorefrontRewards: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListStorefrontRewards = %d rewards, want 1", len(got))
	}
	if got[0].RemainingStock == nil || *got[0].RemainingStock != 3 {
		t.Errorf("RemainingStock = %v, want 3 (5 - 2)", got[0].RemainingStock)
	}
}

// TestRewardRepository_ListStorefrontRewards_IgnoresCancelledRedemptions
// verifies that a cancelled redemption does not count against the reward's
// stock cap.
func TestRewardRepository_ListStorefrontRewards_IgnoresCancelledRedemptions(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedRewardFull(t, rewardRepo, h.ID, "One-stock reward", 10, intPtr(1))
	seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionCancelled)

	got, err := rewardRepo.ListStorefrontRewards(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListStorefrontRewards: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListStorefrontRewards = %d rewards, want 1 (cancelled redemption must not count)", len(got))
	}
	if got[0].RemainingStock == nil || *got[0].RemainingStock != 1 {
		t.Errorf("RemainingStock = %v, want 1 (cancelled redemption excluded from the count)", got[0].RemainingStock)
	}
}

// TestRewardRepository_ListStorefrontRewards_IgnoresDeniedRedemptions
// verifies that a denied redemption does not count against the reward's
// stock cap either (NES-127), mirroring the cancelled case immediately
// above.
func TestRewardRepository_ListStorefrontRewards_IgnoresDeniedRedemptions(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedRewardFull(t, rewardRepo, h.ID, "One-stock reward", 10, intPtr(1))
	seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionDenied)

	got, err := rewardRepo.ListStorefrontRewards(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListStorefrontRewards: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListStorefrontRewards = %d rewards, want 1 (denied redemption must not count)", len(got))
	}
	if got[0].RemainingStock == nil || *got[0].RemainingStock != 1 {
		t.Errorf("RemainingStock = %v, want 1 (denied redemption excluded from the count)", got[0].RemainingStock)
	}
}

// TestRewardRepository_ListStorefrontRewards_Empty verifies that an empty
// slice (not an error) is returned when the household has no qualifying
// rewards.
func TestRewardRepository_ListStorefrontRewards_Empty(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	got, err := rewardRepo.ListStorefrontRewards(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListStorefrontRewards(empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListStorefrontRewards(empty) = %d rows, want 0", len(got))
	}
}

// ---------------------------------------------------------------------------
// ListAllRewards — NES-126 AC1 (admin catalogue)
// ---------------------------------------------------------------------------

// TestRewardRepository_ListAllRewards_IncludesArchived verifies that the
// admin listing includes both active and archived rewards, unlike
// ListActiveRewards/ListStorefrontRewards.
func TestRewardRepository_ListAllRewards_IncludesArchived(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	active := seedReward(t, rewardRepo, h.ID, "Active reward", 10)
	archived := seedReward(t, rewardRepo, h.ID, "Archived reward", 10)
	if err := rewardRepo.ArchiveReward(testCtx(t), h.ID, archived.ID); err != nil {
		t.Fatalf("ArchiveReward: %v", err)
	}

	got, err := rewardRepo.ListAllRewards(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListAllRewards: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListAllRewards = %d rewards, want 2", len(got))
	}
	ids := map[domain.RewardID]bool{}
	for _, r := range got {
		ids[r.ID] = true
	}
	if !ids[active.ID] || !ids[archived.ID] {
		t.Errorf("ListAllRewards missing one of the seeded rewards: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// UpdateReward — NES-126 AC1
// ---------------------------------------------------------------------------

// TestRewardRepository_UpdateReward_PersistsChanges verifies that UpdateReward
// persists new field values and leaves Active untouched.
func TestRewardRepository_UpdateReward_PersistsChanges(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Original name", 10)

	newImageRef := "🍕"
	reward.Name = "Updated name"
	reward.Description = "Updated description"
	reward.CostPoints = 25
	reward.ImageRef = &newImageRef
	reward.QuantityAvailable = intPtr(3)

	if err := rewardRepo.UpdateReward(testCtx(t), reward); err != nil {
		t.Fatalf("UpdateReward: %v", err)
	}

	got, err := rewardRepo.GetReward(testCtx(t), h.ID, reward.ID)
	if err != nil {
		t.Fatalf("GetReward after update: %v", err)
	}
	if got.Name != "Updated name" {
		t.Errorf("Name = %q, want %q", got.Name, "Updated name")
	}
	if got.CostPoints != 25 {
		t.Errorf("CostPoints = %d, want 25", got.CostPoints)
	}
	if got.QuantityAvailable == nil || *got.QuantityAvailable != 3 {
		t.Errorf("QuantityAvailable = %v, want 3", got.QuantityAvailable)
	}
	if !got.Active {
		t.Error("Active = false, want unchanged true")
	}
}

// TestRewardRepository_UpdateReward_NotFound verifies that UpdateReward
// returns ErrRewardNotFound for an unknown id.
func TestRewardRepository_UpdateReward_NotFound(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	unknown := &domain.Reward{
		ID: domain.NewRewardID(), HouseholdID: h.ID, Name: "Ghost", CostPoints: 10, Active: true,
	}
	err := rewardRepo.UpdateReward(testCtx(t), unknown)
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("UpdateReward(unknown) = %v, want ErrRewardNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// ArchiveReward — NES-126 AC1
// ---------------------------------------------------------------------------

// TestRewardRepository_ArchiveReward_SetsInactive verifies that
// ArchiveReward flips Active to false without touching redemption history.
func TestRewardRepository_ArchiveReward_SetsInactive(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "To archive", 10)

	if err := rewardRepo.ArchiveReward(testCtx(t), h.ID, reward.ID); err != nil {
		t.Fatalf("ArchiveReward: %v", err)
	}

	got, err := rewardRepo.GetReward(testCtx(t), h.ID, reward.ID)
	if err != nil {
		t.Fatalf("GetReward after archive: %v", err)
	}
	if got.Active {
		t.Error("Active = true after ArchiveReward, want false")
	}
}

// TestRewardRepository_ArchiveReward_NotFound verifies that ArchiveReward
// returns ErrRewardNotFound for an unknown id.
func TestRewardRepository_ArchiveReward_NotFound(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	err := rewardRepo.ArchiveReward(testCtx(t), h.ID, domain.NewRewardID())
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("ArchiveReward(unknown) = %v, want ErrRewardNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// DeleteReward — NES-126 AC5 (cannot hard-delete a reward with redemptions)
// ---------------------------------------------------------------------------

// TestRewardRepository_DeleteReward_SucceedsWithoutRedemptions verifies that
// a reward with no redemption history can be hard-deleted.
func TestRewardRepository_DeleteReward_SucceedsWithoutRedemptions(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "No redemptions", 10)

	if err := rewardRepo.DeleteReward(testCtx(t), h.ID, reward.ID); err != nil {
		t.Fatalf("DeleteReward: %v", err)
	}

	if _, err := rewardRepo.GetReward(testCtx(t), h.ID, reward.ID); !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("GetReward after delete = %v, want ErrRewardNotFound", err)
	}
}

// TestRewardRepository_DeleteReward_BlockedByRedemptions is the core NES-126
// AC5 assertion: a reward with at least one redemption can never be
// hard-deleted — the reward_redemption_reward_fk ON DELETE RESTRICT
// constraint (00024_reward_catalog_admin.sql) rejects it, and
// DeleteReward maps that rejection to ErrRewardHasRedemptions. The
// redemption row (and the reward) must both still exist afterward.
func TestRewardRepository_DeleteReward_BlockedByRedemptions(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Has redemptions", 10)
	seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionFulfilled)

	err := rewardRepo.DeleteReward(testCtx(t), h.ID, reward.ID)
	if !errors.Is(err, domain.ErrRewardHasRedemptions) {
		t.Fatalf("DeleteReward(reward with redemptions) = %v, want ErrRewardHasRedemptions", err)
	}

	// The reward must still exist — the delete was rejected, not partially applied.
	if _, getErr := rewardRepo.GetReward(testCtx(t), h.ID, reward.ID); getErr != nil {
		t.Errorf("GetReward after blocked delete: %v, want the reward to still exist", getErr)
	}
}

// TestRewardRepository_DeleteReward_NotFound verifies that DeleteReward
// returns ErrRewardNotFound for an unknown id.
func TestRewardRepository_DeleteReward_NotFound(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	err := rewardRepo.DeleteReward(testCtx(t), h.ID, domain.NewRewardID())
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("DeleteReward(unknown) = %v, want ErrRewardNotFound", err)
	}
}
