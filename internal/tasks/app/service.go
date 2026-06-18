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
// persistence contracts in the adapter. The service adds cross-cutting
// concerns such as input validation and multi-step coordination (e.g.
// Create+SetRotationMembers).
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
	if err := task.Cadence.Validate(); err != nil {
		return fmt.Errorf("create recurring task: %w", err)
	}

	if task.RotationPolicy != domain.RotationClaimable && len(pool) == 0 {
		return fmt.Errorf("create recurring task: %w", domain.ErrNoRotationMembers)
	}

	// Persist the task and its rotation pool atomically so a mid-operation
	// failure can never leave a task without its required rotation members.
	if err := s.taskRepo.CreateWithRotation(ctx, task, pool); err != nil {
		return fmt.Errorf("create recurring task: %w", err)
	}

	return nil
}

// CompleteInstance transitions a task instance from pending to done, recording
// the completing member (by) and the completion timestamp (at).
//
// Error contracts:
//   - Returns [domain.ErrInstanceNotFound] when id is unknown or belongs to
//     another household.
//   - Returns [domain.ErrInstanceInTerminalState] when the instance is already
//     done, skipped, or overdue.
func (s *TaskService) CompleteInstance(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
	by household.MemberID,
	at time.Time,
) error {
	if err := s.instanceRepo.Complete(ctx, householdID, id, by, at); err != nil {
		return fmt.Errorf("complete instance: %w", err)
	}
	return nil
}

// SkipInstance transitions a task instance from pending to skipped.
//
// Error contracts:
//   - Returns [domain.ErrInstanceNotFound] when id is unknown or belongs to
//     another household.
//   - Returns [domain.ErrInstanceInTerminalState] when the instance is already
//     in a terminal state.
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

// ClaimInstance assigns an unassigned pending instance to assignee.
// Claiming is first-come: an already-assigned instance cannot be re-claimed.
//
// Error contracts:
//   - Returns [domain.ErrInstanceNotFound] when id is unknown or belongs to
//     another household.
//   - Returns [domain.ErrInstanceInTerminalState] when the instance is done,
//     skipped, or overdue.
//   - Returns [domain.ErrInstanceAlreadyClaimed] when a pending instance is
//     already assigned to a member.
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
