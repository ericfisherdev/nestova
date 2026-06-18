package domain

import "errors"

// Domain errors for the tasks bounded context.
var (
	// ErrTaskNotFound is returned by RecurringTaskRepository.Get when the
	// requested RecurringTaskID does not exist.
	ErrTaskNotFound = errors.New("tasks: recurring task not found")

	// ErrInstanceNotFound is returned by TaskInstanceRepository.Get when the
	// requested TaskInstanceID does not exist.
	ErrInstanceNotFound = errors.New("tasks: task instance not found")

	// ErrNoRotationMembers is returned by the generator when a non-claimable
	// recurring task has an empty rotation pool (i.e. SetRotationMembers was
	// never called, or the pool was cleared). A task with RotationFixed or
	// RotationRoundRobin must have at least one member in its pool before
	// instances can be materialised.
	ErrNoRotationMembers = errors.New("tasks: rotation pool is empty")

	// ErrInstanceInTerminalState is returned by TaskInstanceRepository.Complete
	// and TaskInstanceRepository.Skip when the target instance is already in a
	// terminal state (done, skipped, or overdue). Callers should check this
	// sentinel and treat it as a no-op or surface it to the user accordingly.
	ErrInstanceInTerminalState = errors.New("tasks: instance is already in a terminal state")

	// ErrInstanceAlreadyClaimed is returned by TaskInstanceRepository.Claim when
	// the target instance is already assigned to a member. Claiming is
	// first-come, not reassignment: an already-assigned instance cannot be
	// re-claimed by another member.
	ErrInstanceAlreadyClaimed = errors.New("tasks: instance is already claimed")

	// ErrDuplicateInstance is returned by TaskInstanceRepository.Insert when an
	// instance already exists for the (recurring_task_id, due_on) pair. The
	// generator uses this sentinel to implement idempotent materialisation:
	// re-running the generator for an already-materialised window is safe.
	ErrDuplicateInstance = errors.New("tasks: instance already exists for this task and due date")
)
