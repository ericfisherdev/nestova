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

// PhotoPolicy governs whether completing an instance of a recurring task
// requires proof photos (NES-119/NES-120), and which ones. Stored as text on
// recurring_task (not task_instance — see the 00030 migration's doc for why
// this is a join, not a per-instance copy), validated here. The values match
// the recurring_task.photo_policy CHECK constraint.
type PhotoPolicy string

// Photo policies for recurring tasks.
const (
	// PhotoPolicyNone requires no proof photos to complete an instance — the
	// default, and the effective policy for every recurring task created
	// before NES-120.
	PhotoPolicyNone PhotoPolicy = "none"
	// PhotoPolicyAfterOnly requires a single "after" chore-proof photo
	// (domain.PhotoKindAfter in the media bounded context) before an
	// instance can be completed.
	PhotoPolicyAfterOnly PhotoPolicy = "after_only"
	// PhotoPolicyBeforeAfter requires both a "before" and an "after"
	// chore-proof photo before an instance can be completed.
	PhotoPolicyBeforeAfter PhotoPolicy = "before_after"
)

// Valid reports whether p is a known photo policy.
func (p PhotoPolicy) Valid() bool {
	switch p {
	case PhotoPolicyNone, PhotoPolicyAfterOnly, PhotoPolicyBeforeAfter:
		return true
	default:
		return false
	}
}

// String returns the photo policy's stored value.
func (p PhotoPolicy) String() string { return string(p) }

// RequiresPhotos reports whether p requires at least one chore-proof photo
// before an instance may be completed. The zero Go value ("") is treated
// the same as PhotoPolicyNone: only the persistence adapter's INSERT path
// defaults "" to PhotoPolicyNone on write (see insertRecurringTask's doc in
// tasks/adapter), so a RecurringTask constructed directly in Go — as every
// test that predates NES-120 already does, without ever setting this field
// — must behave identically to one round-tripped through the database with
// PhotoPolicyNone, not be mistaken for a policy requiring photos.
func (p PhotoPolicy) RequiresPhotos() bool {
	return p != "" && p != PhotoPolicyNone
}

// ParsePhotoPolicy validates and returns a PhotoPolicy, or an error for an
// unknown value.
func ParsePhotoPolicy(s string) (PhotoPolicy, error) {
	p := PhotoPolicy(s)
	if !p.Valid() {
		return "", fmt.Errorf("invalid photo policy %q", s)
	}
	return p, nil
}

// InstanceKind classifies how a task instance was materialised. Stored as
// text, validated here. The values match the task_instance.kind CHECK
// constraint (NES-116).
type InstanceKind string

// Task instance kinds.
const (
	// KindScheduled marks an instance materialised ahead of time by the
	// recurrence engine (the generator) for a dated cadence. It always carries
	// a non-nil DueOn.
	KindScheduled InstanceKind = "scheduled"
	// KindStanding marks the single open instance of an as-needed
	// (household.FreqAsNeeded) recurring task. It has a nil DueOn and is never
	// produced by the recurrence engine: one is materialised when the parent
	// task is created, and a fresh one replaces it in the same transaction
	// every time it is completed, so an as-needed task always has exactly one
	// open standing instance.
	KindStanding InstanceKind = "standing"
)

// Valid reports whether k is a known instance kind.
func (k InstanceKind) Valid() bool {
	switch k {
	case KindScheduled, KindStanding:
		return true
	default:
		return false
	}
}

// String returns the instance kind's stored value.
func (k InstanceKind) String() string { return string(k) }

// ParseInstanceKind validates and returns an InstanceKind, or an error for an
// unknown value.
func ParseInstanceKind(s string) (InstanceKind, error) {
	k := InstanceKind(s)
	if !k.Valid() {
		return "", fmt.Errorf("invalid instance kind %q", s)
	}
	return k, nil
}
