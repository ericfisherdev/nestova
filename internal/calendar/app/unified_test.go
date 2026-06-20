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
	subscriptionsdomain "github.com/ericfisherdev/nestova/internal/subscriptions/domain"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
)

func unifiedLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeUnifiedEvents struct {
	events []*calendardomain.ExternalEvent
}

func (f *fakeUnifiedEvents) ListByHouseholdRange(context.Context, household.HouseholdID, time.Time, time.Time) ([]*calendardomain.ExternalEvent, error) {
	return f.events, nil
}

type fakeUnifiedTasks struct {
	byStatus map[tasksdomain.InstanceStatus][]*tasksdomain.TaskInstance
}

func (f *fakeUnifiedTasks) ListByHousehold(_ context.Context, _ household.HouseholdID, status tasksdomain.InstanceStatus, _, _ time.Time) ([]*tasksdomain.TaskInstance, error) {
	return f.byStatus[status], nil
}

type fakeRecurringTasks struct {
	titles map[tasksdomain.RecurringTaskID]string
	err    error // when set, returned for every Get (a non-NotFound failure)
}

func (f *fakeRecurringTasks) Get(_ context.Context, _ household.HouseholdID, id tasksdomain.RecurringTaskID) (*tasksdomain.RecurringTask, error) {
	if f.err != nil {
		return nil, f.err
	}
	title, ok := f.titles[id]
	if !ok {
		return nil, tasksdomain.ErrTaskNotFound
	}
	return &tasksdomain.RecurringTask{ID: id, Title: title}, nil
}

type fakeUnifiedSubs struct {
	subs []*subscriptionsdomain.Subscription
}

func (f *fakeUnifiedSubs) ListActiveByHousehold(context.Context, household.HouseholdID) ([]*subscriptionsdomain.Subscription, error) {
	return f.subs, nil
}

type fakeMembers struct{ members []*household.Member }

func (f *fakeMembers) ListMembers(context.Context, household.HouseholdID) ([]*household.Member, error) {
	return f.members, nil
}

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func mustUnified(t *testing.T, events *fakeUnifiedEvents, tasks *fakeUnifiedTasks, rec *fakeRecurringTasks, subs *fakeUnifiedSubs, members *fakeMembers) *app.UnifiedCalendarService {
	t.Helper()
	svc, err := app.NewUnifiedCalendarService(events, tasks, rec, subs, members, unifiedLogger())
	if err != nil {
		t.Fatalf("NewUnifiedCalendarService: %v", err)
	}
	return svc
}

func TestListMergesOrdersAndColors(t *testing.T) {
	hh := household.NewHouseholdID()
	from, to := day(2026, 7, 1), day(2026, 7, 31)

	assignee := &household.Member{ID: household.NewMemberID(), Color: household.ColorClay}
	payer := &household.Member{ID: household.NewMemberID(), Color: household.ColorPlum}
	members := &fakeMembers{members: []*household.Member{assignee, payer}}

	events := &fakeUnifiedEvents{events: []*calendardomain.ExternalEvent{{
		ID: calendardomain.NewExternalEventID(), ExternalID: "e1", Title: "Dentist",
		StartsAt: time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC), EndsAt: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC),
	}}}

	recID := tasksdomain.NewRecurringTaskID()
	tasks := &fakeUnifiedTasks{byStatus: map[tasksdomain.InstanceStatus][]*tasksdomain.TaskInstance{
		tasksdomain.StatusPending: {{
			ID: tasksdomain.NewTaskInstanceID(), RecurringTaskID: recID,
			AssigneeID: &assignee.ID, DueOn: day(2026, 7, 5), Status: tasksdomain.StatusPending,
		}},
	}}
	rec := &fakeRecurringTasks{titles: map[tasksdomain.RecurringTaskID]string{recID: "Vacuum"}}

	amount, _ := household.NewMoney(999, "USD")
	subs := &fakeUnifiedSubs{subs: []*subscriptionsdomain.Subscription{{
		ID: subscriptionsdomain.NewSubscriptionID(), Name: "Streaming", Amount: amount,
		Cycle: subscriptionsdomain.CycleMonthly, NextRenewalOn: day(2026, 7, 20),
		PayerID: &payer.ID, Active: true,
	}}}

	items, err := mustUnified(t, events, tasks, rec, subs, members).List(context.Background(), hh, from, to)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	// Ordered by start: task (5th) < event (10th) < renewal (20th).
	if items[0].Kind != app.KindTask || items[1].Kind != app.KindEvent || items[2].Kind != app.KindRenewal {
		t.Fatalf("order = %s,%s,%s, want task,event,renewal", items[0].Kind, items[1].Kind, items[2].Kind)
	}
	if items[0].Title != "Vacuum" || items[0].MemberColor != "clay" {
		t.Fatalf("task item = %+v, want Vacuum/clay", items[0])
	}
	if items[1].MemberColor != "" {
		t.Fatalf("event color = %q, want empty (unattributed)", items[1].MemberColor)
	}
	if items[2].Title != "Streaming" || items[2].MemberColor != "plum" {
		t.Fatalf("renewal item = %+v, want Streaming/plum", items[2])
	}
}

func TestListEmptyRangeReturnsEmptySlice(t *testing.T) {
	items, err := mustUnified(t, &fakeUnifiedEvents{}, &fakeUnifiedTasks{}, &fakeRecurringTasks{}, &fakeUnifiedSubs{}, &fakeMembers{}).
		List(context.Background(), household.NewHouseholdID(), day(2026, 7, 1), day(2026, 7, 31))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if items == nil || len(items) != 0 {
		t.Fatalf("List empty = %v, want non-nil empty slice", items)
	}
}

func TestListExcludesCustomSubscription(t *testing.T) {
	amount, _ := household.NewMoney(500, "USD")
	subs := &fakeUnifiedSubs{subs: []*subscriptionsdomain.Subscription{{
		ID: subscriptionsdomain.NewSubscriptionID(), Name: "Odd", Amount: amount,
		Cycle: subscriptionsdomain.CycleCustom, NextRenewalOn: day(2026, 7, 10), Active: true,
	}}}
	items, err := mustUnified(t, &fakeUnifiedEvents{}, &fakeUnifiedTasks{}, &fakeRecurringTasks{}, subs, &fakeMembers{}).
		List(context.Background(), household.NewHouseholdID(), day(2026, 7, 1), day(2026, 7, 31))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("custom subscription produced %d items, want 0", len(items))
	}
}

func TestListProjectsMultipleRenewals(t *testing.T) {
	amount, _ := household.NewMoney(700, "USD")
	subs := &fakeUnifiedSubs{subs: []*subscriptionsdomain.Subscription{{
		ID: subscriptionsdomain.NewSubscriptionID(), Name: "Weekly", Amount: amount,
		Cycle: subscriptionsdomain.CycleWeekly, NextRenewalOn: day(2026, 7, 2), Active: true,
	}}}
	// July 2..31, weekly -> 2, 9, 16, 23, 30 = 5 occurrences.
	items, err := mustUnified(t, &fakeUnifiedEvents{}, &fakeUnifiedTasks{}, &fakeRecurringTasks{}, subs, &fakeMembers{}).
		List(context.Background(), household.NewHouseholdID(), day(2026, 7, 1), day(2026, 7, 31))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("weekly projection produced %d items, want 5", len(items))
	}
	if !items[0].Start.Equal(day(2026, 7, 2)) || !items[4].Start.Equal(day(2026, 7, 30)) {
		t.Fatalf("renewal range = %s..%s, want 2026-07-02..2026-07-30", items[0].Start, items[4].Start)
	}
}

func TestListRenewalRangeFiltering(t *testing.T) {
	amount, _ := household.NewMoney(700, "USD")
	// Renewal before the range should be excluded; the next monthly occurrence
	// lands inside it.
	subs := &fakeUnifiedSubs{subs: []*subscriptionsdomain.Subscription{{
		ID: subscriptionsdomain.NewSubscriptionID(), Name: "Monthly", Amount: amount,
		Cycle: subscriptionsdomain.CycleMonthly, NextRenewalOn: day(2026, 6, 15), Active: true,
	}}}
	items, err := mustUnified(t, &fakeUnifiedEvents{}, &fakeUnifiedTasks{}, &fakeRecurringTasks{}, subs, &fakeMembers{}).
		List(context.Background(), household.NewHouseholdID(), day(2026, 7, 1), day(2026, 7, 31))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || !items[0].Start.Equal(day(2026, 7, 15)) {
		t.Fatalf("range-filtered renewals = %+v, want one on 2026-07-15", items)
	}
}

func TestNewUnifiedCalendarServiceValidatesDeps(t *testing.T) {
	e, ta, r, su, m, l := &fakeUnifiedEvents{}, &fakeUnifiedTasks{}, &fakeRecurringTasks{}, &fakeUnifiedSubs{}, &fakeMembers{}, unifiedLogger()
	cases := []func() (*app.UnifiedCalendarService, error){
		func() (*app.UnifiedCalendarService, error) {
			return app.NewUnifiedCalendarService(nil, ta, r, su, m, l)
		},
		func() (*app.UnifiedCalendarService, error) { return app.NewUnifiedCalendarService(e, nil, r, su, m, l) },
		func() (*app.UnifiedCalendarService, error) {
			return app.NewUnifiedCalendarService(e, ta, nil, su, m, l)
		},
		func() (*app.UnifiedCalendarService, error) { return app.NewUnifiedCalendarService(e, ta, r, nil, m, l) },
		func() (*app.UnifiedCalendarService, error) {
			return app.NewUnifiedCalendarService(e, ta, r, su, nil, l)
		},
		func() (*app.UnifiedCalendarService, error) {
			return app.NewUnifiedCalendarService(e, ta, r, su, m, nil)
		},
	}
	for i, fn := range cases {
		if _, err := fn(); err == nil {
			t.Errorf("case %d: expected an error, got nil", i)
		}
	}
}

func TestListSkipsTaskWithMissingTemplate(t *testing.T) {
	// A task instance whose recurring-task template is missing is skipped (the
	// title cannot be resolved) rather than failing the whole view.
	orphan := tasksdomain.NewRecurringTaskID()
	tasks := &fakeUnifiedTasks{byStatus: map[tasksdomain.InstanceStatus][]*tasksdomain.TaskInstance{
		tasksdomain.StatusPending: {{
			ID: tasksdomain.NewTaskInstanceID(), RecurringTaskID: orphan,
			DueOn: day(2026, 7, 5), Status: tasksdomain.StatusPending,
		}},
	}}
	rec := &fakeRecurringTasks{titles: map[tasksdomain.RecurringTaskID]string{}} // empty -> not found
	items, err := mustUnified(t, &fakeUnifiedEvents{}, tasks, rec, &fakeUnifiedSubs{}, &fakeMembers{}).
		List(context.Background(), household.NewHouseholdID(), day(2026, 7, 1), day(2026, 7, 31))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("orphaned task produced %d items, want 0 (skipped)", len(items))
	}
}

func TestListPropagatesTitleResolutionError(t *testing.T) {
	tasks := &fakeUnifiedTasks{byStatus: map[tasksdomain.InstanceStatus][]*tasksdomain.TaskInstance{
		tasksdomain.StatusPending: {{
			ID: tasksdomain.NewTaskInstanceID(), RecurringTaskID: tasksdomain.NewRecurringTaskID(),
			DueOn: day(2026, 7, 5), Status: tasksdomain.StatusPending,
		}},
	}}
	rec := &fakeRecurringTasks{err: errors.New("database is down")} // not ErrTaskNotFound
	_, err := mustUnified(t, &fakeUnifiedEvents{}, tasks, rec, &fakeUnifiedSubs{}, &fakeMembers{}).
		List(context.Background(), household.NewHouseholdID(), day(2026, 7, 1), day(2026, 7, 31))
	if err == nil {
		t.Fatal("List error = nil, want a non-NotFound title-resolution error propagated")
	}
}

func TestListInvertedRangeReturnsEmpty(t *testing.T) {
	// to before from: deterministic empty result without touching repositories.
	items, err := mustUnified(t, &fakeUnifiedEvents{}, &fakeUnifiedTasks{}, &fakeRecurringTasks{}, &fakeUnifiedSubs{}, &fakeMembers{}).
		List(context.Background(), household.NewHouseholdID(), day(2026, 7, 31), day(2026, 7, 1))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if items == nil || len(items) != 0 {
		t.Fatalf("inverted range = %v, want non-nil empty slice", items)
	}
}

func TestListSortsByTitleWhenStartsEqual(t *testing.T) {
	sameDay := day(2026, 7, 10)
	recZ := tasksdomain.NewRecurringTaskID()
	recA := tasksdomain.NewRecurringTaskID()
	tasks := &fakeUnifiedTasks{byStatus: map[tasksdomain.InstanceStatus][]*tasksdomain.TaskInstance{
		tasksdomain.StatusPending: {
			{ID: tasksdomain.NewTaskInstanceID(), RecurringTaskID: recZ, DueOn: sameDay, Status: tasksdomain.StatusPending},
			{ID: tasksdomain.NewTaskInstanceID(), RecurringTaskID: recA, DueOn: sameDay, Status: tasksdomain.StatusPending},
		},
	}}
	rec := &fakeRecurringTasks{titles: map[tasksdomain.RecurringTaskID]string{recZ: "Zebra", recA: "Apple"}}
	items, err := mustUnified(t, &fakeUnifiedEvents{}, tasks, rec, &fakeUnifiedSubs{}, &fakeMembers{}).
		List(context.Background(), household.NewHouseholdID(), day(2026, 7, 1), day(2026, 7, 31))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Title != "Apple" || items[1].Title != "Zebra" {
		t.Fatalf("titles for same start = [%s, %s], want [Apple, Zebra]", items[0].Title, items[1].Title)
	}
}
