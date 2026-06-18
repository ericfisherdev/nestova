package adapter_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	householdadapter "github.com/ericfisherdev/nestova/internal/household/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
)

// newTestPool connects to NESTOVA_TEST_DATABASE_URL and applies migrations, or
// skips when the env var is unset (keeping the default test run hermetic).
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the notify adapter tests")
	}

	setupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := migrate.Reset(setupCtx, dsn); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := migrate.Up(setupCtx, dsn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := migrate.Reset(cleanupCtx, dsn); err != nil {
			t.Logf("cleanup reset failed: %v", err)
		}
	})

	pool, err := db.New(setupCtx, config.DBConfig{DSN: dsn, ConnTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testCtx returns a per-call context bounded so a slow/unresponsive database
// fails the test rather than hanging it.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// seedHouseholdAndMember creates a household and one member to satisfy the
// notification table's foreign key constraints.
func seedHouseholdAndMember(t *testing.T, pool *pgxpool.Pool) (household.HouseholdID, household.MemberID) {
	t.Helper()
	hhRepo := householdadapter.NewPostgresRepository(pool)

	h := &household.Household{ID: household.NewHouseholdID(), Name: "Test Household"}
	if err := hhRepo.CreateHousehold(testCtx(t), h); err != nil {
		t.Fatalf("CreateHousehold: %v", err)
	}
	m := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: h.ID,
		DisplayName: "Test Member",
		Role:        household.RoleAdult,
		Color:       household.ColorSage,
	}
	if err := hhRepo.AddMember(testCtx(t), m); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	return h.ID, m.ID
}

func newPendingNotification(hhID household.HouseholdID, scheduledFor time.Time) *domain.Notification {
	return &domain.Notification{
		ID:           domain.NewNotificationID(),
		HouseholdID:  hhID,
		Channel:      domain.ChannelInApp,
		Title:        "Test Notification",
		Body:         "Test body text",
		ScheduledFor: scheduledFor,
		Status:       domain.StatusPending,
	}
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

func TestEnqueueAndClaimDue(t *testing.T) {
	pool := newTestPool(t)
	repo := notifyadapter.NewOutboxRepository(pool)

	hhID, _ := seedHouseholdAndMember(t, pool)

	n := newPendingNotification(hhID, time.Now().Add(-time.Second))
	if err := repo.Enqueue(testCtx(t), n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if n.CreatedAt.IsZero() {
		t.Error("Enqueue did not populate CreatedAt")
	}

	claimed, err := repo.ClaimDue(testCtx(t), 10)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimDue returned %d notifications, want 1", len(claimed))
	}
	got := claimed[0]
	if got.ID != n.ID {
		t.Errorf("ClaimDue ID = %v, want %v", got.ID, n.ID)
	}
	if got.Channel != domain.ChannelInApp {
		t.Errorf("ClaimDue channel = %v, want %v", got.Channel, domain.ChannelInApp)
	}
	// Optimistic claim: status is 'sent' after claim.
	if got.Status != domain.StatusSent {
		t.Errorf("ClaimDue status = %v, want sent", got.Status)
	}
}

func TestClaimDueRespectsFutureScheduledFor(t *testing.T) {
	pool := newTestPool(t)
	repo := notifyadapter.NewOutboxRepository(pool)

	hhID, _ := seedHouseholdAndMember(t, pool)

	// Future notification must NOT be claimed.
	future := newPendingNotification(hhID, time.Now().Add(time.Hour))
	if err := repo.Enqueue(testCtx(t), future); err != nil {
		t.Fatalf("Enqueue(future): %v", err)
	}

	// Past notification MUST be claimed.
	past := newPendingNotification(hhID, time.Now().Add(-time.Second))
	if err := repo.Enqueue(testCtx(t), past); err != nil {
		t.Fatalf("Enqueue(past): %v", err)
	}

	claimed, err := repo.ClaimDue(testCtx(t), 10)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimDue returned %d notifications, want 1 (only the past one)", len(claimed))
	}
	if claimed[0].ID != past.ID {
		t.Errorf("ClaimDue returned wrong notification: got %v, want %v", claimed[0].ID, past.ID)
	}
}

func TestClaimDue_Concurrency_NoDoubleClaimWithSKIPLOCKED(t *testing.T) {
	// Connect a second pool to the same database to simulate two independent
	// dispatcher instances. Each calls ClaimDue concurrently; SKIP LOCKED must
	// ensure the single due notification is claimed by exactly one of them.
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the notify adapter tests")
	}

	pool1 := newTestPool(t)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	pool2, err := db.New(ctx2, config.DBConfig{DSN: dsn, ConnTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect pool2: %v", err)
	}
	t.Cleanup(pool2.Close)

	repo1 := notifyadapter.NewOutboxRepository(pool1)
	repo2 := notifyadapter.NewOutboxRepository(pool2)

	hhID, _ := seedHouseholdAndMember(t, pool1)

	// Enqueue one due notification.
	n := newPendingNotification(hhID, time.Now().Add(-time.Second))
	if err := repo1.Enqueue(testCtx(t), n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Run ClaimDue from both repos concurrently.
	type result struct {
		claimed []*domain.Notification
		err     error
	}

	var wg, ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	results := make([]result, 2)

	for i, repo := range []domain.Outbox{repo1, repo2} {
		wg.Add(1)
		go func(idx int, r domain.Outbox) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Both goroutines block here until released together, forcing the two
			// ClaimDue calls to actually contend rather than run sequentially.
			ready.Done()
			<-start
			claimed, err := r.ClaimDue(ctx, 10)
			results[idx] = result{claimed: claimed, err: err}
		}(i, repo)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	// Verify: no errors.
	for i, res := range results {
		if res.err != nil {
			t.Errorf("repo%d ClaimDue error = %v", i+1, res.err)
		}
	}

	// Verify: the notification was claimed exactly once across both goroutines.
	totalClaimed := len(results[0].claimed) + len(results[1].claimed)
	if totalClaimed != 1 {
		t.Errorf("total claimed = %d, want 1 (SKIP LOCKED must prevent double-claim)", totalClaimed)
	}
}

func TestMarkSent_Transitions(t *testing.T) {
	pool := newTestPool(t)
	repo := notifyadapter.NewOutboxRepository(pool)

	hhID, _ := seedHouseholdAndMember(t, pool)

	n := newPendingNotification(hhID, time.Now().Add(-time.Second))
	if err := repo.Enqueue(testCtx(t), n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := repo.MarkSent(testCtx(t), n.ID); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	// Second MarkSent on same row must also succeed (idempotent update).
	if err := repo.MarkSent(testCtx(t), n.ID); err != nil {
		t.Errorf("MarkSent (idempotent) error = %v, want nil", err)
	}
}

func TestMarkSent_UnknownID_ReturnsErrNotificationNotFound(t *testing.T) {
	pool := newTestPool(t)
	repo := notifyadapter.NewOutboxRepository(pool)

	err := repo.MarkSent(testCtx(t), domain.NewNotificationID())
	if !errors.Is(err, domain.ErrNotificationNotFound) {
		t.Errorf("MarkSent(unknown) error = %v, want ErrNotificationNotFound", err)
	}
}

func TestMarkFailed_Transitions(t *testing.T) {
	pool := newTestPool(t)
	repo := notifyadapter.NewOutboxRepository(pool)

	hhID, _ := seedHouseholdAndMember(t, pool)

	n := newPendingNotification(hhID, time.Now().Add(-time.Second))
	if err := repo.Enqueue(testCtx(t), n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := repo.MarkFailed(testCtx(t), n.ID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
}

func TestMarkFailed_UnknownID_ReturnsErrNotificationNotFound(t *testing.T) {
	pool := newTestPool(t)
	repo := notifyadapter.NewOutboxRepository(pool)

	err := repo.MarkFailed(testCtx(t), domain.NewNotificationID())
	if !errors.Is(err, domain.ErrNotificationNotFound) {
		t.Errorf("MarkFailed(unknown) error = %v, want ErrNotificationNotFound", err)
	}
}

func TestEnqueue_MemberTargeted(t *testing.T) {
	pool := newTestPool(t)
	repo := notifyadapter.NewOutboxRepository(pool)

	hhID, memberID := seedHouseholdAndMember(t, pool)

	n := newPendingNotification(hhID, time.Now().Add(-time.Second))
	n.MemberID = &memberID

	if err := repo.Enqueue(testCtx(t), n); err != nil {
		t.Fatalf("Enqueue (member-targeted): %v", err)
	}

	claimed, err := repo.ClaimDue(testCtx(t), 10)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimDue returned %d, want 1", len(claimed))
	}
	if claimed[0].MemberID == nil || *claimed[0].MemberID != memberID {
		t.Errorf("ClaimDue MemberID = %v, want %v", claimed[0].MemberID, memberID)
	}
}
