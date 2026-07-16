package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// Generator materialises [domain.TaskInstance] rows for active recurring tasks
// up to a configurable horizon ahead of the current time. It is designed to run
// as a background system process across ALL households; for user-facing
// operations use [TaskService] instead.
//
// Idempotency guarantee: calling [Generator.GenerateDue] more than once with
// the same asOf produces no duplicate rows. The [domain.ErrDuplicateInstance]
// sentinel from the instance repository is treated as a benign skip so that
// re-runs and overlapping scheduler ticks are safe.
type Generator struct {
	taskRepo     domain.RecurringTaskRepository
	instanceRepo domain.TaskInstanceRepository
	logger       *slog.Logger
	horizon      time.Duration
}

// NewGenerator constructs a Generator with injected dependencies.
//   - taskRepo provides access to all active recurring tasks across all
//     households via [domain.RecurringTaskRepository.ListAllActive].
//   - instanceRepo is used to query the latest materialised due date and to
//     insert new instances.
//   - logger receives structured log lines; task IDs (not PII) are the only
//     identifiers logged.
//   - horizon controls how far ahead of asOf instances are materialised. A
//     value of 14*24*time.Hour materialises two weeks of upcoming instances.
func NewGenerator(
	taskRepo domain.RecurringTaskRepository,
	instanceRepo domain.TaskInstanceRepository,
	logger *slog.Logger,
	horizon time.Duration,
) (*Generator, error) {
	if taskRepo == nil {
		return nil, errors.New("app: NewGenerator requires a non-nil task repository")
	}
	if instanceRepo == nil {
		return nil, errors.New("app: NewGenerator requires a non-nil instance repository")
	}
	if logger == nil {
		return nil, errors.New("app: NewGenerator requires a non-nil logger")
	}
	if horizon <= 0 {
		return nil, fmt.Errorf("app: NewGenerator horizon must be positive, got %v", horizon)
	}
	return &Generator{
		taskRepo:     taskRepo,
		instanceRepo: instanceRepo,
		logger:       logger,
		horizon:      horizon,
	}, nil
}

// GenerateDue materialises pending [domain.TaskInstance] rows for every active
// recurring task, covering the window (latestMaterialised, asOf+horizon].
//
// For each task the stable zero-based occurrence ordinal is computed once from
// the cadence's full history, guaranteeing that re-running GenerateDue with the
// same asOf assigns identical assignees (idempotency). Tasks that fail to
// materialise are logged and skipped — one bad task never aborts the rest.
//
// Returns the count of newly-inserted instances (duplicate inserts are not
// counted). Returns the first non-duplicate error encountered across all tasks,
// if any, after processing every task.
func (g *Generator) GenerateDue(ctx context.Context, asOf time.Time) (int, error) {
	tasks, err := g.taskRepo.ListAllActive(ctx)
	if err != nil {
		return 0, fmt.Errorf("generator: list all active tasks: %w", err)
	}

	horizon := domain.DateOf(asOf.Add(g.horizon))

	var (
		totalInserted int
		firstErr      error
	)

	for _, task := range tasks {
		inserted, err := g.materialiseTask(ctx, task, horizon)
		totalInserted += inserted
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return totalInserted, firstErr
}

// materialiseTask generates and inserts all missing instances for a single
// recurring task within the given window. horizon is the inclusive upper bound
// for occurrence dates, computed by the caller as DateOf(asOf + g.horizon).
// It returns the number of newly inserted rows and any non-duplicate error.
func (g *Generator) materialiseTask(
	ctx context.Context,
	task *domain.RecurringTask,
	horizon time.Time,
) (int, error) {
	// As-needed tasks are never scheduled by the recurrence engine (NES-116):
	// they have a single standing instance materialised when the task is
	// created and replaced in the same transaction on every completion
	// (TaskInstanceRepository.CreateWithRotation / CompleteAndAward), not by
	// ahead-of-time generation. Skipping here avoids an unnecessary
	// LatestDueOn round trip for every poll cycle.
	if task.Cadence.Freq == household.FreqAsNeeded {
		return 0, nil
	}

	// Determine the start of the generation window.
	windowStart, err := g.windowStart(ctx, task)
	if err != nil {
		g.logger.Error("generator: failed to determine window start",
			"task_id", task.ID.String(),
			"error", err,
		)
		return 0, fmt.Errorf("generator: task %s: window start: %w", task.ID, err)
	}

	// Validate the persisted cadence before expanding occurrences. A corrupt
	// cadence (e.g. interval 0 from a manual edit) would make OccurrencesBetween
	// loop forever, so skip the task rather than hang the generator.
	if err := task.Cadence.Validate(); err != nil {
		g.logger.Error("generator: skipping task with invalid cadence",
			"task_id", task.ID.String(),
			"error", err,
		)
		return 0, fmt.Errorf("generator: task %s: invalid cadence: %w", task.ID, err)
	}

	occurrences := task.Cadence.OccurrencesBetween(windowStart, horizon)
	if len(occurrences) == 0 {
		return 0, nil
	}

	// Compute the base ordinal: number of occurrences already at or before
	// windowStart. Combined with the loop index i, this gives each occurrence
	// its stable, globally-ordered zero-based ordinal regardless of how many
	// prior GenerateDue runs have taken place.
	anchorMinusOne := task.Cadence.Anchor.Add(-time.Nanosecond)
	base := len(task.Cadence.OccurrencesBetween(anchorMinusOne, windowStart))

	// Fetch the rotation pool for non-claimable policies up front, so a single
	// missing pool aborts the whole task rather than producing partially-assigned
	// batches.
	var pool []household.MemberID
	if task.RotationPolicy != domain.RotationClaimable {
		pool, err = g.taskRepo.RotationMembers(ctx, task.HouseholdID, task.ID)
		if err != nil {
			g.logger.Error("generator: failed to fetch rotation pool",
				"task_id", task.ID.String(),
				"error", err,
			)
			return 0, fmt.Errorf("generator: task %s: rotation members: %w", task.ID, err)
		}
		if len(pool) == 0 {
			g.logger.Warn("generator: skipping task with empty rotation pool",
				"task_id", task.ID.String(),
				"policy", task.RotationPolicy.String(),
			)
			return 0, domain.ErrNoRotationMembers
		}
	}

	inserted := 0
	for i, occ := range occurrences {
		ordinal := base + i
		assignee := assigneeFor(task.RotationPolicy, pool, ordinal)

		inst := &domain.TaskInstance{
			ID:              domain.NewTaskInstanceID(),
			RecurringTaskID: task.ID,
			HouseholdID:     task.HouseholdID,
			AssigneeID:      assignee,
			DueOn:           domain.DueOnPtr(occ),
			Status:          domain.StatusPending,
			Kind:            domain.KindScheduled,
		}

		if err := g.instanceRepo.Insert(ctx, inst); err != nil {
			if errors.Is(err, domain.ErrDuplicateInstance) {
				// Already materialised — benign skip; idempotency is the contract.
				continue
			}
			g.logger.Error("generator: failed to insert task instance",
				"task_id", task.ID.String(),
				"due_on", inst.DueOn.Format(time.DateOnly),
				"error", err,
			)
			return inserted, fmt.Errorf("generator: task %s: insert due %s: %w",
				task.ID, inst.DueOn.Format(time.DateOnly), err)
		}
		inserted++
	}

	return inserted, nil
}

// windowStart returns the time.Time that the generation window begins from.
// If instances already exist, the window starts from the latest materialised
// due_on (so OccurrencesBetween uses the exclusive-start semantics of the
// cadence to avoid re-generating the already-materialised occurrence). If no
// instances exist, the window starts just before the Anchor so the Anchor
// itself is included as occurrence ordinal 0.
func (g *Generator) windowStart(
	ctx context.Context,
	task *domain.RecurringTask,
) (time.Time, error) {
	latest, ok, err := g.instanceRepo.LatestDueOn(ctx, task.HouseholdID, task.ID)
	if err != nil {
		return time.Time{}, err
	}
	if ok {
		return latest, nil
	}
	// No instances yet: position the window start just before the Anchor so the
	// Anchor occurrence is captured by OccurrencesBetween's (start, end] window.
	return task.Cadence.Anchor.Add(-time.Nanosecond), nil
}

// assigneeFor returns the member that should be assigned to an occurrence with
// the given zero-based ordinal under the given rotation policy and pool.
//
// This is a pure function of policy, pool, and ordinal — it carries no mutable
// state, so two independent calls with the same arguments always return the
// same result. This is the mechanism that guarantees assignment stability across
// repeated GenerateDue runs (idempotency of assignee selection).
//
// Preconditions (enforced by materialiseTask before this is called):
//   - For [domain.RotationFixed] and [domain.RotationRoundRobin], pool is non-empty.
//   - For [domain.RotationClaimable], pool may be nil or empty (return is nil).
func assigneeFor(
	policy domain.RotationPolicy,
	pool []household.MemberID,
	ordinal int,
) *household.MemberID {
	switch policy {
	case domain.RotationFixed:
		m := pool[0]
		return &m
	case domain.RotationRoundRobin:
		m := pool[ordinal%len(pool)]
		return &m
	case domain.RotationClaimable:
		return nil
	default:
		return nil
	}
}
