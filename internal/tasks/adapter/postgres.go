package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// PostgreSQL SQLSTATE codes used for error mapping.
const (
	// sqlstateUniqueViolation is the PostgreSQL SQLSTATE for a unique-constraint
	// violation (error class 23 "Integrity Constraint Violation", subcode 505).
	sqlstateUniqueViolation = "23505"
)

// Constraint names from 00003_tasks.sql used to map database errors to domain
// sentinels. Naming them here avoids stringly-typed comparisons scattered across
// the adapter.
const (
	// constraintTaskInstanceDuplicateUniq is the unique constraint on
	// (recurring_task_id, due_on) whose violation maps to ErrDuplicateInstance.
	constraintTaskInstanceDuplicateUniq = "task_instance_task_due_uniq"
)

// row abstracts pgx.Row and pgx.Rows so scan helpers can accept either.
type row interface {
	Scan(dest ...any) error
}

// RecurringTaskRepository is the pgx-backed implementation of
// domain.RecurringTaskRepository. UUIDs are passed and scanned as text so no
// pgx UUID codec registration is required. The Cadence jsonb column is handled
// via encoding/json — pgx sends []byte directly to jsonb without a custom codec.
type RecurringTaskRepository struct {
	dbtx db.TX
}

// Compile-time assurance that RecurringTaskRepository satisfies the port.
var _ domain.RecurringTaskRepository = (*RecurringTaskRepository)(nil)

// NewRecurringTaskRepository constructs a RecurringTaskRepository with an
// injected query executor. The executor is a db.TX, satisfied by both
// *pgxpool.Pool (the default composition) and pgx.Tx (so the repository can
// run inside a caller's transaction).
func NewRecurringTaskRepository(dbtx db.TX) *RecurringTaskRepository {
	if dbtx == nil {
		panic("adapter: NewRecurringTaskRepository requires a non-nil db.TX")
	}
	return &RecurringTaskRepository{dbtx: dbtx}
}

// recurringTaskInsertSQL inserts a recurring_task row and returns the
// store-populated created_at/updated_at timestamps. It is shared by Create and
// CreateWithRotation so the column list lives in exactly one place.
const recurringTaskInsertSQL = `
	INSERT INTO recurring_task
		(id, household_id, title, category, cadence, rotation_policy,
		 points, lead_time_days, active)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	RETURNING created_at, updated_at`

// querier abstracts QueryRow so insertRecurringTask works against both the
// repository's db.TX and a pgx.Tx opened for an atomic CreateWithRotation.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// insertRecurringTask marshals rt.Cadence and inserts the recurring_task row via
// q, scanning the generated timestamps back into rt. Callers wrap the returned
// error with their own context prefix.
func insertRecurringTask(ctx context.Context, q querier, rt *domain.RecurringTask) error {
	cadenceJSON, err := json.Marshal(rt.Cadence)
	if err != nil {
		return fmt.Errorf("marshal cadence: %w", err)
	}
	return q.QueryRow(ctx, recurringTaskInsertSQL,
		rt.ID.String(),
		rt.HouseholdID.String(),
		rt.Title,
		rt.Category.String(),
		cadenceJSON,
		rt.RotationPolicy.String(),
		rt.Points,
		rt.LeadTimeDays,
		rt.Active,
	).Scan(&rt.CreatedAt, &rt.UpdatedAt)
}

// Create persists a new recurring task. The caller must populate ID,
// HouseholdID, Title, Category, Cadence, RotationPolicy, Points, LeadTimeDays,
// and Active; the store populates CreatedAt and UpdatedAt.
func (r *RecurringTaskRepository) Create(ctx context.Context, rt *domain.RecurringTask) error {
	if rt == nil {
		return errors.New("adapter: create recurring task: nil task")
	}
	if err := insertRecurringTask(ctx, r.dbtx, rt); err != nil {
		return fmt.Errorf("create recurring task: %w", err)
	}
	return nil
}

// CreateWithRotation atomically persists a new recurring task together with its
// initial rotation pool in a single transaction. The recurring_task row and all
// rotation_member rows are inserted under one transaction; any failure (e.g. a
// member id that does not exist or belongs to another household, which violates
// the composite tenant FK) rolls back the whole operation, leaving no
// recurring_task row behind.
//
// The pool slice order determines rotation position (position = slice index).
// An empty pool persists the task with no rotation members.
func (r *RecurringTaskRepository) CreateWithRotation(
	ctx context.Context,
	task *domain.RecurringTask,
	pool []household.MemberID,
) error {
	if task == nil {
		return errors.New("adapter: create recurring task with rotation: nil task")
	}

	// db.TX is either a *pgxpool.Pool or a pgx.Tx; both expose Begin (pgx.Tx.Begin
	// opens a savepoint), so this is safe in either context — the same pattern
	// SetRotationMembers uses.
	beginner, ok := r.dbtx.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return errors.New("create recurring task with rotation: executor does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("create recurring task with rotation: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := insertRecurringTask(ctx, tx, task); err != nil {
		return fmt.Errorf("create recurring task with rotation: insert task: %w", err)
	}

	const ins = `
		INSERT INTO rotation_member (household_id, recurring_task_id, member_id, position)
		VALUES ($1, $2, $3, $4)`
	for i, memberID := range pool {
		if _, err := tx.Exec(ctx, ins,
			task.HouseholdID.String(),
			task.ID.String(),
			memberID.String(),
			i,
		); err != nil {
			return fmt.Errorf("create recurring task with rotation: insert position %d: %w", i, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("create recurring task with rotation: commit: %w", err)
	}
	return nil
}

// Get returns the recurring task identified by id within the household, or
// domain.ErrTaskNotFound when id is unknown or belongs to another household.
func (r *RecurringTaskRepository) Get(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RecurringTaskID,
) (*domain.RecurringTask, error) {
	const q = `
		SELECT id, household_id, title, category, cadence, rotation_policy,
		       points, lead_time_days, active, created_at, updated_at
		  FROM recurring_task
		 WHERE id = $1
		   AND household_id = $2`
	rt, err := scanRecurringTask(r.dbtx.QueryRow(ctx, q, id.String(), householdID.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTaskNotFound
		}
		return nil, fmt.Errorf("get recurring task: %w", err)
	}
	return rt, nil
}

// ListActive returns all active recurring tasks for the household, ordered by
// creation time. Returns an empty slice (not an error) for an unknown household.
func (r *RecurringTaskRepository) ListActive(
	ctx context.Context,
	householdID household.HouseholdID,
) ([]*domain.RecurringTask, error) {
	const q = `
		SELECT id, household_id, title, category, cadence, rotation_policy,
		       points, lead_time_days, active, created_at, updated_at
		  FROM recurring_task
		 WHERE household_id = $1
		   AND active = true
		 ORDER BY created_at`
	rows, err := r.dbtx.Query(ctx, q, householdID.String())
	if err != nil {
		return nil, fmt.Errorf("list active recurring tasks: %w", err)
	}
	defer rows.Close()

	tasks := make([]*domain.RecurringTask, 0)
	for rows.Next() {
		rt, err := scanRecurringTask(rows)
		if err != nil {
			return nil, fmt.Errorf("list active recurring tasks: scan: %w", err)
		}
		tasks = append(tasks, rt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active recurring tasks: %w", err)
	}
	return tasks, nil
}

// ListAllActive returns every active recurring task across ALL households,
// ordered by household_id then created_at.
//
// WARNING: this method is intentionally NOT household-scoped. It is reserved
// for the background materialisation process (Generator.GenerateDue) and must
// not be called from user-facing request handlers, which must use the
// household-scoped [ListActive] instead.
func (r *RecurringTaskRepository) ListAllActive(ctx context.Context) ([]*domain.RecurringTask, error) {
	const q = `
		SELECT id, household_id, title, category, cadence, rotation_policy,
		       points, lead_time_days, active, created_at, updated_at
		  FROM recurring_task
		 WHERE active = true
		 ORDER BY household_id, created_at`
	rows, err := r.dbtx.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list all active recurring tasks: %w", err)
	}
	defer rows.Close()

	tasks := make([]*domain.RecurringTask, 0)
	for rows.Next() {
		rt, err := scanRecurringTask(rows)
		if err != nil {
			return nil, fmt.Errorf("list all active recurring tasks: scan: %w", err)
		}
		tasks = append(tasks, rt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all active recurring tasks: %w", err)
	}
	return tasks, nil
}

// SetRotationMembers atomically replaces the rotation pool for the task within
// the household. Position is determined by the slice order (position = index).
// Passing an empty slice clears the pool.
//
// Atomicity note: the delete+insert pair runs against the provided db.TX. When
// dbtx is a *pgxpool.Pool, the two statements are not wrapped in a single
// database transaction; callers requiring strict atomicity must supply a pgx.Tx.
// NES-30 use-cases wrap this call in a transaction when needed.
//
// Returns domain.ErrTaskNotFound when id is unknown or belongs to another
// household.
func (r *RecurringTaskRepository) SetRotationMembers(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RecurringTaskID,
	members []household.MemberID,
) error {
	// Verify the task exists in this household before touching the rotation pool.
	if _, err := r.Get(ctx, householdID, id); err != nil {
		return err
	}

	// Replace the pool atomically so a mid-loop failure cannot leave it partially
	// rewritten. db.TX is either a *pgxpool.Pool or a pgx.Tx; both expose Begin
	// (pgx.Tx.Begin opens a savepoint), so this is safe in either context.
	beginner, ok := r.dbtx.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return errors.New("set rotation members: executor does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("set rotation members: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const del = `DELETE FROM rotation_member WHERE recurring_task_id = $1`
	if _, err := tx.Exec(ctx, del, id.String()); err != nil {
		return fmt.Errorf("set rotation members: delete existing: %w", err)
	}

	const ins = `
		INSERT INTO rotation_member (household_id, recurring_task_id, member_id, position)
		VALUES ($1, $2, $3, $4)`
	for i, memberID := range members {
		if _, err := tx.Exec(ctx, ins,
			householdID.String(),
			id.String(),
			memberID.String(),
			i,
		); err != nil {
			return fmt.Errorf("set rotation members: insert position %d: %w", i, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("set rotation members: commit: %w", err)
	}
	return nil
}

// RotationMembers returns the rotation pool members ordered by position for the
// task within the household. Returns domain.ErrTaskNotFound when the task is
// unknown or belongs to another household. Returns an empty slice (not an error)
// when the pool is empty.
func (r *RecurringTaskRepository) RotationMembers(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RecurringTaskID,
) ([]household.MemberID, error) {
	// Verify the task exists in this household before querying its pool.
	if _, err := r.Get(ctx, householdID, id); err != nil {
		return nil, err
	}

	const q = `
		SELECT rm.member_id
		  FROM rotation_member rm
		  JOIN recurring_task rt
		    ON rm.recurring_task_id = rt.id
		 WHERE rm.recurring_task_id = $1
		   AND rt.household_id = $2
		 ORDER BY rm.position`
	rows, err := r.dbtx.Query(ctx, q, id.String(), householdID.String())
	if err != nil {
		return nil, fmt.Errorf("rotation members: %w", err)
	}
	defer rows.Close()

	members := make([]household.MemberID, 0)
	for rows.Next() {
		var memberIDStr string
		if err := rows.Scan(&memberIDStr); err != nil {
			return nil, fmt.Errorf("rotation members: scan: %w", err)
		}
		memberID, err := household.ParseMemberID(memberIDStr)
		if err != nil {
			return nil, fmt.Errorf("rotation members: parse member id: %w", err)
		}
		members = append(members, memberID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rotation members: %w", err)
	}
	return members, nil
}

// scanRecurringTask scans a recurring_task row from r, unmarshalling the cadence
// jsonb column into a household.Cadence value.
func scanRecurringTask(r row) (*domain.RecurringTask, error) {
	var (
		rt                                              domain.RecurringTask
		idStr, householdIDStr, category, rotationPolicy string
		cadenceJSON                                     []byte
	)
	err := r.Scan(
		&idStr,
		&householdIDStr,
		&rt.Title,
		&category,
		&cadenceJSON,
		&rotationPolicy,
		&rt.Points,
		&rt.LeadTimeDays,
		&rt.Active,
		&rt.CreatedAt,
		&rt.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	id, err := domain.ParseRecurringTaskID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan recurring task: %w", err)
	}
	householdID, err := household.ParseHouseholdID(householdIDStr)
	if err != nil {
		return nil, fmt.Errorf("scan recurring task: %w", err)
	}
	cat, err := domain.ParseCategory(category)
	if err != nil {
		return nil, fmt.Errorf("scan recurring task: %w", err)
	}
	policy, err := domain.ParseRotationPolicy(rotationPolicy)
	if err != nil {
		return nil, fmt.Errorf("scan recurring task: %w", err)
	}
	if err := json.Unmarshal(cadenceJSON, &rt.Cadence); err != nil {
		return nil, fmt.Errorf("scan recurring task: unmarshal cadence: %w", err)
	}

	rt.ID = id
	rt.HouseholdID = householdID
	rt.Category = cat
	rt.RotationPolicy = policy
	return &rt, nil
}

// TaskInstanceRepository is the pgx-backed implementation of
// domain.TaskInstanceRepository. UUIDs are passed and scanned as text so no
// pgx UUID codec registration is required.
type TaskInstanceRepository struct {
	dbtx db.TX
}

// Compile-time assurance that TaskInstanceRepository satisfies the port.
var _ domain.TaskInstanceRepository = (*TaskInstanceRepository)(nil)

// NewTaskInstanceRepository constructs a TaskInstanceRepository with an
// injected query executor. The executor is a db.TX, satisfied by both
// *pgxpool.Pool (the default composition) and pgx.Tx (so the repository can
// run inside a caller's transaction).
func NewTaskInstanceRepository(dbtx db.TX) *TaskInstanceRepository {
	if dbtx == nil {
		panic("adapter: NewTaskInstanceRepository requires a non-nil db.TX")
	}
	return &TaskInstanceRepository{dbtx: dbtx}
}

// Insert persists a new task instance. The caller must populate ID,
// RecurringTaskID, HouseholdID, DueOn, Status, and optionally AssigneeID; the
// store populates CreatedAt and UpdatedAt.
//
// Returns domain.ErrDuplicateInstance on a (recurring_task_id, due_on)
// conflict (constraint task_instance_task_due_uniq).
func (r *TaskInstanceRepository) Insert(ctx context.Context, inst *domain.TaskInstance) error {
	if inst == nil {
		return errors.New("adapter: insert task instance: nil instance")
	}
	dueOn := domain.DateOf(inst.DueOn)

	var assigneeIDStr *string
	if inst.AssigneeID != nil {
		s := inst.AssigneeID.String()
		assigneeIDStr = &s
	}

	const q = `
		INSERT INTO task_instance
			(id, household_id, recurring_task_id, assignee_id, due_on, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at`
	err := r.dbtx.QueryRow(ctx, q,
		inst.ID.String(),
		inst.HouseholdID.String(),
		inst.RecurringTaskID.String(),
		assigneeIDStr,
		dueOn,
		inst.Status.String(),
	).Scan(&inst.CreatedAt, &inst.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) &&
			pgErr.Code == sqlstateUniqueViolation &&
			pgErr.ConstraintName == constraintTaskInstanceDuplicateUniq {
			return domain.ErrDuplicateInstance
		}
		return fmt.Errorf("insert task instance: %w", err)
	}
	inst.DueOn = dueOn
	return nil
}

// Get returns the task instance identified by id within the household, or
// domain.ErrInstanceNotFound when id is unknown or belongs to another household.
func (r *TaskInstanceRepository) Get(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
) (*domain.TaskInstance, error) {
	const q = `
		SELECT id, household_id, recurring_task_id, assignee_id,
		       due_on, status, completed_at, completed_by,
		       created_at, updated_at
		  FROM task_instance
		 WHERE id = $1
		   AND household_id = $2`
	inst, err := scanTaskInstance(r.dbtx.QueryRow(ctx, q, id.String(), householdID.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrInstanceNotFound
		}
		return nil, fmt.Errorf("get task instance: %w", err)
	}
	return inst, nil
}

// ListByHousehold returns instances for the household filtered by status and
// due date range [from, to] (inclusive), ordered by due_on. Returns an empty
// slice when no instances match.
func (r *TaskInstanceRepository) ListByHousehold(
	ctx context.Context,
	householdID household.HouseholdID,
	status domain.InstanceStatus,
	from, to time.Time,
) ([]*domain.TaskInstance, error) {
	const q = `
		SELECT id, household_id, recurring_task_id, assignee_id,
		       due_on, status, completed_at, completed_by,
		       created_at, updated_at
		  FROM task_instance
		 WHERE household_id = $1
		   AND status = $2
		   AND due_on BETWEEN $3 AND $4
		 ORDER BY due_on`
	rows, err := r.dbtx.Query(ctx, q,
		householdID.String(),
		status.String(),
		domain.DateOf(from),
		domain.DateOf(to),
	)
	if err != nil {
		return nil, fmt.Errorf("list task instances by household: %w", err)
	}
	defer rows.Close()

	instances := make([]*domain.TaskInstance, 0)
	for rows.Next() {
		inst, err := scanTaskInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("list task instances by household: scan: %w", err)
		}
		instances = append(instances, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list task instances by household: %w", err)
	}
	return instances, nil
}

// LatestDueOn returns the most recent due_on materialised for the task within
// the household and ok=true, or the zero time and ok=false when no instances
// exist yet.
func (r *TaskInstanceRepository) LatestDueOn(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RecurringTaskID,
) (time.Time, bool, error) {
	const q = `
		SELECT max(due_on)
		  FROM task_instance
		 WHERE household_id = $1
		   AND recurring_task_id = $2`
	var maxDueOn *time.Time
	if err := r.dbtx.QueryRow(ctx, q, householdID.String(), id.String()).Scan(&maxDueOn); err != nil {
		return time.Time{}, false, fmt.Errorf("latest due on: %w", err)
	}
	if maxDueOn == nil {
		return time.Time{}, false, nil
	}
	return domain.DateOf(*maxDueOn), true, nil
}

// Claim assigns the instance to assignee when it is pending or overdue and
// currently unassigned. An overdue chore is still claimable — it can be picked
// up late. On 0 rows affected, the instance is read to produce the precise
// sentinel: domain.ErrInstanceNotFound, domain.ErrInstanceInTerminalState
// (done/skipped), or domain.ErrInstanceAlreadyClaimed (already assigned).
func (r *TaskInstanceRepository) Claim(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
	assignee household.MemberID,
) error {
	const q = `
		UPDATE task_instance
		   SET assignee_id = $3,
		       updated_at  = now()
		 WHERE id          = $1
		   AND household_id = $2
		   AND status       IN ('pending', 'overdue')
		   AND assignee_id IS NULL`
	tag, err := r.dbtx.Exec(ctx, q, id.String(), householdID.String(), assignee.String())
	if err != nil {
		return fmt.Errorf("claim task instance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return r.disambiguateClaim(ctx, householdID, id)
	}
	return nil
}

// disambiguateClaim reads the instance to determine why a Claim UPDATE matched
// zero rows, returning the appropriate domain sentinel. A done or skipped row is
// terminal (ErrInstanceInTerminalState); a pending-or-overdue row that is
// already assigned yields ErrInstanceAlreadyClaimed.
func (r *TaskInstanceRepository) disambiguateClaim(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
) error {
	inst, err := r.Get(ctx, householdID, id)
	if err != nil {
		// Covers both ErrInstanceNotFound and unexpected DB errors.
		return err
	}
	if inst.Status != domain.StatusPending && inst.Status != domain.StatusOverdue {
		// done or skipped → terminal.
		return domain.ErrInstanceInTerminalState
	}
	// Status is pending or overdue but assignee_id is not null.
	return domain.ErrInstanceAlreadyClaimed
}

// Complete transitions the instance from pending or overdue to done, recording
// by and at. An overdue chore is still actionable — it can be completed late.
// On 0 rows affected, the instance is read to produce the precise sentinel:
// domain.ErrInstanceNotFound or domain.ErrInstanceInTerminalState (done/skipped).
func (r *TaskInstanceRepository) Complete(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
	by household.MemberID,
	at time.Time,
) error {
	const q = `
		UPDATE task_instance
		   SET status       = 'done',
		       completed_by = $3,
		       completed_at = $4,
		       updated_at   = now()
		 WHERE id           = $1
		   AND household_id = $2
		   AND status       IN ('pending', 'overdue')`
	tag, err := r.dbtx.Exec(ctx, q, id.String(), householdID.String(), by.String(), at)
	if err != nil {
		return fmt.Errorf("complete task instance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return r.disambiguateTerminal(ctx, householdID, id)
	}
	return nil
}

// Skip transitions the instance from pending or overdue to skipped. An overdue
// chore is still actionable — it can be skipped late. On 0 rows affected, the
// instance is read to produce the precise sentinel: domain.ErrInstanceNotFound
// or domain.ErrInstanceInTerminalState (done/skipped).
func (r *TaskInstanceRepository) Skip(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
) error {
	const q = `
		UPDATE task_instance
		   SET status       = 'skipped',
		       updated_at   = now()
		 WHERE id           = $1
		   AND household_id = $2
		   AND status       IN ('pending', 'overdue')`
	tag, err := r.dbtx.Exec(ctx, q, id.String(), householdID.String())
	if err != nil {
		return fmt.Errorf("skip task instance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return r.disambiguateTerminal(ctx, householdID, id)
	}
	return nil
}

// disambiguateTerminal reads the instance to determine why a Complete or Skip
// UPDATE matched zero rows, returning domain.ErrInstanceNotFound when the row
// does not exist in the household or domain.ErrInstanceInTerminalState when it
// is already done or skipped. Because Complete and Skip now act on both pending
// and overdue rows, a zero-row update for an existing instance means the row is
// in a terminal state (done or skipped).
func (r *TaskInstanceRepository) disambiguateTerminal(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
) error {
	if _, err := r.Get(ctx, householdID, id); err != nil {
		// Covers both ErrInstanceNotFound and unexpected DB errors.
		return err
	}
	return domain.ErrInstanceInTerminalState
}

// MarkPendingOverdue bulk-transitions all pending instances for the household
// whose due_on < asOf to overdue. Returns the number of rows updated.
func (r *TaskInstanceRepository) MarkPendingOverdue(
	ctx context.Context,
	householdID household.HouseholdID,
	asOf time.Time,
) (int, error) {
	const q = `
		UPDATE task_instance
		   SET status     = 'overdue',
		       updated_at = now()
		 WHERE household_id = $1
		   AND status       = 'pending'
		   AND due_on       < $2`
	tag, err := r.dbtx.Exec(ctx, q, householdID.String(), domain.DateOf(asOf))
	if err != nil {
		return 0, fmt.Errorf("mark pending overdue: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// MarkPendingOverdueAll bulk-transitions all pending instances across ALL
// households whose due_on < asOf to overdue. It returns the newly-overdue rows
// as [domain.ReminderTarget] values so the caller can enqueue overdue
// notifications without an additional query. Callers that only want the count
// use len() on the returned slice.
//
// Implementation: a single UPDATE … RETURNING captures the transitioned rows,
// then one follow-up SELECT fetches recurring_task title+category keyed by the
// distinct recurring_task_ids. This avoids N+1 while keeping the UPDATE simple
// (UPDATE … RETURNING cannot JOIN).
//
// WARNING: this method is intentionally NOT household-scoped. It is a
// system-process method reserved for the background scheduler (NES-31) and
// must not be called from user-facing request handlers, which must use the
// household-scoped [MarkPendingOverdue] instead.
func (r *TaskInstanceRepository) MarkPendingOverdueAll(ctx context.Context, asOf time.Time) ([]domain.ReminderTarget, error) {
	const updateQ = `
		UPDATE task_instance
		   SET status     = 'overdue',
		       updated_at = now()
		 WHERE status = 'pending'
		   AND due_on < $1
		RETURNING id, household_id, assignee_id, due_on, recurring_task_id`

	rows, err := r.dbtx.Query(ctx, updateQ, domain.DateOf(asOf))
	if err != nil {
		return nil, fmt.Errorf("mark pending overdue all: %w", err)
	}
	defer rows.Close()

	return scanReminderRows(ctx, rows, domain.ReminderOverdue, "mark pending overdue all", r)
}

// ClaimDueSoonReminders atomically selects pending instances inside the closed
// due-soon window (asOf <= due_on <= asOf + lead_time_days) that have not yet
// been reminded (reminded_at IS NULL), stamps reminded_at = now() on each, and
// returns them as [domain.ReminderTarget] values (Kind=[domain.ReminderDueSoon]).
//
// The lower bound (due_on >= asOf) deliberately excludes already-past-due rows:
// a pending row with due_on < asOf is overdue (or about to be transitioned by
// the overdue sweep) and must be handled by the overdue path, not the due-soon
// path. Without the lower bound, an overdue sweep failure in the same tick would
// leak a past-due pending row into the due-soon stream.
//
// A CTE with SELECT … FOR UPDATE SKIP LOCKED + UPDATE keeps the claim atomic
// and safe for concurrent callers. Because reminded_at is set in the same
// statement, a row can only ever be returned once.
//
// WARNING: this method is intentionally NOT household-scoped. It is a
// system-process method reserved for the background scheduler (NES-34) and
// must not be called from user-facing request handlers.
func (r *TaskInstanceRepository) ClaimDueSoonReminders(ctx context.Context, asOf time.Time) ([]domain.ReminderTarget, error) {
	// Closed due-soon window: asOf <= due_on <= asOf + lead_time_days. The upper
	// bound is expressed as
	//   ti.due_on <= $1::date + make_interval(days => rt.lead_time_days)
	// so Postgres can use the partial index on due_on; the lower bound
	// (ti.due_on >= $1::date) excludes past-due pending rows from the due-soon
	// stream.
	const q = `
		WITH due AS (
			SELECT ti.id
			  FROM task_instance ti
			  JOIN recurring_task rt ON rt.id = ti.recurring_task_id
			 WHERE ti.status       = 'pending'
			   AND ti.reminded_at IS NULL
			   AND ti.due_on      >= $1::date
			   AND ti.due_on      <= $1::date + make_interval(days => rt.lead_time_days)
			   FOR UPDATE OF ti SKIP LOCKED
		)
		UPDATE task_instance ti
		   SET reminded_at = now()
		  FROM due
		 WHERE ti.id = due.id
		RETURNING ti.id, ti.household_id, ti.assignee_id, ti.due_on,
		          ti.recurring_task_id`

	rows, err := r.dbtx.Query(ctx, q, domain.DateOf(asOf))
	if err != nil {
		return nil, fmt.Errorf("claim due-soon reminders: %w", err)
	}
	defer rows.Close()

	return scanReminderRows(ctx, rows, domain.ReminderDueSoon, "claim due-soon reminders", r)
}

// ClearDueSoonReminder resets reminded_at to NULL for the instance so a later
// [ClaimDueSoonReminders] call can re-claim it. It is the recovery counterpart
// to ClaimDueSoonReminders: the caller invokes it when a due-soon enqueue fails
// after the row was claimed, so the reminder is retried instead of lost.
//
// An unknown id is a no-op (nil error): recovery must be idempotent and
// tolerant of a row deleted between claim and clear.
//
// WARNING: this method is intentionally NOT household-scoped. It is a
// system-process recovery method reserved for the background scheduler (NES-34)
// and must not be called from user-facing request handlers.
func (r *TaskInstanceRepository) ClearDueSoonReminder(ctx context.Context, id domain.TaskInstanceID) error {
	const q = `UPDATE task_instance SET reminded_at = NULL WHERE id = $1`
	if _, err := r.dbtx.Exec(ctx, q, id.String()); err != nil {
		return fmt.Errorf("clear due-soon reminder: %w", err)
	}
	return nil
}

// reminderRow is the intermediate representation for a single row returned by
// MarkPendingOverdueAll or ClaimDueSoonReminders before task metadata is joined
// in memory.
type reminderRow struct {
	instanceID      domain.TaskInstanceID
	householdID     household.HouseholdID
	assigneeIDStr   *string
	dueOn           time.Time
	recurringTaskID domain.RecurringTaskID
}

// scanReminderRows consumes an open pgx.Rows cursor containing the five columns
// (id, household_id, assignee_id, due_on, recurring_task_id), fetches
// title+category for the distinct recurring tasks in one follow-up query, and
// assembles the final []domain.ReminderTarget slice. op is a short label used
// in error messages.
func scanReminderRows(
	ctx context.Context,
	rows interface {
		Next() bool
		Scan(dest ...any) error
		Err() error
	},
	kind domain.ReminderKind,
	op string,
	r *TaskInstanceRepository,
) ([]domain.ReminderTarget, error) {
	var scanned []reminderRow
	taskIDSet := make(map[domain.RecurringTaskID]bool)

	for rows.Next() {
		var (
			instStr, hhStr, rtStr string
			assigneeIDStr         *string
			dueOn                 time.Time
		)
		if err := rows.Scan(&instStr, &hhStr, &assigneeIDStr, &dueOn, &rtStr); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", op, err)
		}
		instID, err := domain.ParseTaskInstanceID(instStr)
		if err != nil {
			return nil, fmt.Errorf("%s: parse instance id: %w", op, err)
		}
		hhID, err := household.ParseHouseholdID(hhStr)
		if err != nil {
			return nil, fmt.Errorf("%s: parse household id: %w", op, err)
		}
		rtID, err := domain.ParseRecurringTaskID(rtStr)
		if err != nil {
			return nil, fmt.Errorf("%s: parse recurring task id: %w", op, err)
		}
		scanned = append(scanned, reminderRow{
			instanceID:      instID,
			householdID:     hhID,
			assigneeIDStr:   assigneeIDStr,
			dueOn:           domain.DateOf(dueOn),
			recurringTaskID: rtID,
		})
		taskIDSet[rtID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if len(scanned) == 0 {
		return nil, nil
	}

	meta, err := r.fetchTaskMeta(ctx, taskIDSet)
	if err != nil {
		return nil, fmt.Errorf("%s: fetch task meta: %w", op, err)
	}

	targets := make([]domain.ReminderTarget, 0, len(scanned))
	for _, row := range scanned {
		m := meta[row.recurringTaskID]
		target := domain.ReminderTarget{
			InstanceID:  row.instanceID,
			HouseholdID: row.householdID,
			Title:       m.title,
			Category:    m.category,
			DueOn:       row.dueOn,
			Kind:        kind,
		}
		if row.assigneeIDStr != nil {
			memberID, err := household.ParseMemberID(*row.assigneeIDStr)
			if err != nil {
				return nil, fmt.Errorf("%s: parse assignee id: %w", op, err)
			}
			target.AssigneeID = &memberID
		}
		targets = append(targets, target)
	}
	return targets, nil
}

// taskMeta holds the minimal fields fetched from recurring_task for
// constructing reminder notifications.
type taskMeta struct {
	title    string
	category domain.Category
}

// fetchTaskMeta performs a single SELECT to look up title and category for
// the recurring tasks identified by taskIDSet. It returns a map keyed by
// RecurringTaskID so callers can do O(1) lookups per row.
func (r *TaskInstanceRepository) fetchTaskMeta(
	ctx context.Context,
	taskIDSet map[domain.RecurringTaskID]bool,
) (map[domain.RecurringTaskID]taskMeta, error) {
	ids := make([]string, 0, len(taskIDSet))
	for id := range taskIDSet {
		ids = append(ids, id.String())
	}

	const q = `
		SELECT id, title, category
		  FROM recurring_task
		 WHERE id = ANY($1::uuid[])`
	rows, err := r.dbtx.Query(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("fetch task meta: %w", err)
	}
	defer rows.Close()

	result := make(map[domain.RecurringTaskID]taskMeta, len(taskIDSet))
	for rows.Next() {
		var idStr, title, categoryStr string
		if err := rows.Scan(&idStr, &title, &categoryStr); err != nil {
			return nil, fmt.Errorf("fetch task meta: scan: %w", err)
		}
		id, err := domain.ParseRecurringTaskID(idStr)
		if err != nil {
			return nil, fmt.Errorf("fetch task meta: parse id: %w", err)
		}
		cat, err := domain.ParseCategory(categoryStr)
		if err != nil {
			return nil, fmt.Errorf("fetch task meta: parse category: %w", err)
		}
		result[id] = taskMeta{title: title, category: cat}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fetch task meta: %w", err)
	}
	return result, nil
}

// scanTaskInstance scans a task_instance row from r. Nullable columns
// (assignee_id, completed_at, completed_by) are read into pointer types and
// converted to domain pointer fields. DueOn is normalized with domain.DateOf.
func scanTaskInstance(r row) (*domain.TaskInstance, error) {
	var (
		inst                                              domain.TaskInstance
		idStr, householdIDStr, recurringTaskIDStr, status string
		assigneeIDStr, completedByStr                     *string
		completedAt                                       *time.Time
		dueOn                                             time.Time
	)
	err := r.Scan(
		&idStr,
		&householdIDStr,
		&recurringTaskIDStr,
		&assigneeIDStr,
		&dueOn,
		&status,
		&completedAt,
		&completedByStr,
		&inst.CreatedAt,
		&inst.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	id, err := domain.ParseTaskInstanceID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan task instance: %w", err)
	}
	householdID, err := household.ParseHouseholdID(householdIDStr)
	if err != nil {
		return nil, fmt.Errorf("scan task instance: %w", err)
	}
	recurringTaskID, err := domain.ParseRecurringTaskID(recurringTaskIDStr)
	if err != nil {
		return nil, fmt.Errorf("scan task instance: %w", err)
	}
	instanceStatus, err := domain.ParseInstanceStatus(status)
	if err != nil {
		return nil, fmt.Errorf("scan task instance: %w", err)
	}

	inst.ID = id
	inst.HouseholdID = householdID
	inst.RecurringTaskID = recurringTaskID
	inst.Status = instanceStatus
	inst.DueOn = domain.DateOf(dueOn)
	inst.CompletedAt = completedAt

	if assigneeIDStr != nil {
		memberID, err := household.ParseMemberID(*assigneeIDStr)
		if err != nil {
			return nil, fmt.Errorf("scan task instance: parse assignee id: %w", err)
		}
		inst.AssigneeID = &memberID
	}
	if completedByStr != nil {
		memberID, err := household.ParseMemberID(*completedByStr)
		if err != nil {
			return nil, fmt.Errorf("scan task instance: parse completed_by id: %w", err)
		}
		inst.CompletedBy = &memberID
	}
	return &inst, nil
}
