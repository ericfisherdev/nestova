package domain

import (
	"fmt"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// ReminderKind classifies the reason a task reminder is emitted. Stored as
// text in the notification source_type field context.
type ReminderKind string

// Task reminder kinds.
const (
	// ReminderDueSoon is emitted once when a pending instance first enters its
	// lead-time window (due_on - lead_time_days <= today). Idempotency is
	// enforced by the reminded_at column: ClaimDueSoonReminders atomically marks
	// reminded_at and returns the row exactly once.
	ReminderDueSoon ReminderKind = "due_soon"
	// ReminderOverdue is emitted once on the pending→overdue transition.
	// Idempotency is inherent: MarkPendingOverdueAll only transitions (and thus
	// returns) each row once.
	ReminderOverdue ReminderKind = "overdue"
)

// Valid reports whether k is a known reminder kind.
func (k ReminderKind) Valid() bool {
	switch k {
	case ReminderDueSoon, ReminderOverdue:
		return true
	default:
		return false
	}
}

// String returns the reminder kind's string value.
func (k ReminderKind) String() string { return string(k) }

// ParseReminderKind validates and returns a ReminderKind, or an error for an
// unknown value.
func ParseReminderKind(s string) (ReminderKind, error) {
	k := ReminderKind(s)
	if !k.Valid() {
		return "", fmt.Errorf("invalid reminder kind %q", s)
	}
	return k, nil
}

// ReminderTarget carries the fields needed to build and route a task reminder
// notification. It is returned by ClaimDueSoonReminders and
// MarkPendingOverdueAll so the application layer can enqueue notifications
// without additional database queries.
type ReminderTarget struct {
	// InstanceID is the task_instance.id that triggered this reminder.
	InstanceID TaskInstanceID
	// HouseholdID scopes the notification to the correct household.
	HouseholdID household.HouseholdID
	// AssigneeID is nil when the instance is unassigned (claimable policy).
	// When set, the notification is addressed to that member.
	AssigneeID *household.MemberID
	// Title is the recurring_task.title, used in the notification body.
	Title string
	// Category identifies the task type (chore vs maintenance) for human-readable
	// notification text.
	Category Category
	// DueOn is the instance's due date (midnight UTC).
	DueOn time.Time
	// Kind indicates whether this is a due-soon or overdue reminder.
	Kind ReminderKind
}
