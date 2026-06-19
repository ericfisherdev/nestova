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
// must set ID, HouseholdID, Name, CostPoints, and Active; the store populates
// CreatedAt and UpdatedAt.
func (r *RewardPostgresRepository) CreateReward(ctx context.Context, reward *domain.Reward) error {
	if reward == nil {
		return errors.New("adapter: create reward: nil reward")
	}
	const q = `
		INSERT INTO reward
			(id, household_id, name, cost_points, active)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at, updated_at`
	if err := r.dbtx.QueryRow(ctx, q,
		reward.ID.String(),
		reward.HouseholdID.String(),
		reward.Name,
		reward.CostPoints,
		reward.Active,
	).Scan(&reward.CreatedAt, &reward.UpdatedAt); err != nil {
		return fmt.Errorf("create reward: %w", err)
	}
	return nil
}

// GetReward returns the reward with the given id within the household.
// Returns [domain.ErrRewardNotFound] when id is unknown or belongs to another
// household.
func (r *RewardPostgresRepository) GetReward(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RewardID,
) (*domain.Reward, error) {
	const q = `
		SELECT id, household_id, name, cost_points, active, created_at, updated_at
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
	const q = `
		SELECT id, household_id, name, cost_points, active, created_at, updated_at
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
		if errors.As(err, &pgErr) && pgErr.Code == sqlstateForeignKeyViolation {
			// FK violation — the reward_id does not exist in this household.
			return domain.ErrRewardNotFound
		}
		return fmt.Errorf("redeem: %w", err)
	}
	return nil
}

// scanReward scans a reward row from r into a new [domain.Reward].
func scanReward(r row) (*domain.Reward, error) {
	var (
		reward                domain.Reward
		idStr, householdIDStr string
	)
	err := r.Scan(
		&idStr,
		&householdIDStr,
		&reward.Name,
		&reward.CostPoints,
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
