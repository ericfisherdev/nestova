package adapter_test

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/tasks/adapter"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// RewardPostgresRepository.RedeemWithDebit — NES-129 deep-link idempotency
// (durable across process boundaries, gated)
//
// These tests exist specifically because the guard they exercise is a
// DATABASE constraint (reward_redemption_deep_link_signature_uniq, added by
// migration 00027), not an in-process one — the property under test is that
// it holds regardless of WHICH Go process or object attempts the second
// redemption, which no in-memory fake or single-process test can prove on
// its own.
// ---------------------------------------------------------------------------

// hashPtr is a small helper so a string literal can be passed where
// domain.RewardRedemption.DeepLinkSignatureHash (a *string) is expected.
func hashPtr(s string) *string { return &s }

// TestRedeemWithDebit_DeepLinkSignature_DurableAcrossRepositoryInstances
// proves the guard does not depend on any state held by a particular
// RewardPostgresRepository (or its pool) instance: a SECOND, entirely
// independent repository — its own pgxpool.Pool, its own Go object, sharing
// nothing in-process with the first — still rejects a redemption for a
// signature hash the FIRST repository already redeemed. This is what
// "durable across multiple server instances" means in practice: a
// multi-process deployment (or, as here, simply two independent connections)
// can never both succeed for the same signed link.
func TestRedeemWithDebit_DeepLinkSignature_DurableAcrossRepositoryInstances(t *testing.T) {
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the tasks repository tests")
	}

	pool1 := newTestPool(t) // resets and migrates the schema once
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool1)
	repo1 := adapter.NewRewardPostgresRepository(pool1)

	h, m1, _ := seedHousehold(t, pool1)
	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 1000)

	reward := &domain.Reward{
		ID: domain.NewRewardID(), HouseholdID: h.ID,
		Name: "Movie night", CostPoints: 50, Active: true,
	}
	if err := repo1.CreateReward(testCtx(t), reward); err != nil {
		t.Fatalf("CreateReward: %v", err)
	}

	const signatureHash = "deadbeef0000000000000000000000000000000000000000000000000000"

	// First redemption, via repo1: succeeds.
	first := buildRedemption(h.ID, m1, reward.ID)
	first.DeepLinkSignatureHash = hashPtr(signatureHash)
	if _, err := repo1.RedeemWithDebit(testCtx(t), first); err != nil {
		t.Fatalf("first RedeemWithDebit (repo1): %v", err)
	}

	// A SECOND, independent pool + repository — no shared Go state with
	// repo1 whatsoever beyond both pointing at the same database.
	pool2, err := db.New(testCtx(t), config.DBConfig{DSN: dsn, ConnTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect second pool: %v", err)
	}
	t.Cleanup(pool2.Close)
	repo2 := adapter.NewRewardPostgresRepository(pool2)

	second := buildRedemption(h.ID, m1, reward.ID)
	second.DeepLinkSignatureHash = hashPtr(signatureHash)
	_, err = repo2.RedeemWithDebit(testCtx(t), second)
	if !errors.Is(err, domain.ErrDeepLinkAlreadyRedeemed) {
		t.Fatalf("second RedeemWithDebit (repo2, same signature hash) error = %v, want %v", err, domain.ErrDeepLinkAlreadyRedeemed)
	}

	// Exactly one debit must have occurred, regardless of which repository
	// instance is asked — a second, successful debit would leave the
	// balance at 900, not 950.
	ledgerRepo2 := adapter.NewPointLedgerPostgresRepository(pool2)
	balance, err := ledgerRepo2.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != 950 {
		t.Errorf("balance after two RedeemWithDebit attempts (different repo instances) = %d, want 950 (debited exactly once)", balance)
	}
}

// TestRedeemWithDebit_DeepLinkSignature_DurableAfterSimulatedRestart proves
// the guard survives losing every in-memory object that participated in the
// first redemption: the pool AND repository that performed it are closed
// and discarded entirely (as a process restart would discard them) before a
// brand-new pool and repository, constructed from nothing but the DSN, are
// asked to redeem the SAME signature hash again.
func TestRedeemWithDebit_DeepLinkSignature_DurableAfterSimulatedRestart(t *testing.T) {
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the tasks repository tests")
	}

	// newTestPool's own t.Cleanup resets the schema at the end of the test
	// (not "on restart") — that is fine here: this test's own restart
	// simulation only closes and reopens the POOL, never the schema/data.
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)

	h, m1, _ := seedHousehold(t, pool)
	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 1000)

	reward := &domain.Reward{
		ID: domain.NewRewardID(), HouseholdID: h.ID,
		Name: "Movie night", CostPoints: 50, Active: true,
	}
	repoBeforeRestart := adapter.NewRewardPostgresRepository(pool)
	if err := repoBeforeRestart.CreateReward(testCtx(t), reward); err != nil {
		t.Fatalf("CreateReward: %v", err)
	}

	const signatureHash = "cafebabe0000000000000000000000000000000000000000000000000000"

	redemption := buildRedemption(h.ID, m1, reward.ID)
	redemption.DeepLinkSignatureHash = hashPtr(signatureHash)
	if _, err := repoBeforeRestart.RedeemWithDebit(testCtx(t), redemption); err != nil {
		t.Fatalf("RedeemWithDebit before simulated restart: %v", err)
	}

	// Simulate a process restart: close the pool that performed the
	// redemption entirely (this is what actually happens to every in-process
	// data structure — including the OLD, now-deleted, in-process
	// consumedSignatureStore this guard replaced — when the server process
	// exits), then reconnect from scratch.
	pool.Close()
	poolAfterRestart, err := db.New(testCtx(t), config.DBConfig{DSN: dsn, ConnTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("reconnect after simulated restart: %v", err)
	}
	t.Cleanup(poolAfterRestart.Close)
	repoAfterRestart := adapter.NewRewardPostgresRepository(poolAfterRestart)

	retry := buildRedemption(h.ID, m1, reward.ID)
	retry.DeepLinkSignatureHash = hashPtr(signatureHash)
	_, err = repoAfterRestart.RedeemWithDebit(testCtx(t), retry)
	if !errors.Is(err, domain.ErrDeepLinkAlreadyRedeemed) {
		t.Fatalf("RedeemWithDebit after simulated restart error = %v, want %v", err, domain.ErrDeepLinkAlreadyRedeemed)
	}

	ledgerRepoAfterRestart := adapter.NewPointLedgerPostgresRepository(poolAfterRestart)
	balance, err := ledgerRepoAfterRestart.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != 950 {
		t.Errorf("balance after simulated restart + retry = %d, want 950 (debited exactly once)", balance)
	}
}
