package adapter_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ericfisherdev/nestova/internal/calendar/adapter"
	"github.com/ericfisherdev/nestova/internal/calendar/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// seedCalendarAccount creates a household, member, and connected calendar account
// and returns the account id (external_event rows reference it).
func seedCalendarAccount(t *testing.T, pool *pgxpool.Pool) (household.HouseholdID, domain.CalendarAccountID) {
	t.Helper()
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Alex")
	acc := newAccount(domain.NewCalendarAccountID(), member, hh)
	if err := adapter.NewCalendarAccountRepository(pool).Create(testCtx(t), acc); err != nil {
		t.Fatalf("seed calendar account: %v", err)
	}
	return hh, acc.ID
}

func extEvent(accountID domain.CalendarAccountID, externalID, title string, start time.Time) *domain.ExternalEvent {
	return &domain.ExternalEvent{
		ID:                domain.NewExternalEventID(),
		CalendarAccountID: accountID,
		ExternalID:        externalID,
		Title:             title,
		StartsAt:          start,
		EndsAt:            start.Add(time.Hour),
	}
}

func TestExternalEventUpsertIsIdempotent(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewExternalEventRepository(pool)
	hh, accID := seedCalendarAccount(t, pool)
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

	if err := repo.UpsertByExternalID(testCtx(t), extEvent(accID, "evt-1", "First", start)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Re-upsert the same external id with a new title -> one row, updated.
	if err := repo.UpsertByExternalID(testCtx(t), extEvent(accID, "evt-1", "Updated", start)); err != nil {
		t.Fatalf("Upsert (2): %v", err)
	}
	got, err := repo.ListByHouseholdRange(testCtx(t), hh, start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListByHouseholdRange: %v", err)
	}
	if len(got) != 1 || got[0].Title != "Updated" || got[0].ExternalID != "evt-1" {
		t.Fatalf("range returned %+v, want one event titled Updated", got)
	}
}

func TestExternalEventPersistsAllDayAndColor(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewExternalEventRepository(pool)
	hh, accID := seedCalendarAccount(t, pool)
	start := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)

	ev := extEvent(accID, "all-day", "Holiday", start)
	ev.AllDay = true
	ev.Color = "11" // a Google color id; stored in the nullable color column
	if err := repo.UpsertByExternalID(testCtx(t), ev); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := repo.ListByHouseholdRange(testCtx(t), hh, start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListByHouseholdRange: %v", err)
	}
	if len(got) != 1 || !got[0].AllDay || got[0].Color != "11" {
		t.Fatalf("round-trip = %+v, want AllDay=true Color=11", got)
	}

	// An empty color round-trips as empty (stored NULL).
	noColor := extEvent(accID, "no-color", "Plain", start)
	if err := repo.UpsertByExternalID(testCtx(t), noColor); err != nil {
		t.Fatalf("Upsert no-color: %v", err)
	}
	got, err = repo.ListByHouseholdRange(testCtx(t), hh, start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListByHouseholdRange: %v", err)
	}
	for _, e := range got {
		if e.ExternalID == "no-color" && e.Color != "" {
			t.Fatalf("no-color event Color = %q, want empty", e.Color)
		}
	}
}

func TestExternalEventDelete(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewExternalEventRepository(pool)
	hh, accID := seedCalendarAccount(t, pool)
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

	if err := repo.UpsertByExternalID(testCtx(t), extEvent(accID, "evt-1", "First", start)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := repo.DeleteByExternalID(testCtx(t), accID, "evt-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := repo.ListByHouseholdRange(testCtx(t), hh, start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListByHouseholdRange: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("range returned %d events after delete, want 0", len(got))
	}
	// Deleting a non-existent event is a no-op, not an error.
	if err := repo.DeleteByExternalID(testCtx(t), accID, "missing"); err != nil {
		t.Fatalf("Delete(missing) = %v, want nil", err)
	}
}

func TestExternalEventListByHouseholdRange(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewExternalEventRepository(pool)
	hh, accID := seedCalendarAccount(t, pool)

	inRange := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	before := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	after := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	for _, e := range []*domain.ExternalEvent{
		extEvent(accID, "in", "In", inRange),
		extEvent(accID, "before", "Before", before),
		extEvent(accID, "after", "After", after),
	} {
		if err := repo.UpsertByExternalID(testCtx(t), e); err != nil {
			t.Fatalf("Upsert %s: %v", e.ExternalID, err)
		}
	}

	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	got, err := repo.ListByHouseholdRange(testCtx(t), hh, from, to)
	if err != nil {
		t.Fatalf("ListByHouseholdRange: %v", err)
	}
	if len(got) != 1 || got[0].ExternalID != "in" {
		t.Fatalf("range returned %+v, want only the in-range event", got)
	}

	// A different household sees none of these events.
	otherHH := seedHousehold(t, pool)
	other, err := repo.ListByHouseholdRange(testCtx(t), otherHH, from, to)
	if err != nil {
		t.Fatalf("ListByHouseholdRange(other): %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("other household saw %d events, want 0", len(other))
	}
}
