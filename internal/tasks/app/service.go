package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// TaskService orchestrates user-facing task use cases. It is a thin
// application-layer coordinator: business rules live in the domain and
// persistence/atomicity contracts in the adapter. The service adds cross-cutting
// concerns such as input validation and policy checks (e.g. requiring a
// non-empty rotation pool), delegating multi-row atomicity to the repository
// (CreateWithRotation).
//
// Methods are tenant-scoped via householdID arguments — the underlying
// repository methods enforce household isolation at the persistence level.
type TaskService struct {
	taskRepo     domain.RecurringTaskRepository
	instanceRepo domain.TaskInstanceRepository
}

// NewTaskService constructs a TaskService with injected dependencies.
func NewTaskService(
	taskRepo domain.RecurringTaskRepository,
	instanceRepo domain.TaskInstanceRepository,
) (*TaskService, error) {
	if taskRepo == nil {
		return nil, errors.New("app: NewTaskService requires a non-nil task repository")
	}
	if instanceRepo == nil {
		return nil, errors.New("app: NewTaskService requires a non-nil instance repository")
	}
	return &TaskService{
		taskRepo:     taskRepo,
		instanceRepo: instanceRepo,
	}, nil
}

// CreateRecurringTask validates and persists a new recurring task along with
// its initial rotation pool. The caller is responsible for populating task.ID
// (via [domain.NewRecurringTaskID]) and task.HouseholdID before calling.
//
// Validation:
//   - task.Cadence must pass [household.Cadence.Validate].
//   - For [domain.RotationFixed] and [domain.RotationRoundRobin] policies,
//     pool must be non-empty; [domain.ErrNoRotationMembers] is returned
//     otherwise, because instances cannot be assigned without at least one
//     member.
//   - For [domain.RotationClaimable], pool is ignored (may be nil or empty).
//
// Error contracts:
//   - Returns [domain.ErrNoRotationMembers] when pool is empty for a
//     non-claimable policy.
//   - Propagates any repository error from CreateWithRotation. Because that
//     method is atomic, a persistence failure leaves no partially-created task
//     (e.g. a task with no rotation pool).
func (s *TaskService) CreateRecurringTask(
	ctx context.Context,
	task *domain.RecurringTask,
	pool []household.MemberID,
) error {
	if task == nil {
		return errors.New("create recurring task: task is nil")
	}
	if err := task.Cadence.Validate(); err != nil {
		return fmt.Errorf("create recurring task: %w", err)
	}

	// A claimable task has no rotation pool; ignore any pool the caller passed so
	// claimable instances are always materialized unassigned.
	if task.RotationPolicy == domain.RotationClaimable {
		pool = nil
	} else if len(pool) == 0 {
		return fmt.Errorf("create recurring task: %w", domain.ErrNoRotationMembers)
	}

	// Persist the task and its rotation pool atomically so a mid-operation
	// failure can never leave a task without its required rotation members.
	if err := s.taskRepo.CreateWithRotation(ctx, task, pool); err != nil {
		return fmt.Errorf("create recurring task: %w", err)
	}

	return nil
}

// CompleteInstance transitions a task instance from pending or overdue to done,
// recording the completing member (by) and the completion timestamp (at), and
// atomically credits the completing member with the task's point award. An
// overdue chore is still actionable: it can be completed late.
//
// The award is performed atomically with the status transition inside the
// adapter: a single database transaction marks the instance done and appends
// the point ledger row so the two writes are never separated. Tasks with
// points = 0 produce no ledger row. Re-completing an already-done instance
// returns the terminal-state sentinel without making a second award.
//
// Error contracts:
//   - Returns [domain.ErrInstanceNotFound] when id is unknown or belongs to
//     another household.
//   - Returns [domain.ErrInstanceInTerminalState] when the instance is already
//     done or skipped.
func (s *TaskService) CompleteInstance(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
	by household.MemberID,
	at time.Time,
) error {
	if err := s.instanceRepo.CompleteAndAward(ctx, householdID, id, by, at); err != nil {
		return fmt.Errorf("complete instance: %w", err)
	}
	return nil
}

// SkipInstance transitions a task instance from pending or overdue to skipped.
// An overdue chore is still actionable: it can be skipped late.
//
// Error contracts:
//   - Returns [domain.ErrInstanceNotFound] when id is unknown or belongs to
//     another household.
//   - Returns [domain.ErrInstanceInTerminalState] when the instance is already
//     done or skipped.
func (s *TaskService) SkipInstance(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
) error {
	if err := s.instanceRepo.Skip(ctx, householdID, id); err != nil {
		return fmt.Errorf("skip instance: %w", err)
	}
	return nil
}

// ClaimInstance assigns an unassigned pending or overdue instance to assignee.
// An overdue chore is still claimable: it can be picked up late. Claiming is
// first-come: an already-assigned instance cannot be re-claimed.
//
// Error contracts:
//   - Returns [domain.ErrInstanceNotFound] when id is unknown or belongs to
//     another household.
//   - Returns [domain.ErrInstanceInTerminalState] when the instance is done or
//     skipped.
//   - Returns [domain.ErrInstanceAlreadyClaimed] when a pending or overdue
//     instance is already assigned to a member.
func (s *TaskService) ClaimInstance(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
	assignee household.MemberID,
) error {
	if err := s.instanceRepo.Claim(ctx, householdID, id, assignee); err != nil {
		return fmt.Errorf("claim instance: %w", err)
	}
	return nil
}
