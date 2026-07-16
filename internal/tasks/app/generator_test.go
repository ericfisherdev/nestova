package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// Fake repositories (hermetic, no DB)
// ---------------------------------------------------------------------------

// fakeRecurringTaskRepo is an in-memory implementation of
// domain.RecurringTaskRepository for use in hermetic tests. Only the methods
// exercised by the generator and service are implemented; unimplemented methods
// panic so any accidental call is immediately obvious.
type fakeRecurringTaskRepo struct {
	mu      sync.Mutex
	tasks   []*domain.RecurringTask
	members map[domain.RecurringTaskID][]household.MemberID
}

func newFakeRecurringTaskRepo() *fakeRecurringTaskRepo {
	return &fakeRecurringTaskRepo{
		members: make(map[domain.RecurringTaskID][]household.MemberID),
	}
}

func (r *fakeRecurringTaskRepo) Create(_ context.Context, rt *domain.RecurringTask) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks = append(r.tasks, rt)
	return nil
}

func (r *fakeRecurringTaskRepo) CreateWithRotation(_ context.Context, task *domain.RecurringTask, pool []household.MemberID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks = append(r.tasks, task)
	r.members[task.ID] = append([]household.MemberID(nil), pool...)
	return nil
}

func (r *fakeRecurringTaskRepo) Get(_ context.Context, householdID household.HouseholdID, id domain.RecurringTaskID) (*domain.RecurringTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		if t.ID == id && t.HouseholdID == householdID {
			return t, nil
		}
	}
	return nil, domain.ErrTaskNotFound
}

func (r *fakeRecurringTaskRepo) ListActive(_ context.Context, householdID household.HouseholdID) ([]*domain.RecurringTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.RecurringTask
	for _, t := range r.tasks {
		if t.HouseholdID == householdID && t.Active {
			out = append(out, t)
		}
	}
	return out, nil
}

func (r *fakeRecurringTaskRepo) ListAllActive(_ context.Context) ([]*domain.RecurringTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.RecurringTask
	for _, t := range r.tasks {
		if t.Active {
			out = append(out, t)
		}
	}
	return out, nil
}

func (r *fakeRecurringTaskRepo) SetRotationMembers(_ context.Context, householdID household.HouseholdID, id domain.RecurringTaskID, members []household.MemberID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		if t.ID == id && t.HouseholdID == householdID {
			r.members[id] = append([]household.MemberID(nil), members...)
			return nil
		}
	}
	return domain.ErrTaskNotFound
}

func (r *fakeRecurringTaskRepo) RotationMembers(_ context.Context, householdID household.HouseholdID, id domain.RecurringTaskID) ([]household.MemberID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		if t.ID == id && t.HouseholdID == householdID {
			return append([]household.MemberID(nil), r.members[id]...), nil
		}
	}
	return nil, domain.ErrTaskNotFound
}

// instanceKey uniquely identifies a task instance for duplicate detection,
// matching the task_instance_task_due_uniq constraint. For a scheduled
// instance, (taskID, dueOn) alone determines duplication. A standing instance
// has no due date; standingID discriminates each one so it never collides
// with another standing instance for the same task, mirroring Postgres (which
// treats every NULL due_on as distinct).
type instanceKey struct {
	taskID     domain.RecurringTaskID
	dueOn      time.Time
	standingID domain.TaskInstanceID
}

// fakeTaskInstanceRepo is an in-memory implementation of
// domain.TaskInstanceRepository for use in hermetic tests.
type fakeTaskInstanceRepo struct {
	mu        sync.Mutex
	instances []*domain.TaskInstance
	seen      map[instanceKey]bool
}

func newFakeTaskInstanceRepo() *fakeTaskInstanceRepo {
	return &fakeTaskInstanceRepo{
		seen: make(map[instanceKey]bool),
	}
}

func (r *fakeTaskInstanceRepo) Insert(_ context.Context, inst *domain.TaskInstance) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	kind := inst.Kind
	if kind == "" {
		kind = domain.KindScheduled
	}

	var key instanceKey
	var dueOn *time.Time
	if kind == domain.KindStanding {
		key = instanceKey{taskID: inst.RecurringTaskID, standingID: inst.ID}
	} else {
		d := domain.DateOf(*inst.DueOn)
		dueOn = &d
		key = instanceKey{taskID: inst.RecurringTaskID, dueOn: d}
	}
	if r.seen[key] {
		return domain.ErrDuplicateInstance
	}
	r.seen[key] = true
	snapshot := *inst
	snapshot.DueOn = dueOn
	snapshot.Kind = kind
	r.instances = append(r.instances, &snapshot)
	return nil
}

func (r *fakeTaskInstanceRepo) Get(_ context.Context, householdID household.HouseholdID, id domain.TaskInstanceID) (*domain.TaskInstance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, inst := range r.instances {
		if inst.ID == id && inst.HouseholdID == householdID {
			return inst, nil
		}
	}
	return nil, domain.ErrInstanceNotFound
}

// ListByHousehold mirrors the real adapter's kind='scheduled' filter: a
// standing instance (nil DueOn) never matches a due-date range regardless of
// the from/to bounds.
func (r *fakeTaskInstanceRepo) ListByHousehold(_ context.Context, householdID household.HouseholdID, status domain.InstanceStatus, from, to time.Time) ([]*domain.TaskInstance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fromDate := domain.DateOf(from)
	toDate := domain.DateOf(to)
	var out []*domain.TaskInstance
	for _, inst := range r.instances {
		if inst.HouseholdID == householdID &&
			inst.Status == status &&
			inst.Kind == domain.KindScheduled &&
			inst.DueOn != nil &&
			!inst.DueOn.Before(fromDate) &&
			!inst.DueOn.After(toDate) {
			out = append(out, inst)
		}
	}
	return out, nil
}

// ListStanding is the in-memory implementation of the NES-116 "Anytime
// section" query: every pending kind='standing' instance for the household.
func (r *fakeTaskInstanceRepo) ListStanding(_ context.Context, householdID household.HouseholdID) ([]*domain.TaskInstance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.TaskInstance
	for _, inst := range r.instances {
		if inst.HouseholdID == householdID &&
			inst.Status == domain.StatusPending &&
			inst.Kind == domain.KindStanding {
			out = append(out, inst)
		}
	}
	return out, nil
}

func (r *fakeTaskInstanceRepo) LatestDueOn(_ context.Context, householdID household.HouseholdID, id domain.RecurringTaskID) (time.Time, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var latest time.Time
	found := false
	for _, inst := range r.instances {
		if inst.HouseholdID == householdID && inst.RecurringTaskID == id && inst.DueOn != nil {
			if !found || inst.DueOn.After(latest) {
				latest = *inst.DueOn
				found = true
			}
		}
	}
	return latest, found, nil
}

// Claim mirrors the real adapter's NES-117 semantics: a previously-unassigned
// instance may be claimed by anyone (sets ClaimedBy/ClaimedAt/ClaimExpiresAt,
// the latter domain.ClaimWindow out); an instance already assigned to
// assignee may be self-claimed again (ClaimedBy/ClaimedAt set, no expiry); an
// instance assigned to a DIFFERENT member is rejected.
func (r *fakeTaskInstanceRepo) Claim(_ context.Context, householdID household.HouseholdID, id domain.TaskInstanceID, assignee household.MemberID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, inst := range r.instances {
		if inst.ID == id && inst.HouseholdID == householdID {
			if inst.Status != domain.StatusPending {
				return domain.ErrInstanceInTerminalState
			}
			wasUnassigned := inst.AssigneeID == nil
			if !wasUnassigned && *inst.AssigneeID != assignee {
				return domain.ErrInstanceAlreadyClaimed
			}
			now := time.Now()
			inst.AssigneeID = &assignee
			inst.ClaimedBy = &assignee
			inst.ClaimedAt = &now
			if wasUnassigned {
				expires := now.Add(domain.ClaimWindow)
				inst.ClaimExpiresAt = &expires
			} else {
				inst.ClaimExpiresAt = nil
			}
			return nil
		}
	}
	return domain.ErrInstanceNotFound
}

// respawnStandingLocked appends a fresh pending standing instance for the same
// recurring task when inst is a standing instance, mirroring the real
// adapter's same-transaction respawn (insertStandingInstance /
// respawnIfStanding) on every terminal transition — Complete, CompleteAndAward,
// and Skip alike (NES-116). Callers must already hold r.mu.
func (r *fakeTaskInstanceRepo) respawnStandingLocked(inst *domain.TaskInstance) {
	if inst.Kind != domain.KindStanding {
		return
	}
	r.instances = append(r.instances, &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: inst.RecurringTaskID,
		HouseholdID:     inst.HouseholdID,
		Status:          domain.StatusPending,
		Kind:            domain.KindStanding,
	})
}

// Complete does not award points (no ledger exists in this fake); it does not
// accept overdue instances either — see CompleteAndAward for both differences.
// NES-116: like every terminal transition, completing a standing instance
// respawns its replacement.
func (r *fakeTaskInstanceRepo) Complete(_ context.Context, householdID household.HouseholdID, id domain.TaskInstanceID, by household.MemberID, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, inst := range r.instances {
		if inst.ID == id && inst.HouseholdID == householdID {
			if inst.Status != domain.StatusPending {
				return domain.ErrInstanceInTerminalState
			}
			inst.Status = domain.StatusDone
			inst.CompletedBy = &by
			inst.CompletedAt = &at
			r.respawnStandingLocked(inst)
			return nil
		}
	}
	return domain.ErrInstanceNotFound
}

// CompleteAndAward is the in-memory implementation of the atomic complete+award
// path. Like the real adapter, it accepts BOTH pending and overdue instances
// (unlike Complete in this fake, which only transitions pending), marks the
// instance done, and silently no-ops the point award since there is no ledger
// in memory. Tests that need to verify award behaviour use the gated Postgres
// tests instead.
//
// NES-116: when the completed instance is a standing instance, a fresh
// pending standing instance for the same recurring task is appended, mirroring
// the real adapter's same-transaction respawn so hermetic tests can verify
// "always exactly one open standing instance" without a database.
func (r *fakeTaskInstanceRepo) CompleteAndAward(_ context.Context, householdID household.HouseholdID, id domain.TaskInstanceID, by household.MemberID, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, inst := range r.instances {
		if inst.ID == id && inst.HouseholdID == householdID {
			if inst.Status != domain.StatusPending && inst.Status != domain.StatusOverdue {
				return domain.ErrInstanceInTerminalState
			}
			inst.Status = domain.StatusDone
			inst.CompletedBy = &by
			inst.CompletedAt = &at
			r.respawnStandingLocked(inst)
			return nil
		}
	}
	return domain.ErrInstanceNotFound
}

// Skip transitions a pending instance to skipped. NES-116: skipping a
// standing instance respawns its replacement, exactly like Complete and
// CompleteAndAward — the "always exactly one open standing instance"
// invariant does not depend on which terminal transition ended it.
func (r *fakeTaskInstanceRepo) Skip(_ context.Context, householdID household.HouseholdID, id domain.TaskInstanceID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, inst := range r.instances {
		if inst.ID == id && inst.HouseholdID == householdID {
			if inst.Status != domain.StatusPending {
				return domain.ErrInstanceInTerminalState
			}
			inst.Status = domain.StatusSkipped
			r.respawnStandingLocked(inst)
			return nil
		}
	}
	return domain.ErrInstanceNotFound
}

// MarkPendingOverdue mirrors the real adapter's kind='scheduled' filter: a
// standing instance (nil DueOn) can never become overdue.
func (r *fakeTaskInstanceRepo) MarkPendingOverdue(_ context.Context, householdID household.HouseholdID, asOf time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	asOfDate := domain.DateOf(asOf)
	count := 0
	for _, inst := range r.instances {
		if inst.HouseholdID == householdID &&
			inst.Status == domain.StatusPending &&
			inst.Kind == domain.KindScheduled &&
			inst.DueOn != nil &&
			inst.DueOn.Before(asOfDate) {
			inst.Status = domain.StatusOverdue
			count++
		}
	}
	return count, nil
}

// MarkPendingOverdueAll mirrors the real adapter's kind='scheduled' filter: a
// standing instance (nil DueOn) can never become overdue.
func (r *fakeTaskInstanceRepo) MarkPendingOverdueAll(_ context.Context, asOf time.Time) ([]domain.ReminderTarget, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	asOfDate := domain.DateOf(asOf)
	var targets []domain.ReminderTarget
	for _, inst := range r.instances {
		if inst.Status == domain.StatusPending &&
			inst.Kind == domain.KindScheduled &&
			inst.DueOn != nil &&
			inst.DueOn.Before(asOfDate) {
			inst.Status = domain.StatusOverdue
			targets = append(targets, domain.ReminderTarget{
				InstanceID:  inst.ID,
				HouseholdID: inst.HouseholdID,
				AssigneeID:  inst.AssigneeID,
				DueOn:       *inst.DueOn,
				Kind:        domain.ReminderOverdue,
			})
		}
	}
	return targets, nil
}

func (r *fakeTaskInstanceRepo) ClaimDueSoonReminders(_ context.Context, _ time.Time) ([]domain.ReminderTarget, error) {
	return nil, nil
}

func (r *fakeTaskInstanceRepo) ClearDueSoonReminder(_ context.Context, _ domain.TaskInstanceID) error {
	return nil
}

func (r *fakeTaskInstanceRepo) CompletionDays(_ context.Context, _ household.HouseholdID, _ household.MemberID, _ time.Time) ([]time.Time, error) {
	return nil, nil
}

// SweepExpiredClaims mirrors the real adapter: it reverts every instance
// whose ClaimExpiresAt is at or before asOf (assignee_id/claimed_by/
// claimed_at/claim_expires_at all cleared) and returns one domain.ExpiredClaim
// per reverted instance, with a fixed penalty of 1 point since this fake has
// no recurring_task points to look up.
func (r *fakeTaskInstanceRepo) SweepExpiredClaims(_ context.Context, asOf time.Time) ([]domain.ExpiredClaim, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var claims []domain.ExpiredClaim
	for _, inst := range r.instances {
		if inst.ClaimExpiresAt == nil || inst.ClaimExpiresAt.After(asOf) {
			continue
		}
		if inst.Status != domain.StatusPending && inst.Status != domain.StatusOverdue {
			continue
		}
		claimedBy := *inst.ClaimedBy
		claims = append(claims, domain.ExpiredClaim{
			InstanceID:      inst.ID,
			HouseholdID:     inst.HouseholdID,
			RecurringTaskID: inst.RecurringTaskID,
			ClaimedBy:       claimedBy,
			Title:           "fake task",
			PenaltyPoints:   1,
		})
		inst.AssigneeID = nil
		inst.ClaimedBy = nil
		inst.ClaimedAt = nil
		inst.ClaimExpiresAt = nil
	}
	return claims, nil
}

// Compile-time assertion.
var _ domain.TaskInstanceRepository = (*fakeTaskInstanceRepo)(nil)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// discardLogger returns a no-op logger suitable for tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// weeklyAnchor is a fixed anchor date used across tests.
var weeklyAnchor = time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC)

// newWeeklyTask returns a minimal recurring task with a weekly cadence and the
// given rotation policy, anchored to weeklyAnchor.
func newWeeklyTask(policy domain.RotationPolicy) *domain.RecurringTask {
	return &domain.RecurringTask{
		ID:          domain.NewRecurringTaskID(),
		HouseholdID: household.NewHouseholdID(),
		Title:       "Test task",
		Category:    domain.ChoreCategory,
		Cadence: household.Cadence{
			Freq:     household.FreqWeekly,
			Interval: 1,
			Anchor:   weeklyAnchor,
		},
		RotationPolicy: policy,
		Points:         5,
		Active:         true,
	}
}

// newGenerator is a test helper that constructs a Generator with the supplied
// repos and a 14-day horizon.
func newGenerator(
	t *testing.T,
	taskRepo domain.RecurringTaskRepository,
	instanceRepo domain.TaskInstanceRepository,
) *app.Generator {
	t.Helper()
	g, err := app.NewGenerator(taskRepo, instanceRepo, discardLogger(), 14*24*time.Hour)
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	return g
}

// ---------------------------------------------------------------------------
// assigneeFor tests (via package-internal export in export_test.go)
// ---------------------------------------------------------------------------

// TestAssigneeFor_Fixed verifies that RotationFixed always returns pool[0].
func TestAssigneeFor_Fixed(t *testing.T) {
	m0 := household.NewMemberID()
	m1 := household.NewMemberID()
	pool := []household.MemberID{m0, m1}

	for ordinal := 0; ordinal < 5; ordinal++ {
		got := app.AssigneeFor(domain.RotationFixed, pool, ordinal)
		if got == nil {
			t.Fatalf("ordinal %d: got nil, want %v", ordinal, m0)
		}
		if *got != m0 {
			t.Errorf("ordinal %d: got %v, want %v", ordinal, *got, m0)
		}
	}
}

// TestAssigneeFor_RoundRobin verifies that RotationRoundRobin cycles through
// pool members by ordinal, giving the same result for the same ordinal.
func TestAssigneeFor_RoundRobin(t *testing.T) {
	m0 := household.NewMemberID()
	m1 := household.NewMemberID()
	m2 := household.NewMemberID()
	pool := []household.MemberID{m0, m1, m2}

	cases := []struct {
		ordinal int
		want    household.MemberID
	}{
		{0, m0},
		{1, m1},
		{2, m2},
		{3, m0}, // wraps
		{4, m1},
		{5, m2},
	}
	for _, tc := range cases {
		got := app.AssigneeFor(domain.RotationRoundRobin, pool, tc.ordinal)
		if got == nil {
			t.Fatalf("ordinal %d: got nil, want %v", tc.ordinal, tc.want)
		}
		if *got != tc.want {
			t.Errorf("ordinal %d: got %v, want %v", tc.ordinal, *got, tc.want)
		}
	}
}

// TestAssigneeFor_Claimable verifies that RotationClaimable always returns nil.
func TestAssigneeFor_Claimable(t *testing.T) {
	pool := []household.MemberID{household.NewMemberID()}
	for ordinal := 0; ordinal < 5; ordinal++ {
		got := app.AssigneeFor(domain.RotationClaimable, pool, ordinal)
		if got != nil {
			t.Errorf("ordinal %d: got %v, want nil", ordinal, *got)
		}
	}
}

// ---------------------------------------------------------------------------
// Generator idempotency tests
// ---------------------------------------------------------------------------

// TestGenerator_Idempotency verifies that running GenerateDue twice for the
// same asOf inserts no duplicates on the second run and produces identical
// instance assignments.
func TestGenerator_Idempotency(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()

	task := newWeeklyTask(domain.RotationRoundRobin)
	m0 := household.NewMemberID()
	m1 := household.NewMemberID()

	// Seed the task and its pool.
	if err := taskRepo.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := taskRepo.SetRotationMembers(context.Background(), task.HouseholdID, task.ID, []household.MemberID{m0, m1}); err != nil {
		t.Fatalf("SetRotationMembers: %v", err)
	}

	g := newGenerator(t, taskRepo, instanceRepo)

	// asOf = anchor + 3 weeks; horizon = 14 days, so the window is
	// (anchor-1ns, DateOf(anchor+21d+14d)] = (anchor-1ns, anchor+35d]. The weekly
	// occurrences in that window are anchor+0w,1w,2w,3w,4w,5w = 6 occurrences.
	asOf := weeklyAnchor.AddDate(0, 0, 21)

	// First run.
	count1, err := g.GenerateDue(context.Background(), asOf)
	if err != nil {
		t.Fatalf("GenerateDue (run 1): %v", err)
	}
	if count1 == 0 {
		t.Fatal("GenerateDue (run 1) inserted 0 instances, want > 0")
	}

	// Capture the first-run assignments.
	snapshot1 := make([]struct {
		dueOn      time.Time
		assigneeID *household.MemberID
	}, len(instanceRepo.instances))
	for i, inst := range instanceRepo.instances {
		snapshot1[i].dueOn = *inst.DueOn
		snapshot1[i].assigneeID = inst.AssigneeID
	}

	// Second run with the same asOf must insert zero new rows.
	count2, err := g.GenerateDue(context.Background(), asOf)
	if err != nil {
		t.Fatalf("GenerateDue (run 2): %v", err)
	}
	if count2 != 0 {
		t.Errorf("GenerateDue (run 2) inserted %d instances, want 0", count2)
	}

	// Assignments must be identical on both runs (ordinal-based, not counter-based).
	if len(instanceRepo.instances) != len(snapshot1) {
		t.Fatalf("instance count changed between runs: %d → %d",
			len(snapshot1), len(instanceRepo.instances))
	}
	for i, inst := range instanceRepo.instances {
		s := snapshot1[i]
		if !inst.DueOn.Equal(s.dueOn) {
			t.Errorf("instance %d: DueOn changed between runs: %v → %v", i, s.dueOn, inst.DueOn)
		}
		switch {
		case s.assigneeID == nil && inst.AssigneeID == nil:
			// both nil — OK
		case s.assigneeID == nil || inst.AssigneeID == nil:
			t.Errorf("instance %d (due %s): assignee nilness changed between runs", i, inst.DueOn.Format(time.DateOnly))
		case *s.assigneeID != *inst.AssigneeID:
			t.Errorf("instance %d (due %s): assignee changed between runs: %v → %v",
				i, inst.DueOn.Format(time.DateOnly), *s.assigneeID, *inst.AssigneeID)
		}
	}
}

// TestGenerator_RoundRobinStable verifies that round-robin assignment is stable
// across two independent GenerateDue runs covering overlapping windows and that
// members are distributed in pool order.
func TestGenerator_RoundRobinStable(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()

	task := newWeeklyTask(domain.RotationRoundRobin)
	m0 := household.NewMemberID()
	m1 := household.NewMemberID()

	if err := taskRepo.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := taskRepo.SetRotationMembers(context.Background(), task.HouseholdID, task.ID, []household.MemberID{m0, m1}); err != nil {
		t.Fatalf("SetRotationMembers: %v", err)
	}

	g := newGenerator(t, taskRepo, instanceRepo)

	// asOf = anchor + 4 weeks; horizon = 14 days, so the window is
	// (anchor-1ns, DateOf(anchor+28d+14d)] = (anchor-1ns, anchor+42d]. The weekly
	// occurrences are anchor+0w..6w = 7 occurrences. The exact count is not
	// asserted here; this test only verifies round-robin cycling in pool order.
	asOf := weeklyAnchor.AddDate(0, 0, 28)
	count, err := g.GenerateDue(context.Background(), asOf)
	if err != nil {
		t.Fatalf("GenerateDue: %v", err)
	}
	if count == 0 {
		t.Fatal("expected at least one inserted instance")
	}

	// Verify cycling in pool order: ordinal 0→m0, 1→m1, 2→m0, 3→m1, 4→m0 …
	for i, inst := range instanceRepo.instances {
		if inst.RecurringTaskID != task.ID {
			continue
		}
		expectedMember := []household.MemberID{m0, m1}[i%2]
		if inst.AssigneeID == nil {
			t.Errorf("instance %d (due %s): assignee is nil, want %v", i, inst.DueOn.Format(time.DateOnly), expectedMember)
			continue
		}
		if *inst.AssigneeID != expectedMember {
			t.Errorf("instance %d (due %s): assignee = %v, want %v",
				i, inst.DueOn.Format(time.DateOnly), *inst.AssigneeID, expectedMember)
		}
	}

	// Snapshot assignments, then re-run with the same asOf: the overlapping run
	// must insert nothing and must leave every existing assignment unchanged.
	snapshot := func() map[string]household.MemberID {
		m := make(map[string]household.MemberID)
		for _, inst := range instanceRepo.instances {
			if inst.RecurringTaskID == task.ID && inst.AssigneeID != nil {
				m[inst.DueOn.Format(time.DateOnly)] = *inst.AssigneeID
			}
		}
		return m
	}
	before := snapshot()

	count2, err := g.GenerateDue(context.Background(), asOf)
	if err != nil {
		t.Fatalf("GenerateDue (second run): %v", err)
	}
	if count2 != 0 {
		t.Errorf("second GenerateDue inserted %d instances, want 0 (idempotent)", count2)
	}

	after := snapshot()
	if len(after) != len(before) {
		t.Fatalf("instance count changed across runs: %d -> %d", len(before), len(after))
	}
	for due, member := range before {
		if after[due] != member {
			t.Errorf("assignee for %s changed across runs: %v -> %v", due, member, after[due])
		}
	}
}

// TestGenerator_FixedEmptyPool verifies that a fixed-rotation task with an
// empty rotation pool is skipped with ErrNoRotationMembers and no instances
// are inserted.
func TestGenerator_FixedEmptyPool(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()

	task := newWeeklyTask(domain.RotationFixed)

	if err := taskRepo.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Deliberately do NOT set rotation members — pool remains empty.

	g := newGenerator(t, taskRepo, instanceRepo)

	asOf := weeklyAnchor.AddDate(0, 0, 14)
	count, err := g.GenerateDue(context.Background(), asOf)

	// The task must be skipped — ErrNoRotationMembers is the first non-duplicate
	// error, returned as firstErr.
	if !errors.Is(err, domain.ErrNoRotationMembers) {
		t.Errorf("GenerateDue empty pool = %v, want ErrNoRotationMembers", err)
	}
	if count != 0 {
		t.Errorf("GenerateDue empty pool inserted %d instances, want 0", count)
	}
	if len(instanceRepo.instances) != 0 {
		t.Errorf("instanceRepo has %d instances, want 0", len(instanceRepo.instances))
	}
}

// TestGenerator_ClaimableNoPool verifies that a claimable task inserts
// instances with nil assignees even without a rotation pool.
func TestGenerator_ClaimableNoPool(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()

	task := newWeeklyTask(domain.RotationClaimable)

	if err := taskRepo.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// No rotation members set — expected for claimable.

	g := newGenerator(t, taskRepo, instanceRepo)

	asOf := weeklyAnchor.AddDate(0, 0, 14)
	count, err := g.GenerateDue(context.Background(), asOf)
	if err != nil {
		t.Fatalf("GenerateDue claimable: %v", err)
	}
	if count == 0 {
		t.Fatal("expected at least one instance for claimable task")
	}

	for _, inst := range instanceRepo.instances {
		if inst.AssigneeID != nil {
			t.Errorf("instance due %s: assignee = %v, want nil for claimable task",
				inst.DueOn.Format(time.DateOnly), *inst.AssigneeID)
		}
	}
}

// ---------------------------------------------------------------------------
// TaskService tests
// ---------------------------------------------------------------------------

// TestTaskService_CreateRecurringTask_InvalidCadence verifies that an invalid
// cadence is rejected before any repository calls.
func TestTaskService_CreateRecurringTask_InvalidCadence(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instanceRepo)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	task := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    household.NewHouseholdID(),
		Title:          "Bad cadence task",
		Category:       domain.ChoreCategory,
		RotationPolicy: domain.RotationClaimable,
		Active:         true,
		// Cadence is zero-value — Interval < 1, fails Validate.
	}

	err = svc.CreateRecurringTask(context.Background(), task, nil)
	if err == nil {
		t.Fatal("CreateRecurringTask(invalid cadence) error = nil, want non-nil")
	}
	// Confirm it wraps the household cadence sentinel.
	if !errors.Is(err, household.ErrInvalidCadence) {
		t.Errorf("error = %v, want to wrap ErrInvalidCadence", err)
	}
	if len(taskRepo.tasks) != 0 {
		t.Errorf("repo has %d tasks after invalid cadence, want 0", len(taskRepo.tasks))
	}
}

// TestTaskService_CreateRecurringTask_FixedEmptyPool verifies that creating a
// fixed-rotation task without a pool returns ErrNoRotationMembers.
func TestTaskService_CreateRecurringTask_FixedEmptyPool(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instanceRepo)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	task := &domain.RecurringTask{
		ID:          domain.NewRecurringTaskID(),
		HouseholdID: household.NewHouseholdID(),
		Title:       "Fixed no pool",
		Category:    domain.ChoreCategory,
		Cadence: household.Cadence{
			Freq:     household.FreqWeekly,
			Interval: 1,
			Anchor:   weeklyAnchor,
		},
		RotationPolicy: domain.RotationFixed,
		Active:         true,
	}

	err = svc.CreateRecurringTask(context.Background(), task, nil)
	if !errors.Is(err, domain.ErrNoRotationMembers) {
		t.Errorf("CreateRecurringTask(fixed, no pool) = %v, want ErrNoRotationMembers", err)
	}
	if len(taskRepo.tasks) != 0 {
		t.Errorf("repo has %d tasks after empty pool rejection, want 0", len(taskRepo.tasks))
	}
}

// TestTaskService_CreateRecurringTask_RoundRobinEmptyPool mirrors the fixed-pool
// check for round_robin policy.
func TestTaskService_CreateRecurringTask_RoundRobinEmptyPool(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instanceRepo)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	task := &domain.RecurringTask{
		ID:          domain.NewRecurringTaskID(),
		HouseholdID: household.NewHouseholdID(),
		Title:       "RR no pool",
		Category:    domain.ChoreCategory,
		Cadence: household.Cadence{
			Freq:     household.FreqWeekly,
			Interval: 1,
			Anchor:   weeklyAnchor,
		},
		RotationPolicy: domain.RotationRoundRobin,
		Active:         true,
	}

	err = svc.CreateRecurringTask(context.Background(), task, []household.MemberID{})
	if !errors.Is(err, domain.ErrNoRotationMembers) {
		t.Errorf("CreateRecurringTask(round_robin, empty pool) = %v, want ErrNoRotationMembers", err)
	}
}

// TestTaskService_CreateRecurringTask_Success verifies the happy path: a valid
// task with a pool is persisted along with its rotation members.
func TestTaskService_CreateRecurringTask_Success(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instanceRepo)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	hid := household.NewHouseholdID()
	m0 := household.NewMemberID()
	m1 := household.NewMemberID()

	task := &domain.RecurringTask{
		ID:          domain.NewRecurringTaskID(),
		HouseholdID: hid,
		Title:       "Dishes",
		Category:    domain.ChoreCategory,
		Cadence: household.Cadence{
			Freq:     household.FreqWeekly,
			Interval: 1,
			Anchor:   weeklyAnchor,
		},
		RotationPolicy: domain.RotationRoundRobin,
		Points:         10,
		Active:         true,
	}

	pool := []household.MemberID{m0, m1}
	if err := svc.CreateRecurringTask(context.Background(), task, pool); err != nil {
		t.Fatalf("CreateRecurringTask: %v", err)
	}

	if len(taskRepo.tasks) != 1 {
		t.Fatalf("repo has %d tasks, want 1", len(taskRepo.tasks))
	}

	members, err := taskRepo.RotationMembers(context.Background(), hid, task.ID)
	if err != nil {
		t.Fatalf("RotationMembers: %v", err)
	}
	if len(members) != 2 || members[0] != m0 || members[1] != m1 {
		t.Errorf("RotationMembers = %v, want [%v %v]", members, m0, m1)
	}
}

// TestTaskService_CreateRecurringTask_ClaimableNoPool verifies that a claimable
// task is created successfully without a pool.
func TestTaskService_CreateRecurringTask_ClaimableNoPool(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instanceRepo)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	task := &domain.RecurringTask{
		ID:          domain.NewRecurringTaskID(),
		HouseholdID: household.NewHouseholdID(),
		Title:       "Claimable dishes",
		Category:    domain.ChoreCategory,
		Cadence: household.Cadence{
			Freq:     household.FreqWeekly,
			Interval: 1,
			Anchor:   weeklyAnchor,
		},
		RotationPolicy: domain.RotationClaimable,
		Active:         true,
	}

	if err := svc.CreateRecurringTask(context.Background(), task, nil); err != nil {
		t.Fatalf("CreateRecurringTask(claimable, no pool): %v", err)
	}
	if len(taskRepo.tasks) != 1 {
		t.Fatalf("repo has %d tasks, want 1", len(taskRepo.tasks))
	}
}

// ---------------------------------------------------------------------------
// NES-116: as-needed cadence tests
// ---------------------------------------------------------------------------

// newAsNeededTask returns a minimal as-needed recurring task with the given
// rotation policy, for exercising the NES-116 constructor guard.
func newAsNeededTask(policy domain.RotationPolicy) *domain.RecurringTask {
	return &domain.RecurringTask{
		ID:          domain.NewRecurringTaskID(),
		HouseholdID: household.NewHouseholdID(),
		Title:       "Refill the soap dispenser",
		Category:    domain.ChoreCategory,
		Cadence: household.Cadence{
			Freq:     household.FreqAsNeeded,
			Interval: 1,
			Anchor:   weeklyAnchor,
		},
		RotationPolicy: policy,
		Points:         3,
		Active:         true,
	}
}

// TestTaskService_CreateRecurringTask_AsNeededRequiresClaimable is the AC1
// regression test: an as-needed task with a non-claimable rotation policy is
// rejected with ErrAsNeededRequiresClaimable before any repository call.
func TestTaskService_CreateRecurringTask_AsNeededRequiresClaimable(t *testing.T) {
	for _, policy := range []domain.RotationPolicy{domain.RotationFixed, domain.RotationRoundRobin} {
		t.Run(policy.String(), func(t *testing.T) {
			taskRepo := newFakeRecurringTaskRepo()
			instanceRepo := newFakeTaskInstanceRepo()
			svc, err := app.NewTaskService(taskRepo, instanceRepo)
			if err != nil {
				t.Fatalf("NewTaskService: %v", err)
			}

			task := newAsNeededTask(policy)
			err = svc.CreateRecurringTask(context.Background(), task, []household.MemberID{household.NewMemberID()})
			if !errors.Is(err, domain.ErrAsNeededRequiresClaimable) {
				t.Errorf("CreateRecurringTask(as-needed, %s) = %v, want ErrAsNeededRequiresClaimable", policy, err)
			}
			if len(taskRepo.tasks) != 0 {
				t.Errorf("repo has %d tasks after rejection, want 0", len(taskRepo.tasks))
			}
		})
	}
}

// TestTaskService_CreateRecurringTask_AsNeededClaimableSucceeds verifies that
// an as-needed task paired with the claimable policy is accepted.
func TestTaskService_CreateRecurringTask_AsNeededClaimableSucceeds(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()
	svc, err := app.NewTaskService(taskRepo, instanceRepo)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	task := newAsNeededTask(domain.RotationClaimable)
	if err := svc.CreateRecurringTask(context.Background(), task, nil); err != nil {
		t.Fatalf("CreateRecurringTask(as-needed, claimable): %v", err)
	}
	if len(taskRepo.tasks) != 1 {
		t.Fatalf("repo has %d tasks, want 1", len(taskRepo.tasks))
	}
}

// TestGenerator_SkipsAsNeededTasks verifies that the generator never
// materialises scheduled instances for an as-needed task, even across a wide
// window — the recurrence engine must never be invoked for it.
func TestGenerator_SkipsAsNeededTasks(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()

	task := newAsNeededTask(domain.RotationClaimable)
	if err := taskRepo.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	g := newGenerator(t, taskRepo, instanceRepo)
	asOf := weeklyAnchor.AddDate(1, 0, 0) // a year out — would be many occurrences if scheduled.
	count, err := g.GenerateDue(context.Background(), asOf)
	if err != nil {
		t.Fatalf("GenerateDue: %v", err)
	}
	if count != 0 {
		t.Errorf("GenerateDue inserted %d instances for an as-needed task, want 0", count)
	}
	if len(instanceRepo.instances) != 0 {
		t.Errorf("instance repo has %d instances after GenerateDue, want 0 (generator must not materialise as-needed instances)", len(instanceRepo.instances))
	}
}

// TestTaskService_CompleteInstance_StandingRespawns is the AC2/AC3 regression
// test: completing an as-needed task's standing instance transitions it to
// done and materialises a fresh pending standing instance for the same task,
// so exactly one open standing instance exists both before and after
// completion.
func TestTaskService_CompleteInstance_StandingRespawns(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()
	svc, err := app.NewTaskService(taskRepo, instanceRepo)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	task := newAsNeededTask(domain.RotationClaimable)
	if err := taskRepo.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	standing := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: task.ID,
		HouseholdID:     task.HouseholdID,
		Status:          domain.StatusPending,
		Kind:            domain.KindStanding,
	}
	if err := instanceRepo.Insert(context.Background(), standing); err != nil {
		t.Fatalf("seed standing instance: %v", err)
	}

	// Before completion: exactly one open standing instance.
	before, err := instanceRepo.ListStanding(context.Background(), task.HouseholdID)
	if err != nil {
		t.Fatalf("ListStanding (before): %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("ListStanding (before) = %d instances, want 1", len(before))
	}

	m := household.NewMemberID()
	if err := svc.CompleteInstance(context.Background(), task.HouseholdID, standing.ID, m, time.Now()); err != nil {
		t.Fatalf("CompleteInstance(standing): %v", err)
	}

	// The original instance is now done.
	got, err := instanceRepo.Get(context.Background(), task.HouseholdID, standing.ID)
	if err != nil {
		t.Fatalf("Get after completion: %v", err)
	}
	if got.Status != domain.StatusDone {
		t.Errorf("original standing instance status = %v, want done", got.Status)
	}

	// After completion: still exactly one open standing instance, and it is a
	// different row than the one just completed.
	after, err := instanceRepo.ListStanding(context.Background(), task.HouseholdID)
	if err != nil {
		t.Fatalf("ListStanding (after): %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("ListStanding (after) = %d instances, want 1", len(after))
	}
	if after[0].ID == standing.ID {
		t.Error("the open standing instance after completion is the same row that was just completed, want a fresh replacement")
	}
	if after[0].DueOn != nil {
		t.Errorf("respawned standing instance DueOn = %v, want nil", after[0].DueOn)
	}
}

// TestTaskService_SkipInstance_StandingRespawns is the CodeRabbit follow-up
// regression test for NES-116: the "always exactly one open standing
// instance" invariant must hold on the skip path too, not just completion.
// Skipping an as-needed task's standing instance transitions it to skipped,
// awards no points, and materialises a fresh pending standing instance for
// the same task.
func TestTaskService_SkipInstance_StandingRespawns(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()
	svc, err := app.NewTaskService(taskRepo, instanceRepo)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	task := newAsNeededTask(domain.RotationClaimable)
	if err := taskRepo.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	standing := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: task.ID,
		HouseholdID:     task.HouseholdID,
		Status:          domain.StatusPending,
		Kind:            domain.KindStanding,
	}
	if err := instanceRepo.Insert(context.Background(), standing); err != nil {
		t.Fatalf("seed standing instance: %v", err)
	}

	if err := svc.SkipInstance(context.Background(), task.HouseholdID, standing.ID); err != nil {
		t.Fatalf("SkipInstance(standing): %v", err)
	}

	// The original instance is now skipped, not completed — no award path was
	// ever invoked (SkipInstance calls Skip, never CompleteAndAward).
	got, err := instanceRepo.Get(context.Background(), task.HouseholdID, standing.ID)
	if err != nil {
		t.Fatalf("Get after skip: %v", err)
	}
	if got.Status != domain.StatusSkipped {
		t.Errorf("original standing instance status = %v, want skipped", got.Status)
	}
	if got.CompletedAt != nil || got.CompletedBy != nil {
		t.Errorf("skipped standing instance has completion fields set: CompletedAt=%v CompletedBy=%v, want both nil (no award)", got.CompletedAt, got.CompletedBy)
	}

	// After the skip: still exactly one open standing instance, and it is a
	// fresh row, not the one just skipped.
	after, err := instanceRepo.ListStanding(context.Background(), task.HouseholdID)
	if err != nil {
		t.Fatalf("ListStanding (after skip): %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("ListStanding (after skip) = %d instances, want 1", len(after))
	}
	if after[0].ID == standing.ID {
		t.Error("the open standing instance after skip is the same row that was just skipped, want a fresh replacement")
	}
	if after[0].DueOn != nil {
		t.Errorf("respawned standing instance DueOn = %v, want nil", after[0].DueOn)
	}
}

// TestTaskService_CompleteInstance_NotFound verifies that completing an unknown
// instance propagates ErrInstanceNotFound.
func TestTaskService_CompleteInstance_NotFound(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instanceRepo)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	hid := household.NewHouseholdID()
	mid := household.NewMemberID()

	err = svc.CompleteInstance(context.Background(), hid, domain.NewTaskInstanceID(), mid, time.Now())
	if !errors.Is(err, domain.ErrInstanceNotFound) {
		t.Errorf("CompleteInstance(unknown) = %v, want ErrInstanceNotFound", err)
	}
}

// TestTaskService_SkipInstance_NotFound verifies that skipping an unknown
// instance propagates ErrInstanceNotFound.
func TestTaskService_SkipInstance_NotFound(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instanceRepo)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	hid := household.NewHouseholdID()

	err = svc.SkipInstance(context.Background(), hid, domain.NewTaskInstanceID())
	if !errors.Is(err, domain.ErrInstanceNotFound) {
		t.Errorf("SkipInstance(unknown) = %v, want ErrInstanceNotFound", err)
	}
}

// TestTaskService_ClaimInstance_NotFound verifies that claiming an unknown
// instance propagates ErrInstanceNotFound.
func TestTaskService_ClaimInstance_NotFound(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instanceRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instanceRepo)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	hid := household.NewHouseholdID()
	mid := household.NewMemberID()

	err = svc.ClaimInstance(context.Background(), hid, domain.NewTaskInstanceID(), mid)
	if !errors.Is(err, domain.ErrInstanceNotFound) {
		t.Errorf("ClaimInstance(unknown) = %v, want ErrInstanceNotFound", err)
	}
}
