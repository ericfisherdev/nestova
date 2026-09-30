package domain_test

import (
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// ChoreTradeID
// ---------------------------------------------------------------------------

func TestChoreTradeIDRoundTrip(t *testing.T) {
	id := domain.NewChoreTradeID()
	s := id.String()
	parsed, err := domain.ParseChoreTradeID(s)
	if err != nil {
		t.Fatalf("ParseChoreTradeID(%q) error = %v, want nil", s, err)
	}
	if parsed != id {
		t.Errorf("ParseChoreTradeID(%q) = %v, want %v", s, parsed, id)
	}
}

func TestParseChoreTradeID_Invalid(t *testing.T) {
	if _, err := domain.ParseChoreTradeID("not-a-uuid"); err == nil {
		t.Error("ParseChoreTradeID(invalid) error = nil, want non-nil")
	}
}

// ---------------------------------------------------------------------------
// TradeStatus
// ---------------------------------------------------------------------------

func TestTradeStatusParseAndValid(t *testing.T) {
	cases := []struct {
		input string
		want  domain.TradeStatus
	}{
		{"proposed", domain.TradeProposed},
		{"accepted", domain.TradeAccepted},
		{"declined", domain.TradeDeclined},
		{"cancelled", domain.TradeCancelled},
		{"expired", domain.TradeExpired},
	}
	for _, tc := range cases {
		got, err := domain.ParseTradeStatus(tc.input)
		if err != nil {
			t.Errorf("ParseTradeStatus(%q) error = %v, want nil", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("ParseTradeStatus(%q) = %v, want %v", tc.input, got, tc.want)
		}
		if !got.Valid() {
			t.Errorf("TradeStatus(%q).Valid() = false, want true", tc.input)
		}
		if got.String() != tc.input {
			t.Errorf("TradeStatus(%q).String() = %q, want %q", tc.input, got.String(), tc.input)
		}
	}
}

func TestTradeStatusParseUnknown(t *testing.T) {
	_, err := domain.ParseTradeStatus("pending")
	if err == nil {
		t.Error("ParseTradeStatus(unknown) error = nil, want non-nil")
	}
}

func TestTradeStatusValid_Unknown(t *testing.T) {
	if domain.TradeStatus("pending").Valid() {
		t.Error(`TradeStatus("pending").Valid() = true, want false`)
	}
}

// TestTradeStatus_CanTransitionTo covers every (from, to) pair across the five
// known statuses plus an unknown value, locking in the state machine: proposed
// is the only status with any legal outgoing transition, and every other
// status (including proposed -> proposed, and every terminal -> anything) is
// illegal.
func TestTradeStatus_CanTransitionTo(t *testing.T) {
	all := []domain.TradeStatus{
		domain.TradeProposed, domain.TradeAccepted, domain.TradeDeclined,
		domain.TradeCancelled, domain.TradeExpired,
	}

	cases := []struct {
		from, to domain.TradeStatus
		want     bool
	}{
		{domain.TradeProposed, domain.TradeAccepted, true},
		{domain.TradeProposed, domain.TradeDeclined, true},
		{domain.TradeProposed, domain.TradeCancelled, true},
		{domain.TradeProposed, domain.TradeExpired, true},
		{domain.TradeProposed, domain.TradeProposed, false},
		{domain.TradeAccepted, domain.TradeProposed, false},
		{domain.TradeDeclined, domain.TradeProposed, false},
		{domain.TradeCancelled, domain.TradeProposed, false},
		{domain.TradeExpired, domain.TradeProposed, false},
		{domain.TradeAccepted, domain.TradeAccepted, false},
		{domain.TradeStatus("bogus"), domain.TradeAccepted, false},
		{domain.TradeProposed, domain.TradeStatus("bogus"), false},
	}
	for _, tc := range cases {
		if got := tc.from.CanTransitionTo(tc.to); got != tc.want {
			t.Errorf("CanTransitionTo(%q -> %q) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}

	// Exhaustively assert that every terminal status has zero legal outgoing
	// transitions, across all five known targets.
	terminal := []domain.TradeStatus{
		domain.TradeAccepted, domain.TradeDeclined, domain.TradeCancelled, domain.TradeExpired,
	}
	for _, from := range terminal {
		for _, to := range all {
			if from.CanTransitionTo(to) {
				t.Errorf("terminal status %q.CanTransitionTo(%q) = true, want false", from, to)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// IsInstanceTradeable
// ---------------------------------------------------------------------------

func newTradeableInstance() *domain.TaskInstance {
	assignee := household.NewMemberID()
	due := domain.DueOnPtr(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
	return &domain.TaskInstance{
		ID:         domain.NewTaskInstanceID(),
		AssigneeID: &assignee,
		DueOn:      due,
		Status:     domain.StatusPending,
		Kind:       domain.KindScheduled,
	}
}

func TestIsInstanceTradeable_PendingScheduledUnclaimed(t *testing.T) {
	inst := newTradeableInstance()
	if !domain.IsInstanceTradeable(inst) {
		t.Error("IsInstanceTradeable(pending, scheduled, unclaimed) = false, want true")
	}
}

func TestIsInstanceTradeable_RejectsNonPendingStatus(t *testing.T) {
	statuses := []domain.InstanceStatus{
		domain.StatusDone, domain.StatusSkipped, domain.StatusOverdue,
	}
	for _, status := range statuses {
		inst := newTradeableInstance()
		inst.Status = status
		if domain.IsInstanceTradeable(inst) {
			t.Errorf("IsInstanceTradeable(status=%q) = true, want false", status)
		}
	}
}

func TestIsInstanceTradeable_RejectsStandingKind(t *testing.T) {
	inst := newTradeableInstance()
	inst.Kind = domain.KindStanding
	inst.DueOn = nil
	if domain.IsInstanceTradeable(inst) {
		t.Error("IsInstanceTradeable(kind=standing) = true, want false")
	}
}

func TestIsInstanceTradeable_RejectsClaimedInstance(t *testing.T) {
	inst := newTradeableInstance()
	claimant := household.NewMemberID()
	inst.ClaimedBy = &claimant
	if domain.IsInstanceTradeable(inst) {
		t.Error("IsInstanceTradeable(claimed) = true, want false")
	}
}

// TestIsInstanceTradeable_RejectsNilInstance guards against a nil-pointer
// dereference: a nil *TaskInstance is never tradeable.
func TestIsInstanceTradeable_RejectsNilInstance(t *testing.T) {
	if domain.IsInstanceTradeable(nil) {
		t.Error("IsInstanceTradeable(nil) = true, want false")
	}
}

// TestIsInstanceTradeable_RejectsNilDueOn covers the defensive case: an
// otherwise-tradeable-shaped instance (pending, scheduled, unclaimed) with a
// nil DueOn — which validateInstanceKindDueOn's insert-time invariant should
// prevent in practice, but a hand-built fixture could still produce — must
// not be treated as tradeable, since Propose's expires_at computation
// dereferences DueOn on both sides once IsInstanceTradeable passes.
func TestIsInstanceTradeable_RejectsNilDueOn(t *testing.T) {
	inst := newTradeableInstance()
	inst.DueOn = nil
	if domain.IsInstanceTradeable(inst) {
		t.Error("IsInstanceTradeable(scheduled, nil DueOn) = true, want false")
	}
}

// TestTradeExpiry_IsEndOfDueDayInLocalTime covers NES-198: the trade window
// closes at local midnight ending the due day, not at 00:00 UTC of the due
// date.
func TestTradeExpiry_IsEndOfDueDayInLocalTime(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	due := domain.DateOf(time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC))

	got := domain.TradeExpiry(due, chicago)

	want := time.Date(2025, 3, 11, 0, 0, 0, 0, chicago)
	if !got.Equal(want) {
		t.Errorf("TradeExpiry = %v, want %v", got, want)
	}
}

func TestIsInstanceTradeableAt_DayBoundary(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	inst := newTradeableInstance()
	inst.DueOn = domain.DueOnPtr(time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC))

	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"evening before the due day", time.Date(2025, 3, 9, 20, 0, 0, 0, chicago), true},
		{"due day, past 00:00 UTC", time.Date(2025, 3, 10, 21, 30, 0, 0, chicago), true},
		{"last instant of the due day", time.Date(2025, 3, 10, 23, 59, 59, 0, chicago), true},
		{"local midnight after the due day", time.Date(2025, 3, 11, 0, 0, 0, 0, chicago), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.IsInstanceTradeableAt(inst, tt.now, chicago); got != tt.want {
				t.Errorf("IsInstanceTradeableAt(now=%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

func TestIsInstanceTradeableAt_RejectsUntradeableInstance(t *testing.T) {
	inst := newTradeableInstance()
	inst.Status = domain.StatusDone
	if domain.IsInstanceTradeableAt(inst, time.Time{}, time.UTC) {
		t.Error("IsInstanceTradeableAt(done instance) = true, want false")
	}
}
