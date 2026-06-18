// Package adapter contains the household context's outbound adapters — here, the
// Postgres implementation of domain.HouseholdRepository.
package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ericfisherdev/nestova/internal/household/domain"
)

const (
	// uniqueViolation is the PostgreSQL SQLSTATE for a unique-constraint violation.
	uniqueViolation = "23505"
	// foreignKeyViolation is the PostgreSQL SQLSTATE for a foreign-key violation.
	foreignKeyViolation = "23503"
	// memberNameUniqueIndex is the unique index enforcing per-household display
	// name uniqueness (the NES-17 baseline migration). Only this constraint maps
	// to ErrDuplicateMember; other unique violations (e.g. the PK) surface as-is.
	memberNameUniqueIndex = "member_household_name_uniq"
)

// PostgresRepository is the pgx-backed HouseholdRepository. UUIDs are passed and
// scanned as text, so no pgx UUID codec registration is required.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.HouseholdRepository = (*PostgresRepository)(nil)

// NewPostgresRepository constructs the repository with an injected pgx pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	if pool == nil {
		panic("adapter: NewPostgresRepository requires a non-nil pool")
	}
	return &PostgresRepository{pool: pool}
}

// CreateHousehold inserts a household and populates its timestamps.
func (r *PostgresRepository) CreateHousehold(ctx context.Context, h *domain.Household) error {
	if h == nil {
		return errors.New("adapter: create household: nil household")
	}
	const q = `INSERT INTO household (id, name) VALUES ($1, $2) RETURNING created_at, updated_at`
	if err := r.pool.QueryRow(ctx, q, h.ID.String(), h.Name).Scan(&h.CreatedAt, &h.UpdatedAt); err != nil {
		return fmt.Errorf("create household: %w", err)
	}
	return nil
}

// GetHousehold returns the household, or domain.ErrHouseholdNotFound.
func (r *PostgresRepository) GetHousehold(ctx context.Context, id domain.HouseholdID) (*domain.Household, error) {
	const q = `SELECT id, name, created_at, updated_at FROM household WHERE id = $1`
	h, err := scanHousehold(r.pool.QueryRow(ctx, q, id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrHouseholdNotFound
		}
		return nil, fmt.Errorf("get household: %w", err)
	}
	return h, nil
}

// AddMember inserts a member, returning domain.ErrDuplicateMember when the
// display name collides within the household.
func (r *PostgresRepository) AddMember(ctx context.Context, m *domain.Member) error {
	if m == nil {
		return errors.New("adapter: add member: nil member")
	}
	const q = `
		INSERT INTO member (id, household_id, display_name, role, color_key)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at, updated_at`
	err := r.pool.QueryRow(ctx, q, m.ID.String(), m.HouseholdID.String(), m.DisplayName, m.Role.String(), m.Color.String()).
		Scan(&m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch {
			case pgErr.Code == uniqueViolation && pgErr.ConstraintName == memberNameUniqueIndex:
				return domain.ErrDuplicateMember
			case pgErr.Code == foreignKeyViolation:
				// The only FK on member is household_id -> household.
				return domain.ErrHouseholdNotFound
			}
		}
		return fmt.Errorf("add member: %w", err)
	}
	return nil
}

// GetMember returns the member, or domain.ErrMemberNotFound.
func (r *PostgresRepository) GetMember(ctx context.Context, id domain.MemberID) (*domain.Member, error) {
	const q = `
		SELECT id, household_id, display_name, role, color_key, created_at, updated_at
		FROM member WHERE id = $1`
	m, err := scanMember(r.pool.QueryRow(ctx, q, id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrMemberNotFound
		}
		return nil, fmt.Errorf("get member: %w", err)
	}
	return m, nil
}

// ListMembers returns the household's members ordered by creation.
func (r *PostgresRepository) ListMembers(ctx context.Context, householdID domain.HouseholdID) ([]*domain.Member, error) {
	const q = `
		SELECT id, household_id, display_name, role, color_key, created_at, updated_at
		FROM member WHERE household_id = $1 ORDER BY created_at, id`
	rows, err := r.pool.Query(ctx, q, householdID.String())
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()

	members := make([]*domain.Member, 0)
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, fmt.Errorf("list members: scan: %w", err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	return members, nil
}

// row abstracts pgx.Row and pgx.Rows for the shared scan helpers.
type row interface {
	Scan(dest ...any) error
}

func scanHousehold(r row) (*domain.Household, error) {
	var (
		h     domain.Household
		idStr string
	)
	if err := r.Scan(&idStr, &h.Name, &h.CreatedAt, &h.UpdatedAt); err != nil {
		return nil, err
	}
	id, err := domain.ParseHouseholdID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan household: %w", err)
	}
	h.ID = id
	return &h, nil
}

func scanMember(r row) (*domain.Member, error) {
	var (
		m                          domain.Member
		idStr, hidStr, role, color string
	)
	if err := r.Scan(&idStr, &hidStr, &m.DisplayName, &role, &color, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, err
	}
	id, err := domain.ParseMemberID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan member: %w", err)
	}
	hid, err := domain.ParseHouseholdID(hidStr)
	if err != nil {
		return nil, fmt.Errorf("scan member: %w", err)
	}
	parsedRole, err := domain.ParseRole(role)
	if err != nil {
		return nil, fmt.Errorf("scan member: %w", err)
	}
	parsedColor, err := domain.ParseMemberColor(color)
	if err != nil {
		return nil, fmt.Errorf("scan member: %w", err)
	}
	m.ID, m.HouseholdID, m.Role, m.Color = id, hid, parsedRole, parsedColor
	return &m, nil
}
