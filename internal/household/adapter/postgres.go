// Package adapter contains the household context's outbound adapters — here, the
// Postgres implementation of domain.HouseholdRepository.
package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

const (
	// uniqueViolation is the PostgreSQL SQLSTATE for a unique-constraint violation.
	uniqueViolation = "23505"
	// foreignKeyViolation is the PostgreSQL SQLSTATE for a foreign-key violation.
	foreignKeyViolation = "23503"
	// memberHouseholdFK is the auto-named FK constraint identity.member.household_id ->
	// identity.household.id. Only this FK maps to ErrHouseholdNotFound.
	memberHouseholdFK = "member_household_id_fkey"
	// memberNameUniqueIndex is the unique index enforcing per-household display
	// name uniqueness on identity.member. Only this constraint maps to
	// ErrDuplicateMember; other unique violations (e.g. the PK) surface as-is.
	memberNameUniqueIndex = "member_household_name_uniq"
	// defaultMemberColor is used when a member has no nestova.member_profile
	// row yet (NSTR-115): identity owns member/household, nestova owns only
	// the presentation-layer color, so a member visible through identity
	// before its profile row exists (a narrow window during provisioning)
	// still renders with a valid palette color instead of an empty one.
	defaultMemberColor = domain.ColorSage
)

// PostgresRepository is the pgx-backed HouseholdRepository. UUIDs are passed and
// scanned as text, so no pgx UUID codec registration is required.
//
// household and member rows live in the shared identity schema (NSTR-115,
// identity.household / identity.member), owned and migrated by nestcore —
// this repository queries them cross-schema rather than owning them, and
// joins nestova's own nestova.member_profile table (color_key) alongside
// identity.member, since presentation color is app-specific and identity
// must stay app-agnostic.
type PostgresRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.HouseholdRepository = (*PostgresRepository)(nil)

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
	const q = `INSERT INTO identity.household (id, name) VALUES ($1, $2) RETURNING created_at, updated_at`
	if err := r.dbtx.QueryRow(ctx, q, h.ID.String(), h.Name).Scan(&h.CreatedAt, &h.UpdatedAt); err != nil {
		return fmt.Errorf("create household: %w", err)
	}
	return nil
}

// GetHousehold returns the household, or domain.ErrHouseholdNotFound.
func (r *PostgresRepository) GetHousehold(ctx context.Context, id domain.HouseholdID) (*domain.Household, error) {
	const q = `
		SELECT id, name, created_at, updated_at
		FROM identity.household WHERE id = $1`
	h, err := scanHousehold(r.dbtx.QueryRow(ctx, q, id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrHouseholdNotFound
		}
		return nil, fmt.Errorf("get household: %w", err)
	}
	return h, nil
}

// memberTxBeginner is the slice of a pgx executor AddMember needs to open
// its own transaction (mirroring authadapter.mfaTxBeginner): a new member
// requires two atomic inserts — identity.member (owned by nestcore) and
// this app's own nestova.member_profile row for color — so a failure
// partway through must not leave an identity.member row with no profile.
type memberTxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// AddMember inserts a member (identity.member) and its nestova-owned
// profile row (nestova.member_profile, for color) in one transaction,
// returning domain.ErrDuplicateMember when the display name collides
// within the household.
func (r *PostgresRepository) AddMember(ctx context.Context, m *domain.Member) error {
	if m == nil {
		return errors.New("adapter: add member: nil member")
	}
	beginner, ok := r.dbtx.(memberTxBeginner)
	if !ok {
		return errors.New("add member: executor does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("add member: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const insertMember = `
		INSERT INTO identity.member (id, household_id, display_name, role)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at, updated_at`
	err = tx.QueryRow(ctx, insertMember, m.ID.String(), m.HouseholdID.String(), m.DisplayName, m.Role.String()).
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

	const insertProfile = `INSERT INTO member_profile (member_id, color_key) VALUES ($1, $2)`
	if _, err := tx.Exec(ctx, insertProfile, m.ID.String(), m.Color.String()); err != nil {
		return fmt.Errorf("add member: profile: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("add member: commit: %w", err)
	}
	return nil
}

// GetMember returns the member, or domain.ErrMemberNotFound.
func (r *PostgresRepository) GetMember(ctx context.Context, id domain.MemberID) (*domain.Member, error) {
	m, _, err := r.getMemberRow(ctx, id)
	return m, err
}

// getMemberRow is GetMember's shared implementation, additionally reporting
// whether a member_profile row already existed — EnsureMemberProfile needs
// that signal to decide whether provisioning is necessary; GetMember itself
// discards it.
func (r *PostgresRepository) getMemberRow(ctx context.Context, id domain.MemberID) (*domain.Member, bool, error) {
	const q = `
		SELECT m.id, m.household_id, m.display_name, m.role,
		       COALESCE(p.color_key, $2), p.color_key IS NOT NULL
		  FROM identity.member m
		  LEFT JOIN member_profile p ON p.member_id = m.id
		 WHERE m.id = $1`
	m, hadProfile, err := scanMemberRow(r.dbtx.QueryRow(ctx, q, id.String(), string(defaultMemberColor)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, domain.ErrMemberNotFound
		}
		return nil, false, fmt.Errorf("get member: %w", err)
	}
	return m, hadProfile, nil
}

// EnsureMemberProfile implements domain.HouseholdRepository.EnsureMemberProfile.
// It is idempotent: a member with an existing profile row is returned
// unchanged, at the cost of one extra (cheap, indexed) query per call — a
// deliberate simplicity trade-off over caching "already provisioned" state,
// since provisioning only ever happens once per member in this app's
// lifetime and this codebase has no existing per-request cache to hang that
// off of.
func (r *PostgresRepository) EnsureMemberProfile(ctx context.Context, id domain.MemberID) (*domain.Member, error) {
	m, hadProfile, err := r.getMemberRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if hadProfile {
		return m, nil
	}

	const usedColorsQ = `
		SELECT p.color_key
		  FROM member_profile p
		  JOIN identity.member hm ON hm.id = p.member_id
		 WHERE hm.household_id = $1`
	rows, err := r.dbtx.Query(ctx, usedColorsQ, m.HouseholdID.String())
	if err != nil {
		return nil, fmt.Errorf("ensure member profile: used colors: %w", err)
	}
	used := make([]domain.MemberColor, 0)
	for rows.Next() {
		var colorStr string
		if err := rows.Scan(&colorStr); err != nil {
			rows.Close()
			return nil, fmt.Errorf("ensure member profile: scan color: %w", err)
		}
		color, err := domain.ParseMemberColor(colorStr)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("ensure member profile: parse color: %w", err)
		}
		used = append(used, color)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("ensure member profile: used colors: %w", err)
	}
	rows.Close()

	const insertProfile = `
		INSERT INTO member_profile (member_id, color_key) VALUES ($1, $2)
		ON CONFLICT (member_id) DO NOTHING`
	if _, err := r.dbtx.Exec(ctx, insertProfile, m.ID.String(), domain.NextColor(used).String()); err != nil {
		return nil, fmt.Errorf("ensure member profile: insert: %w", err)
	}

	// Re-fetch rather than trust this call's own NextColor: a concurrent
	// request racing the same missing-profile member may have inserted
	// first, and ON CONFLICT DO NOTHING keeps whichever row landed first.
	m, _, err = r.getMemberRow(ctx, id)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// HasAnyHousehold reports whether at least one household row exists in the
// database. It is used by the onboarding flow to decide whether the
// first-run setup page should be shown or whether to redirect to /login.
func (r *PostgresRepository) HasAnyHousehold(ctx context.Context) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM identity.household)`
	var exists bool
	if err := r.dbtx.QueryRow(ctx, q).Scan(&exists); err != nil {
		return false, fmt.Errorf("has any household: %w", err)
	}
	return exists, nil
}

// ListMembers returns the household's members ordered by creation.
func (r *PostgresRepository) ListMembers(ctx context.Context, householdID domain.HouseholdID) ([]*domain.Member, error) {
	const q = `
		SELECT m.id, m.household_id, m.display_name, m.role,
		       COALESCE(p.color_key, $2), p.color_key IS NOT NULL
		  FROM identity.member m
		  LEFT JOIN member_profile p ON p.member_id = m.id
		 WHERE m.household_id = $1
		 ORDER BY m.created_at, m.id`
	rows, err := r.dbtx.Query(ctx, q, householdID.String(), string(defaultMemberColor))
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()

	members := make([]*domain.Member, 0)
	for rows.Next() {
		m, _, err := scanMemberRow(rows)
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

// scanMemberRow expects id, household_id, display_name, role, color_key,
// had_profile (in that order) — GetMember/ListMembers/EnsureMemberProfile's
// shared column shape after the identity.member/member_profile join.
// created_at/updated_at intentionally are NOT part of this shape: they come
// back through identity.member's own RETURNING clause on write (AddMember),
// never re-read here, since none of these callers currently exposes them.
func scanMemberRow(r row) (*domain.Member, bool, error) {
	var (
		m                          domain.Member
		idStr, hidStr, role, color string
		hadProfile                 bool
	)
	if err := r.Scan(&idStr, &hidStr, &m.DisplayName, &role, &color, &hadProfile); err != nil {
		return nil, false, err
	}
	id, err := domain.ParseMemberID(idStr)
	if err != nil {
		return nil, false, fmt.Errorf("scan member: %w", err)
	}
	hid, err := domain.ParseHouseholdID(hidStr)
	if err != nil {
		return nil, false, fmt.Errorf("scan member: %w", err)
	}
	parsedRole, err := domain.ParseRole(role)
	if err != nil {
		return nil, false, fmt.Errorf("scan member: %w", err)
	}
	parsedColor, err := domain.ParseMemberColor(color)
	if err != nil {
		return nil, false, fmt.Errorf("scan member: %w", err)
	}
	m.ID, m.HouseholdID, m.Role, m.Color = id, hid, parsedRole, parsedColor
	return &m, hadProfile, nil
}
