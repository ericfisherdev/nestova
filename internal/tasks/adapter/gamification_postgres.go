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
// "redemption" debit or a [domain.SourceTypeRedemptionRefund] denial/
// cancellation credit (NES-127), both keyed via source_id ->
// reward_redemption.id -> reward.id) so the caller can build a human-readable
// reason without an additional query per entry. The two LEFT JOIN chains are
// each gated by source_type so a row only ever matches the chain relevant to
// its own kind; COALESCE collapses an unresolved or inapplicable join to ""
// rather than a NULL scan target. The ordering tiebreaks on pl.id (a UUIDv7,
// so time-ordered) after created_at, so the result is deterministic even when
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
		         ON pl.source_type IN ('redemption', $5)
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
		domain.SourceTypeRedemptionRefund,
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
// excluded when quantity_available is set and the reward's non-cancelled,
// non-denied redemption count has reached it (NES-127: a denied redemption
// frees its reserved unit exactly like a cancelled one — see
// [domain.RewardRepository.RedeemWithDebit]'s matching atomic stock check
// applying the same predicate at write time); a nil quantity_available
// (unlimited) always qualifies. Ordered by cost_points ascending, matching
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
		              AND status NOT IN ('cancelled', 'denied')
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
// write-path that guarantees the balance check and the debit are race-safe,
// extended by NES-127 to also guarantee finite-stock rewards cannot be
// over-redeemed, and to close a TOCTOU gap (CodeRabbit finding): the
// reward's active flag and cost_points are read and locked INSIDE this
// transaction, not passed in by the caller — a caller-supplied cost or
// active flag could have been read before a concurrent archive or price edit
// committed, letting an archived reward be redeemed or a stale price be
// debited. The only reward-identifying input the caller supplies is
// redemption.RewardID; the returned int is the cost actually debited,
// straight from the row this method locked, so callers never need to trust
// their own pre-transaction read for anything but an early, optimistic
// fail-fast (see [RewardRedeemer.RedeemWithDebit]'s doc on the app-layer
// side of that split).
//
// Protocol:
//  1. Begin a transaction.
//  2. Acquire pg_advisory_xact_lock(hashtext(householdID || ':' || memberID)).
//     The lock serialises concurrent redeems for the same (household, member)
//     pair within the same Postgres instance, eliminating the TOCTOU window
//     between reading the balance and inserting the debit row. The lock is
//     transaction-scoped and released automatically on commit/rollback.
//  3. SELECT active, cost_points, quantity_available ... FOR UPDATE the
//     reward row itself. Unlike the advisory lock in step 2 (scoped per
//     member), this locks per REWARD, so it also serialises two DIFFERENT
//     members concurrently redeeming the same finite-stock reward — the case
//     the per-member lock cannot cover — AND makes every value read here
//     authoritative for the rest of this transaction: no concurrent archive
//     or price edit can commit against this row until this transaction ends.
//     Every caller acquires locks in the same order (advisory lock, then
//     this row lock), so the two lock types can never deadlock against each
//     other.
//  4. If no row is found → rollback, return [domain.ErrRewardNotFound]. If
//     the locked row is not active → rollback, return
//     [domain.ErrRewardNotFound] (an archived reward is "not found" from the
//     redeemer's perspective, matching the app layer's existing convention).
//  5. Compute the member's current balance inside the transaction so the read
//     is protected by the advisory lock. If balance < the LOCKED cost_points
//     → rollback, return [domain.ErrInsufficientPoints].
//  6. If the locked row's quantity_available is non-nil, count the reward's
//     current non-cancelled, non-denied redemptions (now safe to trust: under
//     READ COMMITTED, a fresh statement issued after the FOR UPDATE lock is
//     granted sees every row a competing transaction committed before
//     releasing that same lock). If the count has already reached
//     quantity_available → rollback, return [domain.ErrRewardOutOfStock].
//  7. INSERT the reward_redemption row (status = 'pending').
//     A 23503 FK violation on the reward column → rollback,
//     return [domain.ErrRewardNotFound]. A 23505 unique violation on
//     reward_redemption_deep_link_signature_uniq (NES-129, only possible
//     when redemption.DeepLinkSignatureHash is non-nil) → rollback, return
//     [domain.ErrDeepLinkAlreadyRedeemed].
//  8. INSERT a negative point_ledger row (source_type = 'redemption', points
//     = -<locked cost_points>).
//  9. Commit.
//
// Error contracts:
//   - Returns [domain.ErrRewardNotFound] when redemption.RewardID does not
//     exist within the household, is archived (active = false), or the
//     subsequent INSERT's FK violates reward_redemption_reward_fk.
//   - Returns [domain.ErrInsufficientPoints] when balance < the reward's
//     current (locked) cost_points.
//   - Returns [domain.ErrRewardOutOfStock] when the reward has a finite
//     quantity_available and it has already been reached.
//   - Returns [domain.ErrDeepLinkAlreadyRedeemed] (NES-129) when
//     redemption.DeepLinkSignatureHash is non-nil and a redemption with the
//     same (household_id, deep_link_signature_hash) pair already committed
//     — this is a DATABASE constraint, not an in-process guard, so it holds
//     durably across process restarts and multiple server instances.
func (r *RewardPostgresRepository) RedeemWithDebit(
	ctx context.Context,
	redemption *domain.RewardRedemption,
) (int, error) {
	if redemption == nil {
		return 0, errors.New("adapter: redeem with debit: nil redemption")
	}

	tx, err := beginTx(ctx, r.dbtx, "redeem with debit")
	if err != nil {
		return 0, err
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
		return 0, fmt.Errorf("redeem with debit: advisory lock: %w", err)
	}

	// Step 3: lock the reward row and read the fields this transaction must
	// treat as authoritative (active, cost_points, quantity_available) — see
	// this method's doc for why these are never trusted from the caller.
	const lockRewardQ = `
		SELECT active, cost_points, quantity_available
		  FROM reward
		 WHERE id = $1 AND household_id = $2
		 FOR UPDATE`
	var (
		active            bool
		costPoints        int
		quantityAvailable *int
	)
	if err := tx.QueryRow(ctx, lockRewardQ,
		redemption.RewardID.String(),
		redemption.HouseholdID.String(),
	).Scan(&active, &costPoints, &quantityAvailable); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, domain.ErrRewardNotFound
		}
		return 0, fmt.Errorf("redeem with debit: lock reward: %w", err)
	}

	// Step 4: an archived reward can no longer be redeemed, regardless of
	// what the caller believed when it decided to call this method.
	if !active {
		return 0, domain.ErrRewardNotFound
	}

	// Step 5: read the current balance inside the transaction and guard
	// insufficient funds against the LOCKED cost, not a caller-supplied one.
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
		return 0, fmt.Errorf("redeem with debit: read balance: %w", err)
	}
	if balance < costPoints {
		return 0, domain.ErrInsufficientPoints
	}

	// Step 6: finite-stock guard (NES-127). Nil quantity_available means
	// unlimited stock — skip the check entirely.
	if quantityAvailable != nil {
		const stockCountQ = `
			SELECT COUNT(*)
			  FROM reward_redemption
			 WHERE household_id = $1
			   AND reward_id    = $2
			   AND status NOT IN ('cancelled', 'denied')`
		var redeemedCount int
		if err := tx.QueryRow(ctx, stockCountQ,
			redemption.HouseholdID.String(),
			redemption.RewardID.String(),
		).Scan(&redeemedCount); err != nil {
			return 0, fmt.Errorf("redeem with debit: count redemptions: %w", err)
		}
		if redeemedCount >= *quantityAvailable {
			return 0, domain.ErrRewardOutOfStock
		}
	}

	// Step 7: insert the redemption row. deep_link_signature_hash is NULL for
	// every ordinary storefront redemption (redemption.DeepLinkSignatureHash
	// is nil) — only RewardService.RedeemViaDeepLink (NES-129) ever sets it.
	// The partial unique index on (household_id, deep_link_signature_hash)
	// is what durably (across process restarts, at the database level, not
	// via any in-process guard) prevents the SAME signed deep link from
	// redeeming a reward twice.
	const redemptionQ = `
		INSERT INTO reward_redemption
			(id, household_id, reward_id, member_id, status, deep_link_signature_hash, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	_, err = tx.Exec(ctx, redemptionQ,
		redemption.ID.String(),
		redemption.HouseholdID.String(),
		redemption.RewardID.String(),
		redemption.MemberID.String(),
		redemption.Status.String(),
		redemption.DeepLinkSignatureHash,
		redemption.CreatedAt,
		redemption.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == sqlstateForeignKeyViolation &&
			pgErr.ConstraintName == constraintRewardRedemptionRewardFK {
			return 0, domain.ErrRewardNotFound
		}
		if errors.As(err, &pgErr) && pgErr.Code == sqlstateUniqueViolation &&
			pgErr.ConstraintName == constraintRewardRedemptionDeepLinkSignatureUniq {
			return 0, domain.ErrDeepLinkAlreadyRedeemed
		}
		return 0, fmt.Errorf("redeem with debit: insert redemption: %w", err)
	}

	// Step 8: append the debit ledger entry, using the LOCKED cost, not a
	// caller-supplied one. source_type = 'redemption', points is negative to
	// represent a debit. The id is generated app-side as UUIDv7 for index
	// locality, matching all other point entry creation sites in this
	// codebase.
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
		return 0, fmt.Errorf("redeem with debit: insert ledger debit: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("redeem with debit: commit: %w", err)
	}
	return costPoints, nil
}

// ---------------------------------------------------------------------------
// Redemption fulfillment (NES-127) — parent inbox + member self-service.
// ---------------------------------------------------------------------------

// redemptionDetailSelectSQL joins reward_redemption to its reward for the
// parent fulfillment inbox and the member "my redemptions" list, so neither
// caller needs a reward lookup per row. The JOIN is INNER: reward_redemption_
// reward_fk is ON DELETE RESTRICT (00024_reward_catalog_admin.sql), so a
// redemption row can never outlive its reward, mirroring
// tradeSummarySelectSQL's identical INNER-join rationale.
const redemptionDetailSelectSQL = `
	SELECT rr.id, rr.household_id, rr.reward_id, rw.name, rr.member_id,
	       rr.status, rr.denied_reason, rr.created_at, rr.updated_at
	  FROM reward_redemption rr
	  JOIN reward rw ON rw.id = rr.reward_id`

// ListPendingRedemptions returns every pending redemption in the household,
// oldest first — the parent-only fulfillment inbox (NES-127). rr.id (a
// UUIDv7, so itself time-ordered) breaks ties for two redemptions requested
// in the same instant, mirroring PointLedgerPostgresRepository.History's
// identical tie-break precedent. Returns an empty slice (not an error) when
// none are pending.
func (r *RewardPostgresRepository) ListPendingRedemptions(
	ctx context.Context,
	householdID household.HouseholdID,
) ([]domain.RedemptionDetail, error) {
	q := redemptionDetailSelectSQL + `
		 WHERE rr.household_id = $1
		   AND rr.status       = 'pending'
		 ORDER BY rr.created_at, rr.id`
	rows, err := r.dbtx.Query(ctx, q, householdID.String())
	if err != nil {
		return nil, fmt.Errorf("list pending redemptions: %w", err)
	}
	defer rows.Close()

	details := make([]domain.RedemptionDetail, 0)
	for rows.Next() {
		d, err := scanRedemptionDetail(rows)
		if err != nil {
			return nil, fmt.Errorf("list pending redemptions: scan: %w", err)
		}
		details = append(details, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending redemptions: %w", err)
	}
	return details, nil
}

// ListMemberRedemptions returns memberID's most recent redemptions —
// regardless of status — newest first: the member-facing "My redemptions"
// list (NES-127). Every PENDING redemption is always included, uncapped;
// only resolved (fulfilled/denied/cancelled) redemptions are capped at limit
// rows. This split matters because a pending redemption represents points
// already debited but not yet resolved — the ONLY UI surface where the
// member can act on it (Cancel) is this list, so a naive single LIMIT across
// all statuses could let a long-forgotten pending redemption scroll out of
// view behind newer resolved history, stranding its debited points with no
// way to cancel it (CodeRabbit finding). Pending count is inherently small
// at family-appliance scale, so fetching it uncapped is safe. rr.id breaks
// ties in the same (descending) direction as created_at within each half,
// mirroring ListPendingRedemptions' tie-break; the outer ORDER BY re-merges
// both halves by recency, so the combined result still reads as one
// newest-first feed rather than "pending, then resolved" two blocks.
// Returns an empty slice (not an error) when the member has none.
func (r *RewardPostgresRepository) ListMemberRedemptions(
	ctx context.Context,
	householdID household.HouseholdID,
	memberID household.MemberID,
	limit int,
) ([]domain.RedemptionDetail, error) {
	q := `
		WITH pending AS (
			` + redemptionDetailSelectSQL + `
			 WHERE rr.household_id = $1
			   AND rr.member_id    = $2
			   AND rr.status       = 'pending'
		), resolved AS (
			` + redemptionDetailSelectSQL + `
			 WHERE rr.household_id = $1
			   AND rr.member_id    = $2
			   AND rr.status      != 'pending'
			 ORDER BY rr.created_at DESC, rr.id DESC
			 LIMIT $3
		)
		SELECT * FROM pending
		 UNION ALL
		SELECT * FROM resolved
		 ORDER BY created_at DESC, id DESC`
	rows, err := r.dbtx.Query(ctx, q, householdID.String(), memberID.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("list member redemptions: %w", err)
	}
	defer rows.Close()

	details := make([]domain.RedemptionDetail, 0)
	for rows.Next() {
		d, err := scanRedemptionDetail(rows)
		if err != nil {
			return nil, fmt.Errorf("list member redemptions: scan: %w", err)
		}
		details = append(details, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list member redemptions: %w", err)
	}
	return details, nil
}

// Fulfill transitions a pending redemption to fulfilled (NES-127). The
// conditional UPDATE's WHERE status = 'pending' is itself the concurrency
// guard: two concurrent Fulfill/Deny attempts on the same redemption
// serialise on Postgres' row lock, and the loser's statement re-evaluates the
// WHERE clause against the now-committed row (READ COMMITTED's EvalPlanQual),
// so it always sees the up-to-date status and correctly matches 0 rows rather
// than double-resolving the redemption.
func (r *RewardPostgresRepository) Fulfill(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RewardRedemptionID,
) (domain.ResolvedRedemption, error) {
	const q = `
		UPDATE reward_redemption rr
		   SET status = 'fulfilled', updated_at = now()
		  FROM reward rw
		 WHERE rr.id           = $1
		   AND rr.household_id = $2
		   AND rr.status       = 'pending'
		   AND rw.id           = rr.reward_id
		RETURNING rr.member_id, rw.name`
	var memberIDStr, rewardName string
	err := r.dbtx.QueryRow(ctx, q, id.String(), householdID.String()).Scan(&memberIDStr, &rewardName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No open transaction at this point, so reading through r.dbtx (the
			// pool) borrows one connection, not two — unlike Deny below, which
			// must pass its own still-open tx instead.
			return domain.ResolvedRedemption{}, redemptionNotPendingOrNotFound(ctx, r.dbtx, householdID, id)
		}
		return domain.ResolvedRedemption{}, fmt.Errorf("fulfill redemption: %w", err)
	}
	memberID, err := household.ParseMemberID(memberIDStr)
	if err != nil {
		return domain.ResolvedRedemption{}, fmt.Errorf("fulfill redemption: parse member id: %w", err)
	}
	return domain.ResolvedRedemption{
		RedemptionID: id,
		HouseholdID:  householdID,
		MemberID:     memberID,
		RewardName:   rewardName,
		Status:       domain.RedemptionFulfilled,
	}, nil
}

// Deny transitions a pending redemption to denied, records reason (empty
// means no reason given, stored as NULL), and atomically appends a
// compensating refund ledger entry for the exact amount originally debited
// (NES-127). See Fulfill's doc for why the conditional UPDATE alone is a
// sufficient concurrency guard.
func (r *RewardPostgresRepository) Deny(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RewardRedemptionID,
	reason string,
) (domain.ResolvedRedemption, error) {
	tx, err := beginTx(ctx, r.dbtx, "deny redemption")
	if err != nil {
		return domain.ResolvedRedemption{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var reasonArg *string
	if reason != "" {
		reasonArg = &reason
	}

	const resolveQ = `
		UPDATE reward_redemption rr
		   SET status = 'denied', denied_reason = $3, updated_at = now()
		  FROM reward rw
		 WHERE rr.id           = $1
		   AND rr.household_id = $2
		   AND rr.status       = 'pending'
		   AND rw.id           = rr.reward_id
		RETURNING rr.member_id, rw.name`
	var memberIDStr, rewardName string
	err = tx.QueryRow(ctx, resolveQ, id.String(), householdID.String(), reasonArg).Scan(&memberIDStr, &rewardName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Read through the already-open tx, not r.dbtx (the pool) — this
			// transaction still holds a connection checked out, and borrowing a
			// SECOND one here to disambiguate would risk pool exhaustion under
			// load (the same class of bug fixed for
			// TaskInstanceRepository.disambiguateTerminal, NES-116).
			return domain.ResolvedRedemption{}, redemptionNotPendingOrNotFound(ctx, tx, householdID, id)
		}
		return domain.ResolvedRedemption{}, fmt.Errorf("deny redemption: resolve: %w", err)
	}
	memberID, err := household.ParseMemberID(memberIDStr)
	if err != nil {
		return domain.ResolvedRedemption{}, fmt.Errorf("deny redemption: parse member id: %w", err)
	}

	if err := refundRedemption(ctx, tx, householdID, memberID, id); err != nil {
		return domain.ResolvedRedemption{}, fmt.Errorf("deny redemption: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.ResolvedRedemption{}, fmt.Errorf("deny redemption: commit: %w", err)
	}

	return domain.ResolvedRedemption{
		RedemptionID: id,
		HouseholdID:  householdID,
		MemberID:     memberID,
		RewardName:   rewardName,
		Status:       domain.RedemptionDenied,
		DeniedReason: reasonArg,
	}, nil
}

// Cancel transitions a pending redemption belonging to memberID to cancelled
// and appends the same compensating refund as Deny (NES-127). Unlike Fulfill/
// Deny, a 0-row UPDATE here returns [domain.ErrRedemptionNotPending] without
// a disambiguating existence check — see the sentinel's doc for why a member
// should not learn whether a redemption exists at all from a failed cancel of
// someone else's redemption.
func (r *RewardPostgresRepository) Cancel(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RewardRedemptionID,
	memberID household.MemberID,
) (domain.ResolvedRedemption, error) {
	tx, err := beginTx(ctx, r.dbtx, "cancel redemption")
	if err != nil {
		return domain.ResolvedRedemption{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const resolveQ = `
		UPDATE reward_redemption rr
		   SET status = 'cancelled', updated_at = now()
		  FROM reward rw
		 WHERE rr.id           = $1
		   AND rr.household_id = $2
		   AND rr.member_id    = $3
		   AND rr.status       = 'pending'
		   AND rw.id           = rr.reward_id
		RETURNING rw.name`
	var rewardName string
	err = tx.QueryRow(ctx, resolveQ, id.String(), householdID.String(), memberID.String()).Scan(&rewardName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ResolvedRedemption{}, fmt.Errorf("cancel redemption: %w", domain.ErrRedemptionNotPending)
		}
		return domain.ResolvedRedemption{}, fmt.Errorf("cancel redemption: resolve: %w", err)
	}

	if err := refundRedemption(ctx, tx, householdID, memberID, id); err != nil {
		return domain.ResolvedRedemption{}, fmt.Errorf("cancel redemption: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.ResolvedRedemption{}, fmt.Errorf("cancel redemption: commit: %w", err)
	}

	return domain.ResolvedRedemption{
		RedemptionID: id,
		HouseholdID:  householdID,
		MemberID:     memberID,
		RewardName:   rewardName,
		Status:       domain.RedemptionCancelled,
	}, nil
}

// redemptionNotPendingOrNotFound disambiguates a 0-row Fulfill/Deny UPDATE:
// [domain.ErrRedemptionNotFound] when id truly does not exist in the
// household, [domain.ErrRedemptionNotPending] when it exists but is no longer
// pending. Parents act on any redemption in their household (unlike Cancel,
// which is member-scoped), so this extra existence check is worth the one
// additional query on the — expected to be rare — failure path, giving a
// parent a correct 404 for a stale/malformed id versus a 409 for a
// concurrently-resolved one.
//
// q is the caller's read seam: Fulfill (no open transaction at the call site)
// passes r.dbtx (the pool); Deny (still holding its own open tx at the call
// site) passes that tx instead, so this check reuses the connection the
// caller already has checked out rather than borrowing a second one while
// the first is still live — see rowQuerier's doc.
func redemptionNotPendingOrNotFound(
	ctx context.Context,
	q rowQuerier,
	householdID household.HouseholdID,
	id domain.RewardRedemptionID,
) error {
	const existsQ = `SELECT 1 FROM reward_redemption WHERE id = $1 AND household_id = $2`
	var found int
	err := q.QueryRow(ctx, existsQ, id.String(), householdID.String()).Scan(&found)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRedemptionNotFound
		}
		return fmt.Errorf("check redemption exists: %w", err)
	}
	return domain.ErrRedemptionNotPending
}

// refundRedemption looks up the original debit ledger entry for redemptionID
// (source_type = 'redemption') and appends a compensating positive entry
// (source_type = [domain.SourceTypeRedemptionRefund]) for the exact same
// magnitude, within tx. Called by Deny and Cancel after their status-changing
// UPDATE has already matched exactly one row, so the corresponding debit is
// guaranteed to exist by the redeem-time invariant: RedeemWithDebit always
// inserts the redemption row and its debit in the very same transaction, so
// the two either both exist or neither does.
func refundRedemption(
	ctx context.Context,
	tx pgx.Tx,
	householdID household.HouseholdID,
	memberID household.MemberID,
	redemptionID domain.RewardRedemptionID,
) error {
	const debitQ = `
		SELECT points FROM point_ledger
		 WHERE household_id = $1 AND source_type = 'redemption' AND source_id = $2`
	var debitPoints int
	if err := tx.QueryRow(ctx, debitQ, householdID.String(), redemptionID.String()).Scan(&debitPoints); err != nil {
		return fmt.Errorf("find original debit: %w", err)
	}

	const refundQ = `
		INSERT INTO point_ledger
			(id, household_id, member_id, source_type, source_id, points, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())`
	if _, err := tx.Exec(ctx, refundQ,
		domain.NewPointEntryID().String(),
		householdID.String(),
		memberID.String(),
		domain.SourceTypeRedemptionRefund,
		redemptionID.String(),
		-debitPoints,
	); err != nil {
		return fmt.Errorf("insert refund: %w", err)
	}
	return nil
}

// scanRedemptionDetail scans a redemptionDetailSelectSQL row into a
// [domain.RedemptionDetail].
func scanRedemptionDetail(r row) (domain.RedemptionDetail, error) {
	var (
		idStr, householdIDStr, rewardIDStr, memberIDStr, statusStr string
		rewardName                                                 string
		deniedReason                                               *string
		createdAt, updatedAt                                       time.Time
	)
	err := r.Scan(
		&idStr, &householdIDStr, &rewardIDStr, &rewardName, &memberIDStr,
		&statusStr, &deniedReason, &createdAt, &updatedAt,
	)
	if err != nil {
		return domain.RedemptionDetail{}, err
	}
	id, err := domain.ParseRewardRedemptionID(idStr)
	if err != nil {
		return domain.RedemptionDetail{}, fmt.Errorf("scan redemption detail: %w", err)
	}
	householdID, err := household.ParseHouseholdID(householdIDStr)
	if err != nil {
		return domain.RedemptionDetail{}, fmt.Errorf("scan redemption detail: %w", err)
	}
	rewardID, err := domain.ParseRewardID(rewardIDStr)
	if err != nil {
		return domain.RedemptionDetail{}, fmt.Errorf("scan redemption detail: %w", err)
	}
	memberID, err := household.ParseMemberID(memberIDStr)
	if err != nil {
		return domain.RedemptionDetail{}, fmt.Errorf("scan redemption detail: %w", err)
	}
	status, err := domain.ParseRedemptionStatus(statusStr)
	if err != nil {
		return domain.RedemptionDetail{}, fmt.Errorf("scan redemption detail: %w", err)
	}
	return domain.RedemptionDetail{
		ID:           id,
		HouseholdID:  householdID,
		RewardID:     rewardID,
		RewardName:   rewardName,
		MemberID:     memberID,
		Status:       status,
		DeniedReason: deniedReason,
		CreatedAt:    createdAt,
		UpdatedAt:    updatedAt,
	}, nil
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
