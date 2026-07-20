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

	// sqlstateForeignKeyViolation is the PostgreSQL SQLSTATE for a foreign-key
	// violation (error class 23 "Integrity Constraint Violation", subcode 503).
	sqlstateForeignKeyViolation = "23503"
)

// Constraint names from 00003_tasks.sql used to map database errors to domain
// sentinels. Naming them here avoids stringly-typed comparisons scattered across
// the adapter.
const (
	// constraintTaskInstanceDuplicateUniq is the unique constraint on
	// (recurring_task_id, due_on) whose violation maps to ErrDuplicateInstance.
	constraintTaskInstanceDuplicateUniq = "task_instance_task_due_uniq"

	// constraintRewardRedemptionRewardFK is the composite foreign key on
	// reward_redemption (household_id, reward_id) → reward whose violation maps
	// to ErrRewardNotFound. The sibling reward_redemption_member_fk must NOT map
	// to that sentinel — a missing member is a distinct failure with no
	// gamification sentinel, so it falls through to a wrapped generic error.
	constraintRewardRedemptionRewardFK = "reward_redemption_reward_fk"

	// constraintRewardRedemptionDeepLinkSignatureUniq is the partial unique
	// index on reward_redemption (household_id, deep_link_signature_hash)
	// WHERE deep_link_signature_hash IS NOT NULL (00027, NES-129) whose
	// violation maps to ErrDeepLinkAlreadyRedeemed: a signed kiosk QR deep
	// link that has already redeemed a reward once cannot redeem again.
	constraintRewardRedemptionDeepLinkSignatureUniq = "reward_redemption_deep_link_signature_uniq"
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
		 points, lead_time_days, active, photo_policy)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
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
	// The zero Go value of PhotoPolicy ("") is treated as PhotoPolicyNone
	// here, not rejected — mirroring how TaskInstanceRepository.Insert
	// already treats a zero-value Kind as KindScheduled for NES-116. Every
	// caller that predates NES-120 (every existing test and production call
	// site) constructs a RecurringTask without ever setting PhotoPolicy, so
	// defaulting at the persistence boundary — rather than requiring every
	// such caller to be updated — keeps their behavior unchanged.
	photoPolicy := rt.PhotoPolicy
	if photoPolicy == "" {
		photoPolicy = domain.PhotoPolicyNone
	}
	if err := q.QueryRow(ctx, recurringTaskInsertSQL,
		rt.ID.String(),
		rt.HouseholdID.String(),
		rt.Title,
		rt.Category.String(),
		cadenceJSON,
		rt.RotationPolicy.String(),
		rt.Points,
		rt.LeadTimeDays,
		rt.Active,
		photoPolicy.String(),
	).Scan(&rt.CreatedAt, &rt.UpdatedAt); err != nil {
		return err
	}
	// Reflect the defaulted value back onto the caller's struct, mirroring
	// TaskInstanceRepository.Insert's identical Kind-defaulting contract.
	rt.PhotoPolicy = photoPolicy
	return nil
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
//
// NES-116: when task.Cadence.Freq is household.FreqAsNeeded, a single standing
// task_instance is materialised in the same transaction, so an as-needed task
// never exists without its one open standing instance.
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

	if task.Cadence.Freq == household.FreqAsNeeded {
		if err := insertStandingInstance(ctx, tx, task.ID, task.HouseholdID); err != nil {
			return fmt.Errorf("create recurring task with rotation: insert standing instance: %w", err)
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
		       points, lead_time_days, active, created_at, updated_at, photo_policy
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
		       points, lead_time_days, active, created_at, updated_at, photo_policy
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
		       points, lead_time_days, active, created_at, updated_at, photo_policy
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
		photoPolicy                                     string
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
		&photoPolicy,
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
	pp, err := domain.ParsePhotoPolicy(photoPolicy)
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
	rt.PhotoPolicy = pp
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

// insertStandingInstance materialises a fresh, unassigned, pending standing
// instance for taskID via q (either the pool or an open transaction). Every
// call site that must (re)create an as-needed task's one open standing
// instance — RecurringTaskRepository.CreateWithRotation on task creation, and
// TaskInstanceRepository.Complete, CompleteAndAward, and Skip on every
// terminal transition — shares this single implementation (NES-116).
//
// The task_instance_standing_open_uniq partial unique index (recurring_task_id
// WHERE kind='standing' AND status='pending') is the schema-level backstop: a
// bug that tried to respawn twice for the same task would fail loudly here
// rather than silently leaving two open standing instances.
func insertStandingInstance(
	ctx context.Context,
	q db.TX,
	taskID domain.RecurringTaskID,
	householdID household.HouseholdID,
) error {
	repo := NewTaskInstanceRepository(q)
	inst := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: taskID,
		HouseholdID:     householdID,
		Status:          domain.StatusPending,
		Kind:            domain.KindStanding,
	}
	return repo.Insert(ctx, inst)
}

// respawnIfStanding materialises a fresh standing instance for
// recurringTaskIDStr when kindStr is domain.KindStanding, and is a no-op
// otherwise. It is the shared post-transition step for Complete,
// CompleteAndAward, and Skip: whichever terminal transition a standing
// instance goes through (done or skipped), the task must have a fresh open
// standing instance again in the same transaction (NES-116).
func respawnIfStanding(
	ctx context.Context,
	tx pgx.Tx,
	kindStr string,
	recurringTaskIDStr string,
	householdID household.HouseholdID,
) error {
	if kindStr != domain.KindStanding.String() {
		return nil
	}
	recurringTaskID, err := domain.ParseRecurringTaskID(recurringTaskIDStr)
	if err != nil {
		return fmt.Errorf("parse recurring task id: %w", err)
	}
	return insertStandingInstance(ctx, tx, recurringTaskID, householdID)
}

// beginTx opens a transaction on dbtx, which must support it (either a
// *pgxpool.Pool or an already-open pgx.Tx — see db.TX). op labels the returned
// error with the calling method's name. Shared by every TaskInstanceRepository
// method that must transition a row's status and, when it was a standing
// instance, respawn its replacement atomically (NES-116).
func beginTx(ctx context.Context, dbtx db.TX, op string) (pgx.Tx, error) {
	beginner, ok := dbtx.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return nil, fmt.Errorf("%s: executor does not support transactions", op)
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: begin: %w", op, err)
	}
	return tx, nil
}

// Insert persists a new task instance. The caller must populate ID,
// RecurringTaskID, HouseholdID, Status, and optionally AssigneeID; the store
// populates CreatedAt and UpdatedAt.
//
// Kind and DueOn (NES-116): the zero value of inst.Kind is treated as
// domain.KindScheduled for callers that predate NES-116. A domain.KindScheduled
// instance must have a non-nil DueOn; a domain.KindStanding instance must have
// a nil DueOn — validateInstanceKindDueOn rejects the mismatched
// combination before it reaches the database's own
// task_instance_standing_no_due_on CHECK constraint, giving a clearer error.
//
// Returns domain.ErrDuplicateInstance on a (recurring_task_id, due_on)
// conflict (constraint task_instance_task_due_uniq). A NULL due_on (standing
// instances) is never considered a duplicate of another NULL due_on, so a
// task's completed standing instances accumulate as distinct history rows.
func (r *TaskInstanceRepository) Insert(ctx context.Context, inst *domain.TaskInstance) error {
	if inst == nil {
		return errors.New("adapter: insert task instance: nil instance")
	}
	kind := inst.Kind
	if kind == "" {
		kind = domain.KindScheduled
	}
	if err := validateInstanceKindDueOn(kind, inst.DueOn); err != nil {
		return fmt.Errorf("insert task instance: %w", err)
	}

	var dueOn *time.Time
	if inst.DueOn != nil {
		d := domain.DateOf(*inst.DueOn)
		dueOn = &d
	}

	var assigneeIDStr *string
	if inst.AssigneeID != nil {
		s := inst.AssigneeID.String()
		assigneeIDStr = &s
	}

	const q = `
		INSERT INTO task_instance
			(id, household_id, recurring_task_id, assignee_id, due_on, status, kind)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at, updated_at`
	err := r.dbtx.QueryRow(ctx, q,
		inst.ID.String(),
		inst.HouseholdID.String(),
		inst.RecurringTaskID.String(),
		assigneeIDStr,
		dueOn,
		inst.Status.String(),
		kind.String(),
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
	inst.Kind = kind
	return nil
}

// validateInstanceKindDueOn enforces, ahead of the database's
// task_instance_standing_no_due_on CHECK constraint, that a scheduled instance
// carries a due date and a standing instance does not (NES-116).
func validateInstanceKindDueOn(kind domain.InstanceKind, dueOn *time.Time) error {
	switch kind {
	case domain.KindScheduled:
		if dueOn == nil {
			return errors.New("a scheduled instance requires a non-nil DueOn")
		}
	case domain.KindStanding:
		if dueOn != nil {
			return errors.New("a standing instance must have a nil DueOn")
		}
	default:
		return fmt.Errorf("unknown instance kind %q", kind)
	}
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
		       created_at, updated_at, kind,
		       claimed_by, claimed_at, claim_expires_at, claim_warned_at
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

// ListByHousehold returns kind='scheduled' instances for the household
// filtered by status and due date range [from, to] (inclusive), ordered by
// due_on. Standing instances are excluded explicitly (rather than relying on
// their NULL due_on failing the BETWEEN predicate) so the query's intent does
// not depend on that SQL NULL-comparison subtlety. Returns an empty slice when
// no instances match.
func (r *TaskInstanceRepository) ListByHousehold(
	ctx context.Context,
	householdID household.HouseholdID,
	status domain.InstanceStatus,
	from, to time.Time,
) ([]*domain.TaskInstance, error) {
	const q = `
		SELECT id, household_id, recurring_task_id, assignee_id,
		       due_on, status, completed_at, completed_by,
		       created_at, updated_at, kind,
		       claimed_by, claimed_at, claim_expires_at, claim_warned_at
		  FROM task_instance
		 WHERE household_id = $1
		   AND status = $2
		   AND kind = 'scheduled'
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

// ListStanding returns every pending kind='standing' instance for the
// household — each is the single open occurrence of an as-needed recurring
// task (NES-116). Ordered by created_at for a stable display order. Returns an
// empty slice (not an error) when none exist.
func (r *TaskInstanceRepository) ListStanding(
	ctx context.Context,
	householdID household.HouseholdID,
) ([]*domain.TaskInstance, error) {
	const q = `
		SELECT id, household_id, recurring_task_id, assignee_id,
		       due_on, status, completed_at, completed_by,
		       created_at, updated_at, kind,
		       claimed_by, claimed_at, claim_expires_at, claim_warned_at
		  FROM task_instance
		 WHERE household_id = $1
		   AND status = 'pending'
		   AND kind = 'standing'
		 ORDER BY created_at`
	rows, err := r.dbtx.Query(ctx, q, householdID.String())
	if err != nil {
		return nil, fmt.Errorf("list standing task instances: %w", err)
	}
	defer rows.Close()

	instances := make([]*domain.TaskInstance, 0)
	for rows.Next() {
		inst, err := scanTaskInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("list standing task instances: scan: %w", err)
		}
		instances = append(instances, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list standing task instances: %w", err)
	}
	return instances, nil
}

// ListTradeableAssignedToOthers returns every instance assigned to a member
// OTHER than excludeMemberID that satisfies domain.IsInstanceTradeable
// (NES-122): status = pending, kind = scheduled, claimed_by IS NULL, and
// due_on IS NOT NULL. The predicate mirrors IsInstanceTradeable's Go-level
// check exactly (see that function's doc) rather than duplicating its rules
// independently — a future change to tradeability must be made in exactly
// one place (the Go function) and mirrored here.
func (r *TaskInstanceRepository) ListTradeableAssignedToOthers(
	ctx context.Context,
	householdID household.HouseholdID,
	excludeMemberID household.MemberID,
) ([]*domain.TaskInstance, error) {
	const q = `
		SELECT id, household_id, recurring_task_id, assignee_id,
		       due_on, status, completed_at, completed_by,
		       created_at, updated_at, kind,
		       claimed_by, claimed_at, claim_expires_at, claim_warned_at
		  FROM task_instance
		 WHERE household_id = $1
		   AND status = 'pending'
		   AND kind = 'scheduled'
		   AND claimed_by IS NULL
		   AND due_on IS NOT NULL
		   AND assignee_id IS NOT NULL
		   AND assignee_id <> $2
		 ORDER BY due_on`
	rows, err := r.dbtx.Query(ctx, q, householdID.String(), excludeMemberID.String())
	if err != nil {
		return nil, fmt.Errorf("list tradeable task instances assigned to others: %w", err)
	}
	defer rows.Close()

	instances := make([]*domain.TaskInstance, 0)
	for rows.Next() {
		inst, err := scanTaskInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("list tradeable task instances assigned to others: scan: %w", err)
		}
		instances = append(instances, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tradeable task instances assigned to others: %w", err)
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
// either currently unassigned or already assigned to assignee (a self-claim).
// An overdue chore is still claimable — it can be picked up late. On 0 rows
// affected, the instance is read to produce the precise sentinel:
// domain.ErrInstanceNotFound, domain.ErrInstanceInTerminalState
// (done/skipped), or domain.ErrInstanceAlreadyClaimed (assigned to someone
// else).
//
// NES-117: every CASE expression reads the row's PRE-UPDATE values — standard
// SQL evaluates every expression in a single UPDATE's SET list against the
// row as it existed before the statement, so this is correct even though the
// same columns are simultaneously being written. Three cases:
//   - claimed_by (pre-update) already equals assignee — an active claim by
//     this same member already exists (whether still ticking or, for a
//     rotation instance, permanently risk-free). claimed_at and
//     claim_expires_at are left UNCHANGED. This is what stops a member from
//     calling Claim repeatedly on their own active claim to keep resetting
//     (and thereby evading) the expiry timer — a call that only re-asserts an
//     existing claim must never extend or clear it.
//   - Otherwise, assignee_id (pre-update) was NULL — the instance was not
//     originally assigned to anyone, so this is a new at-risk claim:
//     claimed_at is stamped now and claim_expires_at is set
//     [domain.ClaimWindow] out.
//   - Otherwise, assignee_id (pre-update) already equalled assignee (a
//     fixed/round-robin instance's own assignee claiming it for the first
//     time) — claimed_at is stamped now but claim_expires_at is left NULL:
//     no risk, since the chore was already assignee's responsibility.
//
// NES-118: claim_warned_at follows the same re-assert/reset split as
// claim_expires_at — UNCHANGED when claimed_by (pre-update) already equals
// assignee (a re-assertion must not let a member keep resetting their own
// warning status any more than it may reset the expiry itself), and NULL in
// both other branches (a genuinely new claim window, or a no-risk self-claim,
// starts with no warning sent).
func (r *TaskInstanceRepository) Claim(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
	assignee household.MemberID,
) error {
	const q = `
		UPDATE task_instance
		   SET assignee_id      = $3,
		       claimed_by       = $3,
		       claimed_at       = CASE
		                              WHEN claimed_by = $3 THEN claimed_at
		                              ELSE now()
		                          END,
		       claim_expires_at = CASE
		                              WHEN claimed_by = $3 THEN claim_expires_at
		                              WHEN assignee_id IS NULL
		                              THEN now() + make_interval(hours => $4)
		                              ELSE NULL
		                          END,
		       claim_warned_at  = CASE
		                              WHEN claimed_by = $3 THEN claim_warned_at
		                              ELSE NULL
		                          END,
		       updated_at       = now()
		 WHERE id           = $1
		   AND household_id = $2
		   AND status        IN ('pending', 'overdue')
		   AND (assignee_id IS NULL OR assignee_id = $3)`
	tag, err := r.dbtx.Exec(ctx, q,
		id.String(),
		householdID.String(),
		assignee.String(),
		int(domain.ClaimWindow.Hours()),
	)
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
//
// NES-116: this method does not award points (use CompleteAndAward for the
// user-facing completion flow), but it still transitions a standing instance
// to done, so it respawns a fresh standing instance for the same recurring
// task in the same transaction — the "always exactly one open standing
// instance" invariant must hold regardless of which method completed it.
//
// NES-117/NES-118: claimed_by/claimed_at/claim_expires_at/claim_warned_at
// are cleared in the same UPDATE. They are "current claim" fields per
// entities.go's contract, and a done instance has no current claim; leaving
// them set would also let a completed instance's stale claim_expires_at
// linger in task_instance_claim_expires_idx until some later sweep happened
// to notice the status no longer matches.
func (r *TaskInstanceRepository) Complete(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
	by household.MemberID,
	at time.Time,
) error {
	tx, err := beginTx(ctx, r.dbtx, "complete task instance")
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `
		UPDATE task_instance
		   SET status           = 'done',
		       completed_by     = $3,
		       completed_at     = $4,
		       claimed_by       = NULL,
		       claimed_at       = NULL,
		       claim_expires_at = NULL,
		       claim_warned_at  = NULL,
		       updated_at       = now()
		 WHERE id           = $1
		   AND household_id = $2
		   AND status       IN ('pending', 'overdue')
		RETURNING recurring_task_id, kind`

	var recurringTaskIDStr, kindStr string
	err = tx.QueryRow(ctx, q, id.String(), householdID.String(), by.String(), at).
		Scan(&recurringTaskIDStr, &kindStr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r.disambiguateTerminal(ctx, tx, householdID, id)
		}
		return fmt.Errorf("complete task instance: %w", err)
	}

	if err := respawnIfStanding(ctx, tx, kindStr, recurringTaskIDStr, householdID); err != nil {
		return fmt.Errorf("complete task instance: respawn standing instance: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("complete task instance: commit: %w", err)
	}
	return nil
}

// CompleteAndAward atomically transitions the instance from pending or overdue
// to done and appends a point_ledger credit for the completing member, all
// within one database transaction. The approach mirrors CreateWithRotation:
// open a tx, run the instance UPDATE RETURNING recurring_task_id, then
// conditionally INSERT the ledger row with ON CONFLICT DO NOTHING. This
// single-adapter-tx style is the simplest way to guarantee atomicity without
// threading a transaction handle through the service layer.
//
// Idempotency: the ON CONFLICT on the partial unique index
// (point_ledger_task_completion_uniq) is a belt-and-suspenders guard. The
// status predicate IN ('pending','overdue') in the UPDATE already prevents
// re-completion; the ON CONFLICT ensures that a manual or legacy duplicate
// cannot produce a second ledger row either.
//
// Points = 0 optimization: the ledger INSERT is skipped entirely when
// recurring_task.points = 0, keeping the ledger free of zero-value noise.
//
// NES-116: when the completed instance's kind is 'standing', the final step
// inserts a fresh pending standing instance for the same recurring task in the
// same transaction, so an as-needed task always has exactly one open standing
// instance again immediately after completion.
//
// NES-117/NES-118: claimed_by/claimed_at/claim_expires_at/claim_warned_at
// are cleared in the same UPDATE as the status transition — see Complete's
// doc for why a done instance must not keep "current claim" metadata set.
func (r *TaskInstanceRepository) CompleteAndAward(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
	by household.MemberID,
	at time.Time,
) error {
	tx, err := beginTx(ctx, r.dbtx, "complete and award")
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Step 1: mark instance done and return the parent recurring_task_id and
	// kind so we can look up the points value in step 2, and respawn a standing
	// instance in step 3, without separate queries. The status predicate is the
	// guard that resolves the double-completion race: a losing concurrent call
	// matches zero rows here (the winner's commit already flipped status to
	// done) and falls through to disambiguateTerminal below.
	const updateQ = `
		UPDATE task_instance
		   SET status           = 'done',
		       completed_by     = $3,
		       completed_at     = $4,
		       claimed_by       = NULL,
		       claimed_at       = NULL,
		       claim_expires_at = NULL,
		       claim_warned_at  = NULL,
		       updated_at       = now()
		 WHERE id           = $1
		   AND household_id = $2
		   AND status       IN ('pending', 'overdue')
		RETURNING recurring_task_id, kind`

	var recurringTaskIDStr, kindStr string
	err = tx.QueryRow(ctx, updateQ, id.String(), householdID.String(), by.String(), at).
		Scan(&recurringTaskIDStr, &kindStr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r.disambiguateTerminal(ctx, tx, householdID, id)
		}
		return fmt.Errorf("complete and award: update instance: %w", err)
	}

	// Step 2: insert the point ledger row. The SELECT ... FROM recurring_task
	// fetches the points value; the ON CONFLICT clause makes a duplicate a
	// silent no-op. We skip the INSERT entirely when points = 0 to avoid
	// zero-value ledger noise.
	// The ledger id is generated application-side as a UUIDv7, matching the
	// rest of the codebase (do not use gen_random_uuid()/v4 in SQL).
	const awardQ = `
		INSERT INTO point_ledger
			(id, household_id, member_id, source_type, source_id, points, created_at)
		SELECT $1::uuid, $2, $3, 'task_instance', $4::uuid, rt.points, now()
		  FROM recurring_task rt
		 WHERE rt.id     = $5::uuid
		   AND rt.household_id = $2
		   AND rt.points > 0
		ON CONFLICT (source_id) WHERE source_type = 'task_instance'
		DO NOTHING`

	if _, err := tx.Exec(ctx, awardQ,
		domain.NewPointEntryID().String(),
		householdID.String(),
		by.String(),
		id.String(),
		recurringTaskIDStr,
	); err != nil {
		return fmt.Errorf("complete and award: insert ledger: %w", err)
	}

	// Step 3: an as-needed task's standing instance reappears immediately after
	// completion, in the same transaction as the completion itself.
	if err := respawnIfStanding(ctx, tx, kindStr, recurringTaskIDStr, householdID); err != nil {
		return fmt.Errorf("complete and award: respawn standing instance: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("complete and award: commit: %w", err)
	}
	return nil
}

// Skip transitions the instance from pending or overdue to skipped. An overdue
// chore is still actionable — it can be skipped late. On 0 rows affected, the
// instance is read to produce the precise sentinel: domain.ErrInstanceNotFound
// or domain.ErrInstanceInTerminalState (done/skipped).
//
// NES-116: skipping a standing instance releases it back to the pool — a
// fresh, unassigned standing instance for the same recurring task is
// materialised in the same transaction, so the "always exactly one open
// standing instance" invariant holds on the skip path exactly as it does on
// completion.
//
// NES-117/NES-118: claimed_by/claimed_at/claim_expires_at/claim_warned_at
// are cleared in the same UPDATE — see Complete's doc for why a terminal
// instance must not keep "current claim" metadata set. This is distinct
// from (and simpler than) SweepExpiredClaims' revert: Skip does not touch
// assignee_id, since a skipped instance's assignee (whoever it was) is not
// being released back to the pool the way an expiry reverts one — it is
// simply no longer actionable.
func (r *TaskInstanceRepository) Skip(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
) error {
	tx, err := beginTx(ctx, r.dbtx, "skip task instance")
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `
		UPDATE task_instance
		   SET status           = 'skipped',
		       claimed_by       = NULL,
		       claimed_at       = NULL,
		       claim_expires_at = NULL,
		       claim_warned_at  = NULL,
		       updated_at       = now()
		 WHERE id           = $1
		   AND household_id = $2
		   AND status       IN ('pending', 'overdue')
		RETURNING recurring_task_id, kind`

	var recurringTaskIDStr, kindStr string
	err = tx.QueryRow(ctx, q, id.String(), householdID.String()).
		Scan(&recurringTaskIDStr, &kindStr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r.disambiguateTerminal(ctx, tx, householdID, id)
		}
		return fmt.Errorf("skip task instance: %w", err)
	}

	if err := respawnIfStanding(ctx, tx, kindStr, recurringTaskIDStr, householdID); err != nil {
		return fmt.Errorf("skip task instance: respawn standing instance: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("skip task instance: commit: %w", err)
	}
	return nil
}

// disambiguateTerminal reads the instance to determine why a Complete or Skip
// UPDATE matched zero rows, returning domain.ErrInstanceNotFound when the row
// does not exist in the household or domain.ErrInstanceInTerminalState when it
// is already done or skipped. Because Complete and Skip now act on both pending
// and overdue rows, a zero-row update for an existing instance means the row is
// in a terminal state (done or skipped).
//
// q is the caller's open transaction: reading through it keeps the check on
// the connection (and snapshot) the caller already holds instead of borrowing
// a second pool connection while the first is still checked out, which could
// stall under pool pressure.
func (r *TaskInstanceRepository) disambiguateTerminal(
	ctx context.Context,
	q rowQuerier,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
) error {
	const query = `
		SELECT status
		  FROM task_instance
		 WHERE id = $1
		   AND household_id = $2`
	var status string
	err := q.QueryRow(ctx, query, id.String(), householdID.String()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrInstanceNotFound
	}
	if err != nil {
		return fmt.Errorf("disambiguate terminal task instance: %w", err)
	}
	return domain.ErrInstanceInTerminalState
}

// rowQuerier is the single-row read seam disambiguateTerminal needs; both
// pgx.Tx and the pool-backed db.TX satisfy it.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// MarkPendingOverdue bulk-transitions all pending, kind='scheduled' instances
// for the household whose due_on < asOf to overdue. kind='scheduled' is
// explicit rather than relied upon implicitly via the NULL due_on on standing
// instances failing the < comparison, so the query's intent is not hidden
// behind a SQL NULL-comparison subtlety. Returns the number of rows updated.
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
		   AND kind         = 'scheduled'
		   AND due_on       < $2`
	tag, err := r.dbtx.Exec(ctx, q, householdID.String(), domain.DateOf(asOf))
	if err != nil {
		return 0, fmt.Errorf("mark pending overdue: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// MarkPendingOverdueAll bulk-transitions all pending, kind='scheduled'
// instances across ALL households whose due_on < asOf to overdue. Standing
// instances (kind='standing') are excluded explicitly — they have no due date
// and so can never be overdue. It returns the newly-overdue rows as
// [domain.ReminderTarget] values so the caller can enqueue overdue
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
		   AND kind   = 'scheduled'
		   AND due_on < $1
		RETURNING id, household_id, assignee_id, due_on, recurring_task_id`

	rows, err := r.dbtx.Query(ctx, updateQ, domain.DateOf(asOf))
	if err != nil {
		return nil, fmt.Errorf("mark pending overdue all: %w", err)
	}
	defer rows.Close()

	return scanReminderRows(ctx, rows, domain.ReminderOverdue, "mark pending overdue all", r)
}

// ClaimDueSoonReminders atomically selects pending, kind='scheduled' instances
// inside the closed due-soon window (asOf <= due_on <= asOf + lead_time_days)
// that have not yet been reminded (reminded_at IS NULL), stamps reminded_at =
// now() on each, and returns them as [domain.ReminderTarget] values
// (Kind=[domain.ReminderDueSoon]). Standing instances (kind='standing') never
// receive a due-soon reminder: they have no due date to enter the window with.
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
	//
	// The title/category enrichment rides the SAME statement as the mark
	// (NES-146, mirroring ClaimWarnings' NES-118 fix): the due CTE already
	// joins recurring_task for lead_time_days, so it carries rt.title and
	// rt.category through to RETURNING. A separate follow-up title query —
	// the previous shape — had a silent-loss failure mode: if it failed
	// after the mark committed, the rows stayed reminded_at-stamped, were
	// never reselected (reminded_at IS NULL), and their reminders were
	// permanently lost. One statement means the mark and its notification
	// content are returned together or nothing is marked at all.
	const q = `
		WITH due AS (
			SELECT ti.id, rt.title, rt.category
			  FROM task_instance ti
			  JOIN recurring_task rt ON rt.id = ti.recurring_task_id
			 WHERE ti.status       = 'pending'
			   AND ti.kind         = 'scheduled'
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
		          due.title, due.category`

	rows, err := r.dbtx.Query(ctx, q, domain.DateOf(asOf))
	if err != nil {
		return nil, fmt.Errorf("claim due-soon reminders: %w", err)
	}
	defer rows.Close()

	targets := make([]domain.ReminderTarget, 0)
	for rows.Next() {
		var (
			instStr, hhStr, title, categoryStr string
			assigneeIDStr                      *string
			dueOn                              time.Time
		)
		if err := rows.Scan(&instStr, &hhStr, &assigneeIDStr, &dueOn, &title, &categoryStr); err != nil {
			return nil, fmt.Errorf("claim due-soon reminders: scan: %w", err)
		}
		instID, err := domain.ParseTaskInstanceID(instStr)
		if err != nil {
			return nil, fmt.Errorf("claim due-soon reminders: parse instance id: %w", err)
		}
		hhID, err := household.ParseHouseholdID(hhStr)
		if err != nil {
			return nil, fmt.Errorf("claim due-soon reminders: parse household id: %w", err)
		}
		cat, err := domain.ParseCategory(categoryStr)
		if err != nil {
			return nil, fmt.Errorf("claim due-soon reminders: parse category: %w", err)
		}
		target := domain.ReminderTarget{
			InstanceID:  instID,
			HouseholdID: hhID,
			Title:       title,
			Category:    cat,
			DueOn:       domain.DateOf(dueOn),
			Kind:        domain.ReminderDueSoon,
		}
		if assigneeIDStr != nil {
			memberID, err := household.ParseMemberID(*assigneeIDStr)
			if err != nil {
				return nil, fmt.Errorf("claim due-soon reminders: parse assignee id: %w", err)
			}
			target.AssigneeID = &memberID
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim due-soon reminders: %w", err)
	}
	return targets, nil
}

// ClaimWarnings atomically selects every claim entering its warning window
// (claim_expires_at within domain.ClaimWarningWindow of asOf, and not yet
// expired) that has not yet been warned (claim_warned_at IS NULL), marks
// claim_warned_at = now() on each, and joins the parent recurring task's
// title — all in a single SQL statement (NES-118).
//
// The selection, the claim_warned_at UPDATE, and the title lookup are
// chained CTEs feeding one final SELECT, rather than a separate follow-up
// query for the title the way scanReminderRows resolves titles for
// MarkPendingOverdueAll (ClaimDueSoonReminders was moved to this same
// single-statement shape by NES-146). That two-step shape has a
// latent bug this method must not repeat: if a follow-up title query failed
// after the mark had already committed, the row would come back
// claim_warned_at-stamped but never reach the caller — since a stamped row
// is never selected again, the warning would be silently and permanently
// lost. Folding everything into one statement makes the whole operation
// atomic: either the mark and its title are both returned, or nothing is
// marked at all.
//
// A CTE with SELECT ... FOR UPDATE SKIP LOCKED + UPDATE mirrors
// ClaimDueSoonReminders' claim-and-mark shape: because claim_warned_at is
// set in the same statement as the selection, a row can only ever be
// returned once, and two concurrent calls never warn the same claim twice.
//
// The lower bound (claim_expires_at > asOf) excludes already-expired claims
// — those belong to SweepExpiredClaims instead. claimed_by IS NOT NULL
// excludes an orphaned claim (claimant's member row deleted before expiry):
// there is no one to warn.
//
// WARNING: this method is intentionally NOT household-scoped. It is a
// system-process method reserved for the background scheduler and must not
// be called from user-facing request handlers, matching the precedent set by
// SweepExpiredClaims and ClaimDueSoonReminders.
func (r *TaskInstanceRepository) ClaimWarnings(ctx context.Context, asOf time.Time) ([]domain.ClaimWarning, error) {
	const q = `
		WITH candidate AS (
			SELECT ti.id, ti.household_id, ti.recurring_task_id,
			       ti.claimed_by, ti.claim_expires_at
			  FROM task_instance ti
			 WHERE ti.claim_expires_at IS NOT NULL
			   AND ti.claim_warned_at  IS NULL
			   AND ti.claim_expires_at >  $1
			   AND ti.claim_expires_at <= $1 + make_interval(hours => $2)
			   AND ti.status IN ('pending', 'overdue')
			   AND ti.claimed_by IS NOT NULL
			   FOR UPDATE OF ti SKIP LOCKED
		),
		marked AS (
			UPDATE task_instance ti
			   SET claim_warned_at = now()
			  FROM candidate
			 WHERE ti.id = candidate.id
			RETURNING ti.id, candidate.household_id, candidate.recurring_task_id,
			          candidate.claimed_by, candidate.claim_expires_at
		)
		SELECT marked.id, marked.household_id, marked.claimed_by,
		       marked.claim_expires_at, COALESCE(rt.title, '')
		  FROM marked
		  LEFT JOIN recurring_task rt ON rt.id = marked.recurring_task_id`

	rows, err := r.dbtx.Query(ctx, q, asOf, int(domain.ClaimWarningWindow.Hours()))
	if err != nil {
		return nil, fmt.Errorf("claim warnings: %w", err)
	}
	defer rows.Close()

	warnings := make([]domain.ClaimWarning, 0)
	for rows.Next() {
		var instStr, hhStr, claimedByStr, title string
		var expiresAt time.Time
		if err := rows.Scan(&instStr, &hhStr, &claimedByStr, &expiresAt, &title); err != nil {
			return nil, fmt.Errorf("claim warnings: scan: %w", err)
		}
		instID, err := domain.ParseTaskInstanceID(instStr)
		if err != nil {
			return nil, fmt.Errorf("claim warnings: parse instance id: %w", err)
		}
		hhID, err := household.ParseHouseholdID(hhStr)
		if err != nil {
			return nil, fmt.Errorf("claim warnings: parse household id: %w", err)
		}
		claimedBy, err := household.ParseMemberID(claimedByStr)
		if err != nil {
			return nil, fmt.Errorf("claim warnings: parse claimed_by id: %w", err)
		}
		warnings = append(warnings, domain.ClaimWarning{
			InstanceID:  instID,
			HouseholdID: hhID,
			ClaimedBy:   claimedBy,
			Title:       title,
			ExpiresAt:   expiresAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim warnings: %w", err)
	}
	return warnings, nil
}

// ClearClaimWarning resets claim_warned_at to NULL for id's claim, scoped to
// the specific claim window identified by expiresAt (NES-118). It is the
// recovery counterpart to ClaimWarnings, mirroring ClearDueSoonReminder: the
// caller invokes it when a warning notification fails to enqueue after the
// row was marked warned, so the warning is retried on a later tick instead
// of being lost.
//
// The claim_expires_at = expiresAt guard scopes the reset to the SAME claim
// window the warning was generated for. Between the mark and this recovery
// call, the instance may have moved on to a different claim window entirely
// (completed, skipped, or swept-and-reclaimed) — all legitimate, unblocked
// transitions. Without the guard, a blind reset by id alone could clear
// claim_warned_at for that new, unrelated window, causing a spurious
// duplicate warning the next time ClaimWarnings runs.
//
// It is a no-op (nil error) when id is unknown or its claim_expires_at no
// longer matches expiresAt — recovery must be idempotent and tolerant of the
// row having moved on before the clear runs.
//
// WARNING: this method is intentionally NOT household-scoped. It is a
// system-process recovery method reserved for the background scheduler and
// must not be called from user-facing request handlers.
func (r *TaskInstanceRepository) ClearClaimWarning(ctx context.Context, id domain.TaskInstanceID, expiresAt time.Time) error {
	const q = `
		UPDATE task_instance
		   SET claim_warned_at = NULL
		 WHERE id               = $1
		   AND claim_expires_at = $2`
	if _, err := r.dbtx.Exec(ctx, q, id.String(), expiresAt); err != nil {
		return fmt.Errorf("clear claim warning: %w", err)
	}
	return nil
}

// CompletionDays returns the distinct calendar days (midnight UTC) on which
// member completed at least one task within the household, restricted to rows
// with completed_at >= since. Results are ordered ascending.
// Returns an empty slice (not an error) when no completions match.
//
// completed_at is a timestamptz, so a plain ::date cast would bucket using the
// session TimeZone and mis-attribute completions to the wrong calendar day on a
// non-UTC server. The query therefore converts to UTC first with
// (completed_at AT TIME ZONE 'UTC')::date, pinning the day boundary to UTC per
// the NES-37 streak rule. The >= since cutoff is a point-in-time timestamptz
// comparison and is timezone-independent. DateOf is applied on every scanned
// value for defence-in-depth so the returned values are always midnight UTC.
func (r *TaskInstanceRepository) CompletionDays(
	ctx context.Context,
	householdID household.HouseholdID,
	memberID household.MemberID,
	since time.Time,
) ([]time.Time, error) {
	const q = `
		SELECT DISTINCT (completed_at AT TIME ZONE 'UTC')::date
		  FROM task_instance
		 WHERE household_id  = $1
		   AND completed_by  = $2
		   AND status        = 'done'
		   AND completed_at >= $3
		 ORDER BY 1`
	rows, err := r.dbtx.Query(ctx, q,
		householdID.String(),
		memberID.String(),
		since.UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("completion days: %w", err)
	}
	defer rows.Close()

	days := make([]time.Time, 0)
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("completion days: scan: %w", err)
		}
		days = append(days, domain.DateOf(d))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("completion days: %w", err)
	}
	return days, nil
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

// expiredClaimRow is the intermediate representation for a single row
// returned by the revert step of SweepExpiredClaims, before recurring_task
// metadata (title, points) is joined in memory.
type expiredClaimRow struct {
	instanceID      domain.TaskInstanceID
	householdID     household.HouseholdID
	recurringTaskID domain.RecurringTaskID
	claimedBy       household.MemberID
	claimedAt       time.Time
}

// SweepExpiredClaims atomically reverts every claim whose claim_expires_at is
// at or before asOf and whose instance is still pending or overdue, then
// appends a point_ledger penalty entry for each claimant. Both the revert and
// every penalty INSERT happen inside a single transaction so a claimant's
// balance always reflects an actually-reverted claim (NES-117).
//
// A CTE selects candidate rows with FOR UPDATE OF ti SKIP LOCKED (mirroring
// ClaimDueSoonReminders) so two concurrent sweeps never process the same row
// twice; the UPDATE then reverts them in one statement. The revert always
// clears assignee_id to NULL: a row only ever reaches this query when its
// claim carried a real expiry, which — per Claim's contract — only happens
// when the pre-claim assignee_id was NULL (an originally-unassigned
// claimable or standing instance). A fixed/round-robin instance's own
// assignee "claiming" it never sets an expiry, so it never appears here and
// its assignee_id is never touched — "reverting to the original assignee"
// is therefore automatic: that assignee_id never moved in the first place.
//
// The penalty for each claimant is computed in Go via
// [domain.ClaimExpiryPenalty] (the single source of truth for the formula)
// and inserted with ON CONFLICT DO NOTHING against the partial unique index
// point_ledger_claim_expiry_uniq (source_id, claim_started_at) WHERE
// source_type = 'claim_expiry': belt-and-suspenders idempotency alongside the
// SKIP LOCKED guard, keyed on the specific claim window (instance id +
// claimed_at) so a later, independent claim on the same instance is
// penalized again if it also expires.
//
// Orphaned claims: if the claimant's member row was deleted before expiry,
// ON DELETE SET NULL (claimed_by) has already nulled claimed_by while
// claimed_at/claim_expires_at survive (task_instance_claim_consistency is
// directional to allow exactly this). revertExpiredClaims still reverts such
// a row — it is part of the same unconditional UPDATE as every other
// candidate — but skips it before penalty computation: there is no member to
// credit a penalty against, and point_ledger.member_id is NOT NULL, so an
// insert would fail outright even if a penalty were attempted.
func (r *TaskInstanceRepository) SweepExpiredClaims(ctx context.Context, asOf time.Time) ([]domain.ExpiredClaim, error) {
	tx, err := beginTx(ctx, r.dbtx, "sweep expired claims")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	reverted, err := revertExpiredClaims(ctx, tx, asOf)
	if err != nil {
		return nil, fmt.Errorf("sweep expired claims: %w", err)
	}
	if len(reverted) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("sweep expired claims: commit: %w", err)
		}
		return nil, nil
	}

	taskIDSet := make(map[domain.RecurringTaskID]bool, len(reverted))
	for _, row := range reverted {
		taskIDSet[row.recurringTaskID] = true
	}
	meta, err := fetchClaimTaskMeta(ctx, tx, taskIDSet)
	if err != nil {
		return nil, fmt.Errorf("sweep expired claims: fetch task meta: %w", err)
	}

	claims := make([]domain.ExpiredClaim, 0, len(reverted))
	for _, row := range reverted {
		m := meta[row.recurringTaskID]
		penalty := domain.ClaimExpiryPenalty(m.points)

		if err := insertClaimExpiryPenalty(ctx, tx, row, penalty); err != nil {
			return nil, fmt.Errorf("sweep expired claims: insert penalty: %w", err)
		}

		claims = append(claims, domain.ExpiredClaim{
			InstanceID:      row.instanceID,
			HouseholdID:     row.householdID,
			RecurringTaskID: row.recurringTaskID,
			ClaimedBy:       row.claimedBy,
			Title:           m.title,
			PenaltyPoints:   penalty,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("sweep expired claims: commit: %w", err)
	}
	return claims, nil
}

// revertExpiredClaims selects every claim expired as of asOf with FOR UPDATE
// SKIP LOCKED and reverts its claim fields (assignee_id, claimed_by,
// claimed_at, claim_expires_at, claim_warned_at all cleared) in one
// statement, returning the pre-revert claimant and parent task for each row
// so the caller can compute and insert the penalty. Clearing claim_warned_at
// here (NES-118) is not strictly required for correctness — Claim already
// resets it to NULL the next time the now-unclaimed instance is claimed —
// but keeps every "current claim" field consistent with the reverted state
// rather than leaving a stale warned_at to linger until the next claim.
func revertExpiredClaims(ctx context.Context, tx pgx.Tx, asOf time.Time) ([]expiredClaimRow, error) {
	const revertQ = `
		WITH expired AS (
			SELECT ti.id, ti.household_id, ti.recurring_task_id,
			       ti.claimed_by, ti.claimed_at
			  FROM task_instance ti
			 WHERE ti.claim_expires_at IS NOT NULL
			   AND ti.claim_expires_at <= $1
			   AND ti.status IN ('pending', 'overdue')
			   FOR UPDATE OF ti SKIP LOCKED
		)
		UPDATE task_instance ti
		   SET assignee_id      = NULL,
		       claimed_by       = NULL,
		       claimed_at       = NULL,
		       claim_expires_at = NULL,
		       claim_warned_at  = NULL,
		       updated_at       = now()
		  FROM expired
		 WHERE ti.id = expired.id
		RETURNING ti.id, expired.household_id, expired.recurring_task_id,
		          expired.claimed_by, expired.claimed_at`

	rows, err := tx.Query(ctx, revertQ, asOf)
	if err != nil {
		return nil, fmt.Errorf("revert: %w", err)
	}
	defer rows.Close()

	var reverted []expiredClaimRow
	for rows.Next() {
		var (
			instStr, hhStr, rtStr string
			claimedByStr          *string
			claimedAt             *time.Time
		)
		if err := rows.Scan(&instStr, &hhStr, &rtStr, &claimedByStr, &claimedAt); err != nil {
			return nil, fmt.Errorf("revert: scan: %w", err)
		}
		if claimedByStr == nil {
			// A legitimate, expected shape (NES-117): claimed_by is nulled by
			// ON DELETE SET NULL (claimed_by) when the claimant's member row is
			// deleted, while claimed_at/claim_expires_at deliberately survive
			// that (task_instance_claim_consistency is directional precisely to
			// allow it). The revert UPDATE above has already reverted this row
			// unconditionally regardless of this skip; there is simply no one
			// left to penalize or notify, and the point_ledger member FK would
			// reject an insert with a NULL member_id anyway.
			continue
		}
		if claimedAt == nil {
			// Defensive only: task_instance_claim_expiry_requires_claim ties
			// claim_expires_at to claimed_at (not claimed_by, which member
			// deletion can null independently — see above), so a row selected by
			// this query's "claim_expires_at IS NOT NULL" predicate should never
			// have a nil claimed_at. Skip rather than penalize with a
			// nonsensical claim window.
			continue
		}
		instID, err := domain.ParseTaskInstanceID(instStr)
		if err != nil {
			return nil, fmt.Errorf("revert: parse instance id: %w", err)
		}
		hhID, err := household.ParseHouseholdID(hhStr)
		if err != nil {
			return nil, fmt.Errorf("revert: parse household id: %w", err)
		}
		rtID, err := domain.ParseRecurringTaskID(rtStr)
		if err != nil {
			return nil, fmt.Errorf("revert: parse recurring task id: %w", err)
		}
		claimedBy, err := household.ParseMemberID(*claimedByStr)
		if err != nil {
			return nil, fmt.Errorf("revert: parse claimed_by id: %w", err)
		}
		reverted = append(reverted, expiredClaimRow{
			instanceID:      instID,
			householdID:     hhID,
			recurringTaskID: rtID,
			claimedBy:       claimedBy,
			claimedAt:       *claimedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("revert: %w", err)
	}
	return reverted, nil
}

// claimTaskMeta holds the recurring_task fields SweepExpiredClaims needs to
// compute and describe a penalty: points (for domain.ClaimExpiryPenalty) and
// title (for the resulting domain.ExpiredClaim's notification text).
type claimTaskMeta struct {
	title  string
	points int
}

// fetchClaimTaskMeta performs a single SELECT, scoped to the open transaction
// tx, to look up title and points for the recurring tasks identified by
// taskIDSet.
func fetchClaimTaskMeta(
	ctx context.Context,
	tx pgx.Tx,
	taskIDSet map[domain.RecurringTaskID]bool,
) (map[domain.RecurringTaskID]claimTaskMeta, error) {
	ids := make([]string, 0, len(taskIDSet))
	for id := range taskIDSet {
		ids = append(ids, id.String())
	}

	const q = `
		SELECT id, title, points
		  FROM recurring_task
		 WHERE id = ANY($1::uuid[])`
	rows, err := tx.Query(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("fetch claim task meta: %w", err)
	}
	defer rows.Close()

	result := make(map[domain.RecurringTaskID]claimTaskMeta, len(taskIDSet))
	for rows.Next() {
		var idStr, title string
		var points int
		if err := rows.Scan(&idStr, &title, &points); err != nil {
			return nil, fmt.Errorf("fetch claim task meta: scan: %w", err)
		}
		id, err := domain.ParseRecurringTaskID(idStr)
		if err != nil {
			return nil, fmt.Errorf("fetch claim task meta: parse id: %w", err)
		}
		result[id] = claimTaskMeta{title: title, points: points}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fetch claim task meta: %w", err)
	}
	return result, nil
}

// insertClaimExpiryPenalty appends a negative point_ledger entry of
// -penalty for row.claimedBy, keyed for idempotency by
// point_ledger_claim_expiry_uniq (source_id, claim_started_at) WHERE
// source_type = 'claim_expiry'. A conflict (the same claim window already
// penalized) is silently ignored — belt-and-suspenders alongside the SKIP
// LOCKED guard in revertExpiredClaims.
func insertClaimExpiryPenalty(ctx context.Context, tx pgx.Tx, row expiredClaimRow, penalty int) error {
	const q = `
		INSERT INTO point_ledger
			(id, household_id, member_id, source_type, source_id, points, claim_started_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (source_id, claim_started_at) WHERE source_type = 'claim_expiry'
		DO NOTHING`
	_, err := tx.Exec(ctx, q,
		domain.NewPointEntryID().String(),
		row.householdID.String(),
		row.claimedBy.String(),
		domain.SourceTypeClaimExpiry,
		row.instanceID.String(),
		-penalty,
		row.claimedAt,
	)
	if err != nil {
		return fmt.Errorf("insert claim expiry penalty: %w", err)
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
// (assignee_id, completed_at, completed_by, due_on, claimed_by, claimed_at,
// claim_expires_at, claim_warned_at) are read into pointer types and
// converted to domain pointer fields. DueOn is nil for a standing instance
// (NES-116); when present it is normalized with domain.DateOf.
func scanTaskInstance(r row) (*domain.TaskInstance, error) {
	var (
		inst                                                     domain.TaskInstance
		idStr, householdIDStr, recurringTaskIDStr, status, kindS string
		assigneeIDStr, completedByStr, claimedByStr              *string
		completedAt, dueOn, claimedAt, claimExpiresAt            *time.Time
		claimWarnedAt                                            *time.Time
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
		&kindS,
		&claimedByStr,
		&claimedAt,
		&claimExpiresAt,
		&claimWarnedAt,
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
	kind, err := domain.ParseInstanceKind(kindS)
	if err != nil {
		return nil, fmt.Errorf("scan task instance: %w", err)
	}

	inst.ID = id
	inst.HouseholdID = householdID
	inst.RecurringTaskID = recurringTaskID
	inst.Status = instanceStatus
	inst.Kind = kind
	if dueOn != nil {
		inst.DueOn = domain.DueOnPtr(*dueOn)
	}
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
	if claimedByStr != nil {
		memberID, err := household.ParseMemberID(*claimedByStr)
		if err != nil {
			return nil, fmt.Errorf("scan task instance: parse claimed_by id: %w", err)
		}
		inst.ClaimedBy = &memberID
	}
	inst.ClaimedAt = claimedAt
	inst.ClaimExpiresAt = claimExpiresAt
	inst.ClaimWarnedAt = claimWarnedAt
	return &inst, nil
}
