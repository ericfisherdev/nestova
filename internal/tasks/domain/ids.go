package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// RecurringTaskID uniquely identifies a recurring task template.
type RecurringTaskID uuid.UUID

// TaskInstanceID uniquely identifies a materialised task instance.
type TaskInstanceID uuid.UUID

// NewRecurringTaskID returns a new time-ordered (UUIDv7) recurring task id,
// which gives better B-tree index locality than random v4 ids. uuid.NewV7 only
// errors if the crypto random source is unavailable — the same failure under
// which uuid.New itself panics — so Must is appropriate here.
func NewRecurringTaskID() RecurringTaskID { return RecurringTaskID(uuid.Must(uuid.NewV7())) }

// NewTaskInstanceID returns a new time-ordered (UUIDv7) task instance id.
func NewTaskInstanceID() TaskInstanceID { return TaskInstanceID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id RecurringTaskID) String() string { return uuid.UUID(id).String() }

// String returns the canonical UUID string.
func (id TaskInstanceID) String() string { return uuid.UUID(id).String() }

// ParseRecurringTaskID parses a canonical UUID string into a RecurringTaskID.
func ParseRecurringTaskID(s string) (RecurringTaskID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return RecurringTaskID{}, fmt.Errorf("parse recurring task id: %w", err)
	}
	return RecurringTaskID(u), nil
}

// ParseTaskInstanceID parses a canonical UUID string into a TaskInstanceID.
func ParseTaskInstanceID(s string) (TaskInstanceID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return TaskInstanceID{}, fmt.Errorf("parse task instance id: %w", err)
	}
	return TaskInstanceID(u), nil
}
