package adapter_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
	"github.com/ericfisherdev/nestova/internal/subscriptions/adapter"
	"github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

// newTestPool returns a pool against the NESTOVA_TEST_DATABASE_URL database with
// a freshly reset+migrated schema. It refuses to run unless the DSN's database
// name is "test" or ends with "_test" so migrate.Reset can never wipe a real
// database (a substring match could be satisfied by a host or password).
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the subscriptions adapter tests")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	name := strings.ToLower(cfg.ConnConfig.Database)
	if name != "test" && !strings.HasSuffix(name, "_test") {
		t.Fatalf("refusing to reset database %q; name must be \"test\" or end with \"_test\"", name)
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
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelCleanup()
		if err := migrate.Reset(cleanupCtx, dsn); err != nil {
			t.Logf("cleanup reset failed: %v", err)
		}
	})

	poolCtx, cancelPool := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelPool()
	pool, err := db.New(poolCtx, config.DBConfig{DSN: dsn, ConnTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func seedHousehold(t *testing.T, pool *pgxpool.Pool) household.HouseholdID {
	t.Helper()
	id := household.NewHouseholdID()
	if _, err := pool.Exec(testCtx(t), `INSERT INTO household (id, name) VALUES ($1, $2)`,
		id.String(), "The Fishers"); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	return id
}

func seedMember(t *testing.T, pool *pgxpool.Pool, hh household.HouseholdID, name string) household.MemberID {
	t.Helper()
	id := household.NewMemberID()
	if _, err := pool.Exec(testCtx(t),
		`INSERT INTO member (id, household_id, display_name, role, color_key) VALUES ($1, $2, $3, 'owner', 'sage')`,
		id.String(), hh.String(), name); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	return id
}

func dateUTC(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func newSubscription(t *testing.T, hh household.HouseholdID, name string, cents int64, cycle domain.Cycle, next time.Time, payer *household.MemberID, lead int) *domain.Subscription {
	t.Helper()
	amount, err := household.NewMoney(cents, "USD")
	if err != nil {
		t.Fatalf("NewMoney: %v", err)
	}
	sub := &domain.Subscription{
		ID:               domain.NewSubscriptionID(),
		HouseholdID:      hh,
		Name:             name,
		Amount:           amount,
		Cycle:            cycle,
		NextRenewalOn:    next,
		PayerID:          payer,
		Category:         "entertainment",
		ReminderLeadDays: lead,
		Active:           true,
	}
	if err := sub.Validate(); err != nil {
		t.Fatalf("subscription %q invalid: %v", name, err)
	}
	return sub
}

func TestCreateGetRoundTrip(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewSubscriptionRepository(pool)
	hh := seedHousehold(t, pool)
	payer := seedMember(t, pool, hh, "Alex")

	sub := newSubscription(t, hh, "Streaming", 1299, domain.CycleMonthly, dateUTC(2026, 7, 15), &payer, 3)
	if err := repo.Create(testCtx(t), sub); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sub.CreatedAt.IsZero() || sub.UpdatedAt.IsZero() {
		t.Fatal("Create did not populate timestamps")
	}

	got, err := repo.Get(testCtx(t), sub.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "Streaming" || got.Amount.Cents != 1299 || got.Amount.Currency != "USD" {
		t.Fatalf("Get returned %+v", got)
	}
	if got.Cycle != domain.CycleMonthly || !got.NextRenewalOn.Equal(dateUTC(2026, 7, 15)) {
		t.Fatalf("Get cycle/renewal mismatch: %+v", got)
	}
	if got.PayerID == nil || *got.PayerID != payer {
		t.Fatalf("Get payer = %v, want %v", got.PayerID, payer)
	}
}

func TestCreateWithoutPayer(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewSubscriptionRepository(pool)
	hh := seedHousehold(t, pool)

	sub := newSubscription(t, hh, "No payer", 500, domain.CycleWeekly, dateUTC(2026, 7, 1), nil, 0)
	if err := repo.Create(testCtx(t), sub); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.Get(testCtx(t), sub.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PayerID != nil {
		t.Fatalf("Get payer = %v, want nil", got.PayerID)
	}
}

func TestGetUnknown(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewSubscriptionRepository(pool)
	if _, err := repo.Get(testCtx(t), domain.NewSubscriptionID()); !errors.Is(err, domain.ErrSubscriptionNotFound) {
		t.Fatalf("Get(unknown) error = %v, want ErrSubscriptionNotFound", err)
	}
}

func TestCreateUnknownHousehold(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewSubscriptionRepository(pool)
	sub := newSubscription(t, household.NewHouseholdID(), "Orphan", 100, domain.CycleMonthly, dateUTC(2026, 7, 1), nil, 0)
	if err := repo.Create(testCtx(t), sub); !errors.Is(err, household.ErrHouseholdNotFound) {
		t.Fatalf("Create(unknown household) error = %v, want ErrHouseholdNotFound", err)
	}
}

func TestCreateUnknownPayer(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewSubscriptionRepository(pool)
	hh := seedHousehold(t, pool)
	stranger := household.NewMemberID() // never inserted
	sub := newSubscription(t, hh, "Bad payer", 100, domain.CycleMonthly, dateUTC(2026, 7, 1), &stranger, 0)
	if err := repo.Create(testCtx(t), sub); !errors.Is(err, household.ErrMemberNotFound) {
		t.Fatalf("Create(unknown payer) error = %v, want ErrMemberNotFound", err)
	}
}

func TestUpdate(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewSubscriptionRepository(pool)
	hh := seedHousehold(t, pool)
	sub := newSubscription(t, hh, "Before", 100, domain.CycleMonthly, dateUTC(2026, 7, 1), nil, 0)
	if err := repo.Create(testCtx(t), sub); err != nil {
		t.Fatalf("Create: %v", err)
	}

	sub.Name = "After"
	sub.Amount, _ = household.NewMoney(2500, "USD")
	sub.Cycle = domain.CycleYearly
	sub.NextRenewalOn = dateUTC(2027, 1, 1)
	sub.ReminderLeadDays = 7
	if err := repo.Update(testCtx(t), sub); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := repo.Get(testCtx(t), sub.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "After" || got.Amount.Cents != 2500 || got.Cycle != domain.CycleYearly ||
		!got.NextRenewalOn.Equal(dateUTC(2027, 1, 1)) || got.ReminderLeadDays != 7 {
		t.Fatalf("Update did not persist: %+v", got)
	}
}

func TestUpdateUnknown(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewSubscriptionRepository(pool)
	hh := seedHousehold(t, pool)
	sub := newSubscription(t, hh, "Ghost", 100, domain.CycleMonthly, dateUTC(2026, 7, 1), nil, 0)
	// never created
	if err := repo.Update(testCtx(t), sub); !errors.Is(err, domain.ErrSubscriptionNotFound) {
		t.Fatalf("Update(unknown) error = %v, want ErrSubscriptionNotFound", err)
	}
}

func TestDeactivate(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewSubscriptionRepository(pool)
	hh := seedHousehold(t, pool)
	sub := newSubscription(t, hh, "Active", 100, domain.CycleMonthly, dateUTC(2026, 7, 1), nil, 0)
	if err := repo.Create(testCtx(t), sub); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Deactivate(testCtx(t), sub.ID); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	got, err := repo.Get(testCtx(t), sub.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Active {
		t.Fatal("Deactivate did not clear active")
	}
	if err := repo.Deactivate(testCtx(t), domain.NewSubscriptionID()); !errors.Is(err, domain.ErrSubscriptionNotFound) {
		t.Fatalf("Deactivate(unknown) error = %v, want ErrSubscriptionNotFound", err)
	}
}

func TestListActiveByHouseholdIsolation(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewSubscriptionRepository(pool)
	hh1 := seedHousehold(t, pool)
	hh2 := seedHousehold(t, pool)

	active1 := newSubscription(t, hh1, "A", 100, domain.CycleMonthly, dateUTC(2026, 7, 5), nil, 0)
	inactive1 := newSubscription(t, hh1, "B", 100, domain.CycleMonthly, dateUTC(2026, 7, 1), nil, 0)
	other := newSubscription(t, hh2, "C", 100, domain.CycleMonthly, dateUTC(2026, 7, 1), nil, 0)
	for _, s := range []*domain.Subscription{active1, inactive1, other} {
		if err := repo.Create(testCtx(t), s); err != nil {
			t.Fatalf("Create %s: %v", s.Name, err)
		}
	}
	if err := repo.Deactivate(testCtx(t), inactive1.ID); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	got, err := repo.ListActiveByHousehold(testCtx(t), hh1)
	if err != nil {
		t.Fatalf("ListActiveByHousehold: %v", err)
	}
	if len(got) != 1 || got[0].ID != active1.ID {
		t.Fatalf("ListActiveByHousehold returned %d rows, want only the active hh1 subscription", len(got))
	}
}

func TestListDueForRenewal(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewSubscriptionRepository(pool)
	hh := seedHousehold(t, pool)

	// due: renews on the 12th with a 2-day lead -> window opens on the 10th.
	due := newSubscription(t, hh, "Due", 100, domain.CycleMonthly, dateUTC(2026, 7, 12), nil, 2)
	// not due: renews on the 20th with no lead.
	future := newSubscription(t, hh, "Future", 100, domain.CycleMonthly, dateUTC(2026, 7, 20), nil, 0)
	// custom: excluded even though it would otherwise be due.
	custom := newSubscription(t, hh, "Custom", 100, domain.CycleCustom, dateUTC(2026, 7, 1), nil, 0)
	for _, s := range []*domain.Subscription{due, future, custom} {
		if err := repo.Create(testCtx(t), s); err != nil {
			t.Fatalf("Create %s: %v", s.Name, err)
		}
	}

	pst := time.FixedZone("PST", -8*3600)
	cases := []struct {
		name    string
		asOf    time.Time
		wantDue bool // whether "Due" is in the window
	}{
		{"midnight UTC on the window-open date", dateUTC(2026, 7, 10), true},
		{"late UTC on the window-open date (time component ignored)", time.Date(2026, 7, 10, 23, 59, 59, 0, time.UTC), true},
		{"day before the window opens", time.Date(2026, 7, 9, 23, 59, 59, 0, time.UTC), false},
		// PST 2026-07-09T20:00 == 2026-07-10T04:00Z, whose UTC date is the 10th,
		// so the window is open: confirms the comparison uses the UTC date.
		{"non-UTC instant whose UTC date is the window-open date", time.Date(2026, 7, 9, 20, 0, 0, 0, pst), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.ListDueForRenewal(testCtx(t), tc.asOf)
			if err != nil {
				t.Fatalf("ListDueForRenewal: %v", err)
			}
			names := make([]string, len(got))
			for i, s := range got {
				names[i] = s.Name
				if s.Cycle == domain.CycleCustom {
					t.Fatalf("ListDueForRenewal returned a custom-cycle subscription %q", s.Name)
				}
				if s.ID == future.ID {
					t.Fatalf("ListDueForRenewal returned the not-yet-due subscription %q", s.Name)
				}
			}
			wantLen := 0
			if tc.wantDue {
				wantLen = 1
			}
			if len(got) != wantLen || (tc.wantDue && got[0].ID != due.ID) {
				t.Fatalf("ListDueForRenewal(%s) returned %v, wantDue=%v", tc.name, names, tc.wantDue)
			}
		})
	}
}
