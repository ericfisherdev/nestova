package domain

import "fmt"

// Category classifies the kind of recurring task. Stored as text, validated
// here. The values match the recurring_task.category CHECK constraint.
type Category string

// Recurring task categories.
const (
	// ChoreCategory marks a routine household chore (e.g. vacuuming, dishes).
	ChoreCategory Category = "chore"
	// MaintenanceCategory marks a maintenance or repair task (e.g. filter change).
	MaintenanceCategory Category = "maintenance"
)

// Valid reports whether c is a known category.
func (c Category) Valid() bool {
	switch c {
	case ChoreCategory, MaintenanceCategory:
		return true
	default:
		return false
	}
}

// String returns the category's stored value.
func (c Category) String() string { return string(c) }

// ParseCategory validates and returns a Category, or an error for an unknown value.
func ParseCategory(s string) (Category, error) {
	c := Category(s)
	if !c.Valid() {
		return "", fmt.Errorf("invalid category %q", s)
	}
	return c, nil
}

// RotationPolicy governs how a recurring task's assignee is determined each
// cycle. Stored as text, validated here. The values match the
// recurring_task.rotation_policy CHECK constraint.
type RotationPolicy string

// Rotation policies for recurring tasks.
const (
	// RotationFixed assigns the task to a single fixed member every cycle.
	RotationFixed RotationPolicy = "fixed"
	// RotationRoundRobin cycles through the task's rotation pool in position
	// order, advancing one slot per materialised instance.
	RotationRoundRobin RotationPolicy = "round_robin"
	// RotationClaimable leaves the instance unassigned until a household member
	// claims it.
	RotationClaimable RotationPolicy = "claimable"
)

// Valid reports whether p is a known rotation policy.
func (p RotationPolicy) Valid() bool {
	switch p {
	case RotationFixed, RotationRoundRobin, RotationClaimable:
		return true
	default:
		return false
	}
}

// String returns the rotation policy's stored value.
func (p RotationPolicy) String() string { return string(p) }

// ParseRotationPolicy validates and returns a RotationPolicy, or an error for
// an unknown value.
func ParseRotationPolicy(s string) (RotationPolicy, error) {
	p := RotationPolicy(s)
	if !p.Valid() {
		return "", fmt.Errorf("invalid rotation policy %q", s)
	}
	return p, nil
}

// InstanceStatus is the lifecycle state of a task instance. Stored as text,
// validated here. The values match the task_instance.status CHECK constraint.
type InstanceStatus string

// Task instance lifecycle statuses.
const (
	// StatusPending marks an instance that has not yet been acted on.
	StatusPending InstanceStatus = "pending"
	// StatusDone marks an instance that was completed by a household member.
	StatusDone InstanceStatus = "done"
	// StatusSkipped marks an instance that was explicitly skipped for this cycle.
	StatusSkipped InstanceStatus = "skipped"
	// StatusOverdue marks an instance whose due_on has passed without completion.
	StatusOverdue InstanceStatus = "overdue"
)

// Valid reports whether s is a known instance status.
func (s InstanceStatus) Valid() bool {
	switch s {
	case StatusPending, StatusDone, StatusSkipped, StatusOverdue:
		return true
	default:
		return false
	}
}

// String returns the status's stored value.
func (s InstanceStatus) String() string { return string(s) }

// ParseInstanceStatus validates and returns an InstanceStatus, or an error for
// an unknown value.
func ParseInstanceStatus(s string) (InstanceStatus, error) {
	st := InstanceStatus(s)
	if !st.Valid() {
		return "", fmt.Errorf("invalid instance status %q", s)
	}
	return st, nil
}
