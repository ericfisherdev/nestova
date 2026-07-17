package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// Constraint name from 00007_gamification.sql that guards the task-completion
// idempotency invariant. Named here to avoid stringly-typed comparisons
// scattered across the adapter.
const constraintPointLedgerTaskCompletionUniq = "point_ledger_task_completion_uniq"

// PointLedgerPostgresRepository is the pgx-backed implementation of
// domain.PointLedgerRepository. UUIDs are passed and scanned as text so no pgx
// UUID codec registration is required.
type PointLedgerPostgresRepository struct {
	dbtx db.TX
}

// Compile-time assurance that PointLedgerPostgresRepository satisfies the port.
var _ domain.PointLedgerRepository = (*PointLedgerPostgresRepository)(nil)

// NewPointLedgerPostgresRepository constructs a PointLedgerPostgresRepository
// with an injected query executor. The executor is a db.TX, satisfied by both
// *pgxpool.Pool (the default composition) and pgx.Tx (so the repository can
// run inside a caller's transaction).
func NewPointLedgerPostgresRepository(dbtx db.TX) *PointLedgerPostgresRepository {
	if dbtx == nil {
		panic("adapter: NewPointLedgerPostgresRepository requires a non-nil db.TX")
	}
	return &PointLedgerPostgresRepository{dbtx: dbtx}
}

// Append inserts a new point entry into the ledger. Returns
// [domain.ErrDuplicatePointEntry] when the INSERT violates the
// point_ledger_task_completion_uniq partial unique index (source_type =
// 'task_instance', duplicate source_id).
func (r *PointLedgerPostgresRepository) Append(ctx context.Context, entry *domain.PointEntry) error {
	if entry == nil {
		return errors.New("adapter: append point entry: nil entry")
	}

	var sourceIDStr *string
	if entry.SourceID != nil {
		s := entry.SourceID.String()
		sourceIDStr = &s
	}

	const q = `
		INSERT INTO point_ledger
			(id, household_id, member_id, source_type, source_id, points, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := r.dbtx.Exec(ctx, q,
		entry.ID.String(),
		entry.HouseholdID.String(),
		entry.MemberID.String(),
		entry.SourceType,
		sourceIDStr,
		entry.Points,
		entry.CreatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) &&
			pgErr.Code == sqlstateUniqueViolation &&
			pgErr.ConstraintName == constraintPointLedgerTaskCompletionUniq {
			return domain.ErrDuplicatePointEntry
		}
		return fmt.Errorf("append point entry: %w", err)
	}
	return nil
}

// Balance returns the sum of all points for the member within the household.
// Returns 0, nil when no entries exist.
func (r *PointLedgerPostgresRepository) Balance(
	ctx context.Context,
	householdID household.HouseholdID,
	memberID household.MemberID,
) (int, error) {
	const q = `
		SELECT COALESCE(SUM(points), 0)
		  FROM point_ledger
		 WHERE household_id = $1
		   AND member_id    = $2`
	var total int
	if err := r.dbtx.QueryRow(ctx, q, householdID.String(), memberID.String()).Scan(&total); err != nil {
		return 0, fmt.Errorf("balance: %w", err)
	}
	return total, nil
}

// Leaderboard returns per-member point totals for the household, summing only
// entries created at or after since, ordered by total points descending then
// member_id ascending for a stable tie-break.
// Returns an empty slice (not an error) when no entries match.
func (r *PointLedgerPostgresRepository) Leaderboard(
	ctx context.Context,
	householdID household.HouseholdID,
	since time.Time,
) ([]domain.MemberPoints, error) {
	const q = `
		SELECT member_id, COALESCE(SUM(points), 0) AS total
		  FROM point_ledger
		 WHERE household_id = $1
		   AND created_at  >= $2
		 GROUP BY member_id
		 ORDER BY total DESC, member_id`
	rows, err := r.dbtx.Query(ctx, q, householdID.String(), since)
	if err != nil {
		return nil, fmt.Errorf("leaderboard: %w", err)
	}
	defer rows.Close()

	result := make([]domain.MemberPoints, 0)
	for rows.Next() {
		var memberIDStr string
		var total int
		if err := rows.Scan(&memberIDStr, &total); err != nil {
			return nil, fmt.Errorf("leaderboard: scan: %w", err)
		}
		memberID, err := household.ParseMemberID(memberIDStr)
		if err != nil {
			return nil, fmt.Errorf("leaderboard: parse member id: %w", err)
		}
		result = append(result, domain.MemberPoints{
			MemberID: memberID,
			Points:   total,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("leaderboard: %w", err)
	}
	return result, nil
}

// History returns the member's most recent point ledger entries within the
// household, newest first, limited to at most limit rows (NES-118). Each row
// is enriched with recurring_task.title (for a "task_instance" award or
// domain.SourceTypeClaimExpiry penalty, both keyed via point_ledger.source_id
// -> task_instance.id -> recurring_task.id) or reward.name (for a
// "redemption" debit, keyed via source_id -> reward_redemption.id ->
// reward.id) so the caller can build a human-readable reason without an
// additional query per entry. The two LEFT JOIN chains are each gated by
// source_type so a row only ever matches the chain relevant to its own kind;
// COALESCE collapses an unresolved or inapplicable join to "" rather than a
// NULL scan target. The ordering tiebreaks on pl.id (a UUIDv7, so
// time-ordered) after created_at, so the result is deterministic even when
// two entries share the same created_at value.
// Returns an empty slice (not an error) when the member has no entries.
func (r *PointLedgerPostgresRepository) History(
	ctx context.Context,
	householdID household.HouseholdID,
	memberID household.MemberID,
	limit int,
) ([]domain.PointHistoryEntry, error) {
	const q = `
		SELECT pl.id, pl.source_type, pl.points, pl.created_at,
		       COALESCE(rt.title, ''), COALESCE(rw.name, '')
		  FROM point_ledger pl
		  LEFT JOIN task_instance ti
		         ON pl.source_type IN ('task_instance', $3)
		        AND ti.id = pl.source_id
		  LEFT JOIN recurring_task rt ON rt.id = ti.recurring_task_id
		  LEFT JOIN reward_redemption rr
		         ON pl.source_type = 'redemption'
		        AND rr.id = pl.source_id
		  LEFT JOIN reward rw ON rw.id = rr.reward_id
		 WHERE pl.household_id = $1
		   AND pl.member_id    = $2
		 ORDER BY pl.created_at DESC, pl.id DESC
		 LIMIT $4`
	rows, err := r.dbtx.Query(ctx, q,
		householdID.String(),
		memberID.String(),
		domain.SourceTypeClaimExpiry,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	defer rows.Close()

	entries := make([]domain.PointHistoryEntry, 0)
	for rows.Next() {
		var idStr, sourceType, taskTitle, rewardName string
		var points int
		var createdAt time.Time
		if err := rows.Scan(&idStr, &sourceType, &points, &createdAt, &taskTitle, &rewardName); err != nil {
			return nil, fmt.Errorf("history: scan: %w", err)
		}
		id, err := domain.ParsePointEntryID(idStr)
		if err != nil {
			return nil, fmt.Errorf("history: parse id: %w", err)
		}
		entries = append(entries, domain.PointHistoryEntry{
			ID:         id,
			SourceType: sourceType,
			Points:     points,
			CreatedAt:  createdAt,
			TaskTitle:  taskTitle,
			RewardName: rewardName,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	return entries, nil
}

// RewardPostgresRepository is the pgx-backed implementation of
// domain.RewardRepository. UUIDs are passed and scanned as text so no pgx UUID
// codec registration is required.
type RewardPostgresRepository struct {
	dbtx db.TX
}

// Compile-time assurance that RewardPostgresRepository satisfies the port.
var _ domain.RewardRepository = (*RewardPostgresRepository)(nil)

// NewRewardPostgresRepository constructs a RewardPostgresRepository with an
// injected query executor. The executor is a db.TX, satisfied by both
// *pgxpool.Pool (the default composition) and pgx.Tx (so the repository can
// run inside a caller's transaction).
func NewRewardPostgresRepository(dbtx db.TX) *RewardPostgresRepository {
	if dbtx == nil {
		panic("adapter: NewRewardPostgresRepository requires a non-nil db.TX")
	}
	return &RewardPostgresRepository{dbtx: dbtx}
}

// CreateReward persists a new reward in the household's catalogue. The caller
// must set ID, HouseholdID, Name, CostPoints, and Active; Description,
// ImageRef, and QuantityAvailable are optional (NES-126). The store populates
// CreatedAt and UpdatedAt.
func (r *RewardPostgresRepository) CreateReward(ctx context.Context, reward *domain.Reward) error {
	if reward == nil {
		return errors.New("adapter: create reward: nil reward")
	}
	const q = `
		INSERT INTO reward
			(id, household_id, name, description, cost_points, image_ref, quantity_available, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at, updated_at`
	if err := r.dbtx.QueryRow(ctx, q,
		reward.ID.String(),
		reward.HouseholdID.String(),
		reward.Name,
		reward.Description,
		reward.CostPoints,
		reward.ImageRef,
		reward.QuantityAvailable,
		reward.Active,
	).Scan(&reward.CreatedAt, &reward.UpdatedAt); err != nil {
		return fmt.Errorf("create reward: %w", err)
	}
	return nil
}

// rewardColumns is the full column list for a [domain.Reward] row, in the
// exact order scanReward expects. Named once here so every reward query
// selects — and scans — the same shape (NES-126).
const rewardColumns = `id, household_id, name, description, cost_points, image_ref, quantity_available, active, created_at, updated_at`

// GetReward returns the reward with the given id within the household.
// Returns [domain.ErrRewardNotFound] when id is unknown or belongs to another
// household.
func (r *RewardPostgresRepository) GetReward(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RewardID,
) (*domain.Reward, error) {
	q := `
		SELECT ` + rewardColumns + `
		  FROM reward
		 WHERE id           = $1
		   AND household_id = $2`
	reward, err := scanReward(r.dbtx.QueryRow(ctx, q, id.String(), householdID.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRewardNotFound
		}
		return nil, fmt.Errorf("get reward: %w", err)
	}
	return reward, nil
}

// ListActiveRewards returns all active rewards for the household, ordered by
// cost_points ascending.
// Returns an empty slice (not an error) when none exist.
func (r *RewardPostgresRepository) ListActiveRewards(
	ctx context.Context,
	householdID household.HouseholdID,
) ([]*domain.Reward, error) {
	q := `
		SELECT ` + rewardColumns + `
		  FROM reward
		 WHERE household_id = $1
		   AND active       = true
		 ORDER BY cost_points`
	rows, err := r.dbtx.Query(ctx, q, householdID.String())
	if err != nil {
		return nil, fmt.Errorf("list active rewards: %w", err)
	}
	defer rows.Close()

	rewards := make([]*domain.Reward, 0)
	for rows.Next() {
		reward, err := scanReward(rows)
		if err != nil {
			return nil, fmt.Errorf("list active rewards: scan: %w", err)
		}
		rewards = append(rewards, reward)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active rewards: %w", err)
	}
	return rewards, nil
}

// ListStorefrontRewards returns active, in-stock rewards for the member
// storefront (NES-126 AC2), each joined with its remaining stock. A reward is
// excluded when quantity_available is set and the reward's non-cancelled
// redemption count has reached it; a nil quantity_available (unlimited)
// always qualifies. Ordered by cost_points ascending, matching
// ListActiveRewards.
// Returns an empty slice (not an error) when none qualify.
func (r *RewardPostgresRepository) ListStorefrontRewards(
	ctx context.Context,
	householdID household.HouseholdID,
) ([]domain.StorefrontReward, error) {
	q := `
		SELECT ` + rewardColumns + `, r.quantity_available - COALESCE(redeemed.count, 0) AS remaining
		  FROM reward r
		  LEFT JOIN (
		           SELECT reward_id, COUNT(*) AS count
		             FROM reward_redemption
		            WHERE household_id = $1
		              AND status != 'cancelled'
		            GROUP BY reward_id
		       ) redeemed ON redeemed.reward_id = r.id
		 WHERE r.household_id = $1
		   AND r.active       = true
		   AND (r.quantity_available IS NULL OR r.quantity_available > COALESCE(redeemed.count, 0))
		 ORDER BY r.cost_points`
	rows, err := r.dbtx.Query(ctx, q, householdID.String())
	if err != nil {
		return nil, fmt.Errorf("list storefront rewards: %w", err)
	}
	defer rows.Close()

	storefront := make([]domain.StorefrontReward, 0)
	for rows.Next() {
		reward, remaining, err := scanRewardWithRemaining(rows)
		if err != nil {
			return nil, fmt.Errorf("list storefront rewards: scan: %w", err)
		}
		storefront = append(storefront, domain.StorefrontReward{Reward: reward, RemainingStock: remaining})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list storefront rewards: %w", err)
	}
	return storefront, nil
}

// ListAllRewards returns every reward for the household — active and
// archived — for the parent-only admin catalogue view (NES-126 AC1), ordered
// active-first then by name for a stable, scannable list.
// Returns an empty slice (not an error) when the household has no rewards.
func (r *RewardPostgresRepository) ListAllRewards(
	ctx context.Context,
	householdID household.HouseholdID,
) ([]*domain.Reward, error) {
	q := `
		SELECT ` + rewardColumns + `
		  FROM reward
		 WHERE household_id = $1
		 ORDER BY active DESC, name`
	rows, err := r.dbtx.Query(ctx, q, householdID.String())
	if err != nil {
		return nil, fmt.Errorf("list all rewards: %w", err)
	}
	defer rows.Close()

	rewards := make([]*domain.Reward, 0)
	for rows.Next() {
		reward, err := scanReward(rows)
		if err != nil {
			return nil, fmt.Errorf("list all rewards: scan: %w", err)
		}
		rewards = append(rewards, reward)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all rewards: %w", err)
	}
	return rewards, nil
}

// UpdateReward persists changes to an existing reward's editable catalogue
// fields (Name, Description, CostPoints, ImageRef, QuantityAvailable) and
// refreshes UpdatedAt. Active is untouched — see ArchiveReward. Returns
// [domain.ErrRewardNotFound] when r.ID is unknown or belongs to another
// household.
func (r *RewardPostgresRepository) UpdateReward(ctx context.Context, reward *domain.Reward) error {
	if reward == nil {
		return errors.New("adapter: update reward: nil reward")
	}
	const q = `
		UPDATE reward
		   SET name               = $1,
		       description        = $2,
		       cost_points        = $3,
		       image_ref          = $4,
		       quantity_available = $5,
		       updated_at         = now()
		 WHERE id           = $6
		   AND household_id = $7
		RETURNING updated_at`
	err := r.dbtx.QueryRow(ctx, q,
		reward.Name,
		reward.Description,
		reward.CostPoints,
		reward.ImageRef,
		reward.QuantityAvailable,
		reward.ID.String(),
		reward.HouseholdID.String(),
	).Scan(&reward.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRewardNotFound
		}
		return fmt.Errorf("update reward: %w", err)
	}
	return nil
}

// ArchiveReward sets active = false on the reward, retiring it from the
// storefront (NES-126) without touching its redemption history. Returns
// [domain.ErrRewardNotFound] when id is unknown or belongs to another
// household.
func (r *RewardPostgresRepository) ArchiveReward(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RewardID,
) error {
	const q = `
		UPDATE reward
		   SET active     = false,
		       updated_at = now()
		 WHERE id           = $1
		   AND household_id = $2`
	tag, err := r.dbtx.Exec(ctx, q, id.String(), householdID.String())
	if err != nil {
		return fmt.Errorf("archive reward: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrRewardNotFound
	}
	return nil
}

// DeleteReward permanently removes a reward that has no redemption history
// (NES-126 AC5). Returns [domain.ErrRewardNotFound] when id is unknown or
// belongs to another household, and [domain.ErrRewardHasRedemptions] when the
// reward_redemption_reward_fk ON DELETE RESTRICT constraint rejects the
// delete because at least one redemption still references it.
func (r *RewardPostgresRepository) DeleteReward(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RewardID,
) error {
	const q = `
		DELETE FROM reward
		 WHERE id           = $1
		   AND household_id = $2`
	tag, err := r.dbtx.Exec(ctx, q, id.String(), householdID.String())
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) &&
			pgErr.Code == sqlstateForeignKeyViolation &&
			pgErr.ConstraintName == constraintRewardRedemptionRewardFK {
			return domain.ErrRewardHasRedemptions
		}
		return fmt.Errorf("delete reward: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrRewardNotFound
	}
	return nil
}

// Redeem persists a reward redemption row. The caller must set ID, HouseholdID,
// RewardID, MemberID, Status, CreatedAt, and UpdatedAt. Returns
// [domain.ErrRewardNotFound] when redemption.RewardID is unknown or belongs to
// another household (rejected by the composite FK on reward_redemption).
func (r *RewardPostgresRepository) Redeem(ctx context.Context, redemption *domain.RewardRedemption) error {
	if redemption == nil {
		return errors.New("adapter: redeem: nil redemption")
	}
	const q = `
		INSERT INTO reward_redemption
			(id, household_id, reward_id, member_id, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := r.dbtx.Exec(ctx, q,
		redemption.ID.String(),
		redemption.HouseholdID.String(),
		redemption.RewardID.String(),
		redemption.MemberID.String(),
		redemption.Status.String(),
		redemption.CreatedAt,
		redemption.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) &&
			pgErr.Code == sqlstateForeignKeyViolation &&
			pgErr.ConstraintName == constraintRewardRedemptionRewardFK {
			// Reward-FK violation — the reward_id does not exist in this household.
			// A member-FK violation is a distinct failure (no member sentinel in
			// gamification) and falls through to the wrapped generic error below.
			return domain.ErrRewardNotFound
		}
		return fmt.Errorf("redeem reward: %w", err)
	}
	return nil
}

// RedeemWithDebit atomically records a reward redemption and debits the
// member's point balance in a single database transaction. It is the NES-37
// write-path that guarantees the balance check and the debit are race-safe.
//
// Protocol:
//  1. Begin a transaction.
//  2. Acquire pg_advisory_xact_lock(hashtext(householdID || ':' || memberID)).
//     The lock serialises concurrent redeems for the same (household, member)
//     pair within the same Postgres instance, eliminating the TOCTOU window
//     between reading the balance and inserting the debit row. The lock is
//     transaction-scoped and released automatically on commit/rollback.
//  3. Compute the member's current balance inside the transaction so the read
//     is protected by the advisory lock.
//  4. If balance < costPoints → rollback, return [domain.ErrInsufficientPoints].
//  5. INSERT the reward_redemption row (status = 'requested').
//     A 23503 FK violation on the reward column → rollback,
//     return [domain.ErrRewardNotFound].
//  6. INSERT a negative point_ledger row
//     (source_type = 'redemption', points = -costPoints).
//  7. Commit.
//
// Error contracts:
//   - Returns [domain.ErrInsufficientPoints] when balance < costPoints.
//   - Returns [domain.ErrRewardNotFound] when redemption.RewardID does not
//     exist within the household (FK violation on reward_redemption_reward_fk).
func (r *RewardPostgresRepository) RedeemWithDebit(
	ctx context.Context,
	redemption *domain.RewardRedemption,
	costPoints int,
) error {
	if redemption == nil {
		return errors.New("adapter: redeem with debit: nil redemption")
	}

	beginner, ok := r.dbtx.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return errors.New("redeem with debit: executor does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("redeem with debit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Step 2: serialize concurrent redeems for this (household, member) pair.
	// pg_advisory_xact_lock takes a single int8 key; hashtext produces a 32-bit
	// hash of the string, which fits in int8 safely. Concatenating both IDs with a
	// separator prevents collisions between a householdID that is a prefix of a
	// memberID.
	const lockQ = `SELECT pg_advisory_xact_lock(hashtext($1 || ':' || $2))`
	if _, err := tx.Exec(ctx, lockQ,
		redemption.HouseholdID.String(),
		redemption.MemberID.String(),
	); err != nil {
		return fmt.Errorf("redeem with debit: advisory lock: %w", err)
	}

	// Step 3: read the current balance inside the transaction.
	const balQ = `
		SELECT COALESCE(SUM(points), 0)
		  FROM point_ledger
		 WHERE household_id = $1
		   AND member_id    = $2`
	var balance int
	if err := tx.QueryRow(ctx, balQ,
		redemption.HouseholdID.String(),
		redemption.MemberID.String(),
	).Scan(&balance); err != nil {
		return fmt.Errorf("redeem with debit: read balance: %w", err)
	}

	// Step 4: guard insufficient funds.
	if balance < costPoints {
		return domain.ErrInsufficientPoints
	}

	// Step 5: insert the redemption row.
	const redemptionQ = `
		INSERT INTO reward_redemption
			(id, household_id, reward_id, member_id, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err = tx.Exec(ctx, redemptionQ,
		redemption.ID.String(),
		redemption.HouseholdID.String(),
		redemption.RewardID.String(),
		redemption.MemberID.String(),
		redemption.Status.String(),
		redemption.CreatedAt,
		redemption.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) &&
			pgErr.Code == sqlstateForeignKeyViolation &&
			pgErr.ConstraintName == constraintRewardRedemptionRewardFK {
			return domain.ErrRewardNotFound
		}
		return fmt.Errorf("redeem with debit: insert redemption: %w", err)
	}

	// Step 6: append the debit ledger entry.
	// source_type = 'redemption', points is negative to represent a debit.
	// The id is generated app-side as UUIDv7 for index locality, matching all
	// other point entry creation sites in this codebase.
	redemptionUUID := redemption.ID.String()
	const debitQ = `
		INSERT INTO point_ledger
			(id, household_id, member_id, source_type, source_id, points, created_at)
		VALUES ($1::uuid, $2, $3, 'redemption', $4::uuid, $5, now())`
	if _, err := tx.Exec(ctx, debitQ,
		domain.NewPointEntryID().String(),
		redemption.HouseholdID.String(),
		redemption.MemberID.String(),
		redemptionUUID,
		-costPoints,
	); err != nil {
		return fmt.Errorf("redeem with debit: insert ledger debit: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("redeem with debit: commit: %w", err)
	}
	return nil
}

// scanReward scans a reward row (rewardColumns order) from r into a new
// [domain.Reward].
func scanReward(r row) (*domain.Reward, error) {
	var (
		reward                domain.Reward
		idStr, householdIDStr string
	)
	err := r.Scan(
		&idStr,
		&householdIDStr,
		&reward.Name,
		&reward.Description,
		&reward.CostPoints,
		&reward.ImageRef,
		&reward.QuantityAvailable,
		&reward.Active,
		&reward.CreatedAt,
		&reward.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	id, err := domain.ParseRewardID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan reward: %w", err)
	}
	householdID, err := household.ParseHouseholdID(householdIDStr)
	if err != nil {
		return nil, fmt.Errorf("scan reward: %w", err)
	}
	reward.ID = id
	reward.HouseholdID = householdID
	return &reward, nil
}

// scanRewardWithRemaining scans a rewardColumns row plus a trailing computed
// "remaining" column (r.quantity_available - redeemed count) into a
// [domain.Reward] and its remaining-stock pointer. remaining is nil when the
// reward's QuantityAvailable is nil (unlimited stock) — Postgres propagates
// NULL through the subtraction automatically in that case, so the same NULL
// scan target used for QuantityAvailable works here.
func scanRewardWithRemaining(r row) (*domain.Reward, *int, error) {
	var (
		reward                domain.Reward
		idStr, householdIDStr string
		remaining             *int
	)
	err := r.Scan(
		&idStr,
		&householdIDStr,
		&reward.Name,
		&reward.Description,
		&reward.CostPoints,
		&reward.ImageRef,
		&reward.QuantityAvailable,
		&reward.Active,
		&reward.CreatedAt,
		&reward.UpdatedAt,
		&remaining,
	)
	if err != nil {
		return nil, nil, err
	}
	id, err := domain.ParseRewardID(idStr)
	if err != nil {
		return nil, nil, fmt.Errorf("scan reward with remaining: %w", err)
	}
	householdID, err := household.ParseHouseholdID(householdIDStr)
	if err != nil {
		return nil, nil, fmt.Errorf("scan reward with remaining: %w", err)
	}
	reward.ID = id
	reward.HouseholdID = householdID
	return &reward, remaining, nil
}
