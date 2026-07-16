package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/calendar/app"
	calendardomain "github.com/ericfisherdev/nestova/internal/calendar/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

func syncLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeSyncAccountStore struct {
	accounts     []*calendardomain.CalendarAccount
	listErr      error
	setSyncCalls []*string
	setSyncErr   error
}

func (f *fakeSyncAccountStore) ListAll(context.Context) ([]*calendardomain.CalendarAccount, error) {
	return f.accounts, f.listErr
}

func (f *fakeSyncAccountStore) SetSyncToken(_ context.Context, _ calendardomain.CalendarAccountID, syncToken *string) error {
	f.setSyncCalls = append(f.setSyncCalls, syncToken)
	return f.setSyncErr
}

type fakeEventRepo struct {
	upserts []*calendardomain.ExternalEvent
	deletes []string
}

func (f *fakeEventRepo) UpsertByExternalID(_ context.Context, e *calendardomain.ExternalEvent) error {
	f.upserts = append(f.upserts, e)
	return nil
}

func (f *fakeEventRepo) DeleteByExternalID(_ context.Context, _ calendardomain.CalendarAccountID, externalID string) error {
	f.deletes = append(f.deletes, externalID)
	return nil
}

func (f *fakeEventRepo) ListByHouseholdRange(context.Context, household.HouseholdID, time.Time, time.Time) ([]*calendardomain.ExternalEvent, error) {
	return nil, nil
}

type sourceCall struct{ calendarID, syncToken string }

type fakeEventSource struct {
	calls             []sourceCall
	fullEvents        []calendardomain.SyncedEvent
	incrementalEvents []calendardomain.SyncedEvent
	incrementalErr    error
	nextSyncToken     string
}

func (f *fakeEventSource) ListEvents(_ context.Context, _, calendarID, syncToken string) ([]calendardomain.SyncedEvent, string, error) {
	f.calls = append(f.calls, sourceCall{calendarID, syncToken})
	if syncToken != "" {
		if f.incrementalErr != nil {
			return nil, "", f.incrementalErr
		}
		return f.incrementalEvents, f.nextSyncToken, nil
	}
	return f.fullEvents, f.nextSyncToken, nil
}

type fakeTokenProvider struct {
	err    error
	failID *calendardomain.CalendarAccountID // when set, only this account's token call fails
}

func (f *fakeTokenProvider) ValidAccessToken(_ context.Context, id calendardomain.CalendarAccountID) (string, error) {
	if f.failID != nil && id == *f.failID {
		return "", errors.New("token refresh failed")
	}
	if f.err != nil {
		return "", f.err
	}
	return "access-token", nil
}

func account(syncToken string, calendarIDs ...string) *calendardomain.CalendarAccount {
	a := &calendardomain.CalendarAccount{
		ID:          calendardomain.NewCalendarAccountID(),
		MemberID:    household.NewMemberID(),
		HouseholdID: household.NewHouseholdID(),
		Provider:    calendardomain.ProviderGoogle,
		CalendarIDs: calendarIDs,
	}
	if syncToken != "" {
		a.SyncToken = &syncToken
	}
	return a
}

func mustSyncService(t *testing.T, store *fakeSyncAccountStore, events *fakeEventRepo, source *fakeEventSource, tokens *fakeTokenProvider) *app.SyncService {
	t.Helper()
	return mustSyncServiceWithRecorder(t, store, events, source, tokens, metrics.NopSyncRecorder{})
}

func mustSyncServiceWithRecorder(t *testing.T, store *fakeSyncAccountStore, events *fakeEventRepo, source *fakeEventSource, tokens *fakeTokenProvider, recorder metrics.SyncRecorder) *app.SyncService {
	t.Helper()
	svc, err := app.NewSyncService(store, events, source, tokens, syncLogger(), recorder)
	if err != nil {
		t.Fatalf("NewSyncService: %v", err)
	}
	return svc
}

// spySyncRecorder records SyncRecorder calls for assertion. Sync tests run the
// service synchronously via RunOnce, so no locking is needed.
type spySyncRecorder struct {
	eventsSynced  int
	accountErrors int
}

func (s *spySyncRecorder) AddEventsSynced(count int) { s.eventsSynced += count }

func (s *spySyncRecorder) IncAccountError() { s.accountErrors++ }

func event(id string) calendardomain.SyncedEvent {
	return calendardomain.SyncedEvent{
		ExternalID: id, Title: "Event " + id,
		StartsAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
		EndsAt:   time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}
}

func TestRunOnceUpsertsEventsAndPersistsToken(t *testing.T) {
	store := &fakeSyncAccountStore{accounts: []*calendardomain.CalendarAccount{account("", "primary")}}
	events := &fakeEventRepo{}
	source := &fakeEventSource{fullEvents: []calendardomain.SyncedEvent{event("a"), event("b")}, nextSyncToken: "next-token"}
	n, err := mustSyncService(t, store, events, source, &fakeTokenProvider{}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 || len(events.upserts) != 2 {
		t.Fatalf("processed %d (upserts %d), want 2", n, len(events.upserts))
	}
	if len(store.setSyncCalls) != 1 || store.setSyncCalls[0] == nil || *store.setSyncCalls[0] != "next-token" {
		t.Fatalf("SetSyncToken calls = %v, want one with next-token", store.setSyncCalls)
	}
}

func TestRunOnceDeletesCancelledEvents(t *testing.T) {
	store := &fakeSyncAccountStore{accounts: []*calendardomain.CalendarAccount{account("", "primary")}}
	events := &fakeEventRepo{}
	cancelled := calendardomain.SyncedEvent{ExternalID: "gone", Cancelled: true}
	source := &fakeEventSource{fullEvents: []calendardomain.SyncedEvent{event("a"), cancelled}, nextSyncToken: "t"}
	if _, err := mustSyncService(t, store, events, source, &fakeTokenProvider{}).RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(events.upserts) != 1 || len(events.deletes) != 1 || events.deletes[0] != "gone" {
		t.Fatalf("upserts=%d deletes=%v, want 1 upsert and delete of gone", len(events.upserts), events.deletes)
	}
}

func TestRunOnceFullResyncOnInvalidToken(t *testing.T) {
	store := &fakeSyncAccountStore{accounts: []*calendardomain.CalendarAccount{account("stale-token", "primary")}}
	events := &fakeEventRepo{}
	source := &fakeEventSource{
		incrementalErr: calendardomain.ErrSyncTokenInvalid,
		fullEvents:     []calendardomain.SyncedEvent{event("a")},
		nextSyncToken:  "fresh",
	}
	if _, err := mustSyncService(t, store, events, source, &fakeTokenProvider{}).RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(source.calls) != 2 || source.calls[0].syncToken != "stale-token" || source.calls[1].syncToken != "" {
		t.Fatalf("source calls = %+v, want [stale-token, full-resync]", source.calls)
	}
	if len(events.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1 from the full resync", len(events.upserts))
	}
}

func TestRunOnceSyncsOnlyPrimaryCalendar(t *testing.T) {
	// An account with several calendar ids syncs only the first (primary); per-
	// calendar sync tokens for the rest are future work.
	store := &fakeSyncAccountStore{accounts: []*calendardomain.CalendarAccount{account("", "primary", "secondary")}}
	source := &fakeEventSource{fullEvents: []calendardomain.SyncedEvent{event("a")}, nextSyncToken: "t"}
	if _, err := mustSyncService(t, store, &fakeEventRepo{}, source, &fakeTokenProvider{}).RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(source.calls) != 1 || source.calls[0].calendarID != "primary" {
		t.Fatalf("source calls = %+v, want a single call for the primary calendar", source.calls)
	}
}

func TestRunOnceSkipsAccountWithoutCalendars(t *testing.T) {
	store := &fakeSyncAccountStore{accounts: []*calendardomain.CalendarAccount{account("")}} // no calendar ids
	source := &fakeEventSource{}
	if _, err := mustSyncService(t, store, &fakeEventRepo{}, source, &fakeTokenProvider{}).RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(source.calls) != 0 {
		t.Fatalf("source was called %d times for an account with no calendars, want 0", len(source.calls))
	}
}

func TestRunOnceIsolatesAccountErrors(t *testing.T) {
	bad := account("", "primary")
	good := account("", "primary")
	store := &fakeSyncAccountStore{accounts: []*calendardomain.CalendarAccount{bad, good}}
	events := &fakeEventRepo{}
	source := &fakeEventSource{fullEvents: []calendardomain.SyncedEvent{event("a")}, nextSyncToken: "t"}
	// A token provider that fails for the first account only.
	tokens := &fakeTokenProvider{failID: &bad.ID}
	n, err := mustSyncService(t, store, events, source, tokens).RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce error = nil, want the first account's error recorded")
	}
	if n != 1 || len(events.upserts) != 1 {
		t.Fatalf("the healthy account should still sync: processed %d, upserts %d", n, len(events.upserts))
	}
}

func TestRunOnceReturnsErrorWhenSyncTokenPersistFails(t *testing.T) {
	store := &fakeSyncAccountStore{
		accounts:   []*calendardomain.CalendarAccount{account("", "primary")},
		setSyncErr: errors.New("db write failed"),
	}
	events := &fakeEventRepo{}
	source := &fakeEventSource{fullEvents: []calendardomain.SyncedEvent{event("a"), event("b")}, nextSyncToken: "token"}
	n, err := mustSyncService(t, store, events, source, &fakeTokenProvider{}).RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce error = nil, want the SetSyncToken failure surfaced")
	}
	// A failed account's partial count is discarded from the total (matching the
	// restock scheduler), but its event side effects still occurred.
	if n != 0 {
		t.Fatalf("processed = %d, want 0 (the failed account's count is discarded)", n)
	}
	if len(events.upserts) != 2 {
		t.Fatalf("upserts = %d, want 2 (events were applied before the token persist failed)", len(events.upserts))
	}
}

func TestRunOnceSkipsMalformedEvents(t *testing.T) {
	store := &fakeSyncAccountStore{accounts: []*calendardomain.CalendarAccount{account("", "primary")}}
	events := &fakeEventRepo{}
	// The second event is malformed (end precedes start) and must be skipped
	// without aborting the sync of the valid first event.
	malformed := calendardomain.SyncedEvent{
		ExternalID: "bad",
		StartsAt:   time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		EndsAt:     time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	source := &fakeEventSource{fullEvents: []calendardomain.SyncedEvent{event("a"), malformed}, nextSyncToken: "t"}
	if _, err := mustSyncService(t, store, events, source, &fakeTokenProvider{}).RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(events.upserts) != 1 || events.upserts[0].ExternalID != "a" {
		t.Fatalf("upserts = %d, want only the valid event 'a'", len(events.upserts))
	}
}

func TestRunOnceRespectsCancelledContext(t *testing.T) {
	store := &fakeSyncAccountStore{accounts: []*calendardomain.CalendarAccount{account("", "primary")}}
	source := &fakeEventSource{fullEvents: []calendardomain.SyncedEvent{event("a")}, nextSyncToken: "t"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mustSyncService(t, store, &fakeEventRepo{}, source, &fakeTokenProvider{}).RunOnce(ctx); err == nil {
		t.Fatal("RunOnce(cancelled ctx) error = nil, want context cancellation")
	}
}

func TestNewSyncServiceValidatesDeps(t *testing.T) {
	store := &fakeSyncAccountStore{}
	events := &fakeEventRepo{}
	source := &fakeEventSource{}
	tokens := &fakeTokenProvider{}
	log := syncLogger()
	rec := metrics.NopSyncRecorder{}
	cases := []func() (*app.SyncService, error){
		func() (*app.SyncService, error) { return app.NewSyncService(nil, events, source, tokens, log, rec) },
		func() (*app.SyncService, error) { return app.NewSyncService(store, nil, source, tokens, log, rec) },
		func() (*app.SyncService, error) { return app.NewSyncService(store, events, nil, tokens, log, rec) },
		func() (*app.SyncService, error) { return app.NewSyncService(store, events, source, nil, log, rec) },
		func() (*app.SyncService, error) { return app.NewSyncService(store, events, source, tokens, nil, rec) },
		func() (*app.SyncService, error) { return app.NewSyncService(store, events, source, tokens, log, nil) },
	}
	for i, fn := range cases {
		if _, err := fn(); err == nil {
			t.Errorf("case %d: expected an error, got nil", i)
		}
	}
}

func TestRunOnceRecordsSyncedEventCount(t *testing.T) {
	store := &fakeSyncAccountStore{accounts: []*calendardomain.CalendarAccount{account("", "primary")}}
	source := &fakeEventSource{fullEvents: []calendardomain.SyncedEvent{event("a"), event("b")}, nextSyncToken: "t"}
	spy := &spySyncRecorder{}
	svc := mustSyncServiceWithRecorder(t, store, &fakeEventRepo{}, source, &fakeTokenProvider{}, spy)
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if spy.eventsSynced != 2 {
		t.Errorf("recorded events synced = %d, want 2", spy.eventsSynced)
	}
	if spy.accountErrors != 0 {
		t.Errorf("recorded account errors = %d, want 0", spy.accountErrors)
	}
}

func TestRunOnceRecordsAccountErrorAndStillCountsHealthyAccounts(t *testing.T) {
	bad := account("", "primary")
	good := account("", "primary")
	store := &fakeSyncAccountStore{accounts: []*calendardomain.CalendarAccount{bad, good}}
	source := &fakeEventSource{fullEvents: []calendardomain.SyncedEvent{event("a")}, nextSyncToken: "t"}
	// A token provider that fails for the first account only.
	tokens := &fakeTokenProvider{failID: &bad.ID}
	spy := &spySyncRecorder{}
	svc := mustSyncServiceWithRecorder(t, store, &fakeEventRepo{}, source, tokens, spy)
	if _, err := svc.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce error = nil, want the failing account's error recorded")
	}
	if spy.accountErrors != 1 {
		t.Errorf("recorded account errors = %d, want 1", spy.accountErrors)
	}
	if spy.eventsSynced != 1 {
		t.Errorf("recorded events synced = %d, want 1 (the healthy account's event)", spy.eventsSynced)
	}
}
