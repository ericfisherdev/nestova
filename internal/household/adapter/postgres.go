// Package adapter contains the household context's outbound adapters — here, the
// Postgres implementation of domain.HouseholdRepository.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

const (
	// uniqueViolation is the PostgreSQL SQLSTATE for a unique-constraint violation.
	uniqueViolation = "23505"
	// foreignKeyViolation is the PostgreSQL SQLSTATE for a foreign-key violation.
	foreignKeyViolation = "23503"
	// memberHouseholdFK is the auto-named FK constraint member.household_id ->
	// household.id (NES-17 baseline). Only this FK maps to ErrHouseholdNotFound.
	memberHouseholdFK = "member_household_id_fkey"
	// memberNameUniqueIndex is the unique index enforcing per-household display
	// name uniqueness (the NES-17 baseline migration). Only this constraint maps
	// to ErrDuplicateMember; other unique violations (e.g. the PK) surface as-is.
	memberNameUniqueIndex = "member_household_name_uniq"
)

// PostgresRepository is the pgx-backed HouseholdRepository. UUIDs are passed and
// scanned as text, so no pgx UUID codec registration is required.
type PostgresRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.HouseholdRepository = (*PostgresRepository)(nil)

// Compile-time assurance the adapter also satisfies the narrower
// quiet-hours write port (NES-139) — see that interface's own doc for why
// it is separate from HouseholdRepository.
var _ domain.QuietHoursWriter = (*PostgresRepository)(nil)

// NewPostgresRepository constructs the repository with an injected query
// executor. The executor is a db.TX, satisfied by both *pgxpool.Pool (the
// default composition) and pgx.Tx (so the repository can run inside a caller's
// transaction); the same methods work against either.
func NewPostgresRepository(dbtx db.TX) *PostgresRepository {
	if dbtx == nil {
		panic("adapter: NewPostgresRepository requires a non-nil db.TX")
	}
	return &PostgresRepository{dbtx: dbtx}
}

// CreateHousehold inserts a household and populates its timestamps.
func (r *PostgresRepository) CreateHousehold(ctx context.Context, h *domain.Household) error {
	if h == nil {
		return errors.New("adapter: create household: nil household")
	}
	const q = `INSERT INTO household (id, name) VALUES ($1, $2) RETURNING created_at, updated_at`
	if err := r.dbtx.QueryRow(ctx, q, h.ID.String(), h.Name).Scan(&h.CreatedAt, &h.UpdatedAt); err != nil {
		return fmt.Errorf("create household: %w", err)
	}
	return nil
}

// GetHousehold returns the household, or domain.ErrHouseholdNotFound.
func (r *PostgresRepository) GetHousehold(ctx context.Context, id domain.HouseholdID) (*domain.Household, error) {
	const q = `
		SELECT id, name, quiet_hours_start, quiet_hours_end, created_at, updated_at
		FROM household WHERE id = $1`
	h, err := scanHousehold(r.dbtx.QueryRow(ctx, q, id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrHouseholdNotFound
		}
		return nil, fmt.Errorf("get household: %w", err)
	}
	return h, nil
}

// SetQuietHours updates householdID's quiet-hours window (NES-139).
// Passing nil for both start and end disables quiet hours. Returns an
// error when exactly one of start/end is nil — domain.Household's own doc
// states both nil means disabled, so a half-set pair has no defined
// meaning; the repository is the last line of defense for this invariant
// (the HTTP handler already validates it, but a future caller — a test, a
// service, admin tooling — may not). Returns domain.ErrHouseholdNotFound
// when householdID is unknown.
func (r *PostgresRepository) SetQuietHours(ctx context.Context, householdID domain.HouseholdID, start, end *time.Duration) error {
	if (start == nil) != (end == nil) {
		return fmt.Errorf("set quiet hours: start and end must both be set or both be nil")
	}
	const q = `UPDATE household SET quiet_hours_start = $2, quiet_hours_end = $3, updated_at = now() WHERE id = $1`
	tag, err := r.dbtx.Exec(ctx, q, householdID.String(), durationToPgTime(start), durationToPgTime(end))
	if err != nil {
		return fmt.Errorf("set quiet hours: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrHouseholdNotFound
	}
	return nil
}

// durationToPgTime converts a duration-since-midnight to the pgtype.Time
// wire representation Postgres' time column expects, or an invalid
// (NULL-bound) pgtype.Time when d is nil.
func durationToPgTime(d *time.Duration) pgtype.Time {
	if d == nil {
		return pgtype.Time{}
	}
	return pgtype.Time{Microseconds: d.Microseconds(), Valid: true}
}

// pgTimeToDuration converts a scanned pgtype.Time back to a
// duration-since-midnight, or nil when the column was NULL.
func pgTimeToDuration(t pgtype.Time) *time.Duration {
	if !t.Valid {
		return nil
	}
	d := time.Duration(t.Microseconds) * time.Microsecond
	return &d
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
	err := r.dbtx.QueryRow(ctx, q, m.ID.String(), m.HouseholdID.String(), m.DisplayName, m.Role.String(), m.Color.String()).
		Scan(&m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch {
			case pgErr.Code == uniqueViolation && pgErr.ConstraintName == memberNameUniqueIndex:
				return domain.ErrDuplicateMember
			case pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == memberHouseholdFK:
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
	m, err := scanMember(r.dbtx.QueryRow(ctx, q, id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrMemberNotFound
		}
		return nil, fmt.Errorf("get member: %w", err)
	}
	return m, nil
}

// HasAnyHousehold reports whether at least one household row exists in the
// database. It is used by the onboarding flow to decide whether the
// first-run setup page should be shown or whether to redirect to /login.
func (r *PostgresRepository) HasAnyHousehold(ctx context.Context) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM household)`
	var exists bool
	if err := r.dbtx.QueryRow(ctx, q).Scan(&exists); err != nil {
		return false, fmt.Errorf("has any household: %w", err)
	}
	return exists, nil
}

// ListMembers returns the household's members ordered by creation.
func (r *PostgresRepository) ListMembers(ctx context.Context, householdID domain.HouseholdID) ([]*domain.Member, error) {
	const q = `
		SELECT id, household_id, display_name, role, color_key, created_at, updated_at
		FROM member WHERE household_id = $1 ORDER BY created_at, id`
	rows, err := r.dbtx.Query(ctx, q, householdID.String())
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
		h                    domain.Household
		idStr                string
		quietStart, quietEnd pgtype.Time
	)
	if err := r.Scan(&idStr, &h.Name, &quietStart, &quietEnd, &h.CreatedAt, &h.UpdatedAt); err != nil {
		return nil, err
	}
	id, err := domain.ParseHouseholdID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan household: %w", err)
	}
	h.ID = id
	h.QuietHoursStart = pgTimeToDuration(quietStart)
	h.QuietHoursEnd = pgTimeToDuration(quietEnd)
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
