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
	photoChecker domain.ProofPhotoChecker
}

// NewTaskService constructs a TaskService with injected dependencies.
// photoChecker (NES-120) may be nil: only a recurring task whose PhotoPolicy
// is not [domain.PhotoPolicyNone] ever needs it, so most callers — every
// task that predates NES-120's photo policy feature — are unaffected by a
// nil value. CompleteInstance fails closed rather than silently bypassing
// the gate if a photo-policy-requiring task is ever completed with no
// checker configured — see its doc.
func NewTaskService(
	taskRepo domain.RecurringTaskRepository,
	instanceRepo domain.TaskInstanceRepository,
	photoChecker domain.ProofPhotoChecker,
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
		photoChecker: photoChecker,
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
//   - Returns [domain.ErrAsNeededRequiresClaimable] when task.Cadence.Freq is
//     household.FreqAsNeeded and task.RotationPolicy is not
//     [domain.RotationClaimable] (NES-116).
//   - Returns [domain.ErrNoRotationMembers] when pool is empty for a
//     non-claimable policy.
//   - Propagates any repository error from CreateWithRotation. Because that
//     method is atomic, a persistence failure leaves no partially-created task
//     (e.g. a task with no rotation pool, or — for an as-needed task — a task
//     with no standing instance).
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
	// An as-needed task's single standing instance is always unassigned until
	// claimed, so a fixed or round-robin rotation policy could never be
	// honoured — reject the combination before it reaches persistence.
	if task.Cadence.Freq == household.FreqAsNeeded && task.RotationPolicy != domain.RotationClaimable {
		return fmt.Errorf("create recurring task: %w", domain.ErrAsNeededRequiresClaimable)
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
// NES-120 photo policy gate: when the instance's parent recurring task is
// ACTIVE and its PhotoPolicy is not [domain.PhotoPolicyNone], completion is
// blocked until the required chore-proof photo(s) (NES-119) have been
// captured. This check reads the CURRENT policy (a fresh
// RecurringTaskRepository.Get, not a value cached on the instance — see
// [domain.RecurringTask.PhotoPolicy]'s doc) and the instance's CURRENT
// photo state via [s.photoChecker], and both happen BEFORE — and outside
// the same transaction as — the CompleteAndAward call below. This is a
// deliberate read-then-act sequence, not an atomic check: nothing
// currently deletes a task_instance_photo row (see media's
// TaskInstancePhotoRepository, which exposes no delete method), so a racing
// photo removal between this check and CompleteAndAward is not a real
// exposure today; if a delete capability is ever added, this gate would
// need to move inside CompleteAndAward's own transaction to stay race-free.
// The same applies to a racing policy EDIT: no code path updates
// photo_policy after task creation today (there is no edit-task feature in
// this codebase), so the read above can never observe a policy that changes
// mid-request; if an edit-task flow is ever added, this read and the photo
// check together would need to move inside CompleteAndAward's own
// transaction for the same reason.
//
// Inactive-parent exemption: an instance whose parent task is inactive
// (archived) is NEVER gated on PhotoPolicy, regardless of what the policy
// value is — see [domain.RecurringTask.PhotoPolicy]'s doc for the
// rationale. This also keeps the gate consistent with the /tasks row
// builder, which only resolves capture-UI metadata for an ACTIVE parent
// (an inactive parent's row already renders as "(archived)" with no
// capture controls at all — gating completion on a photo the member has no
// way to capture from that row would be a dead end).
//
// Error contracts:
//   - Returns [domain.ErrInstanceNotFound] when id is unknown or belongs to
//     another household.
//   - Returns [domain.ErrInstanceInTerminalState] when the instance is already
//     done or skipped — checked before the photo gate, so an already-finished
//     chore reports "already done" rather than a photo requirement.
//   - Returns [domain.ErrBeforePhotoRequired] or [domain.ErrAfterPhotoRequired]
//     when the parent task is ACTIVE and its PhotoPolicy requires a photo
//     that has not been captured yet.
func (s *TaskService) CompleteInstance(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
	by household.MemberID,
	at time.Time,
) error {
	inst, err := s.instanceRepo.Get(ctx, householdID, id)
	if err != nil {
		return fmt.Errorf("complete instance: %w", err)
	}
	if inst.Status == domain.StatusDone || inst.Status == domain.StatusSkipped {
		return fmt.Errorf("complete instance: %w", domain.ErrInstanceInTerminalState)
	}

	task, err := s.taskRepo.Get(ctx, householdID, inst.RecurringTaskID)
	if err != nil {
		return fmt.Errorf("complete instance: %w", err)
	}
	if task.Active && task.PhotoPolicy.RequiresPhotos() {
		if err := s.requirePhotos(ctx, householdID, id, task.PhotoPolicy); err != nil {
			return err
		}
	}

	if err := s.instanceRepo.CompleteAndAward(ctx, householdID, id, by, at); err != nil {
		return fmt.Errorf("complete instance: %w", err)
	}
	return nil
}

// requirePhotos checks instanceID's captured chore-proof photos against
// policy, returning [domain.ErrBeforePhotoRequired] or
// [domain.ErrAfterPhotoRequired] for the first unmet requirement, or nil
// when policy is satisfied. Callers must already have confirmed
// policy != [domain.PhotoPolicyNone].
func (s *TaskService) requirePhotos(
	ctx context.Context,
	householdID household.HouseholdID,
	instanceID domain.TaskInstanceID,
	policy domain.PhotoPolicy,
) error {
	if s.photoChecker == nil {
		// A recurring task requires proof photos but no checker was wired at
		// the composition root — a misconfiguration, not a user-facing
		// outcome. Fail closed (never silently allow completion) rather than
		// treating the policy as satisfied.
		return fmt.Errorf("complete instance: recurring task requires proof photos (policy %q) but no photo checker is configured", policy)
	}
	beforeID, afterID, err := s.photoChecker.ProofPhotos(ctx, householdID, instanceID)
	if err != nil {
		return fmt.Errorf("complete instance: check proof photos: %w", err)
	}
	if policy == domain.PhotoPolicyBeforeAfter && beforeID == "" {
		return domain.ErrBeforePhotoRequired
	}
	if afterID == "" {
		return domain.ErrAfterPhotoRequired
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

// ClaimInstance assigns a pending or overdue instance to assignee. An
// overdue chore is still claimable: it can be picked up late. Claiming is
// first-come for anyone else: an instance already assigned to a DIFFERENT
// member cannot be taken over.
//
// NES-117: when the instance was previously unassigned (claimable, or a
// NES-116 standing instance), the claim is at risk — it expires
// [domain.ClaimWindow] after being claimed and incurs
// [domain.ClaimExpiryPenalty] if not completed by then (enforced by the
// background scheduler's claim-expiry sweep). When assignee already held the
// instance (a self-claim, always true for a fixed/round-robin instance),
// claiming it records the claim but carries no expiry — see
// [domain.TaskInstanceRepository.Claim] for the full contract.
//
// Error contracts:
//   - Returns [domain.ErrInstanceNotFound] when id is unknown or belongs to
//     another household.
//   - Returns [domain.ErrInstanceInTerminalState] when the instance is done or
//     skipped.
//   - Returns [domain.ErrInstanceAlreadyClaimed] when a pending or overdue
//     instance is already assigned to a different member than assignee.
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
