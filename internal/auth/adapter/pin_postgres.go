package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// pinMemberFK is the composite tenant FK on identity.member_pin (nestcore's
// 00005_identity_member_pin.sql); a violation means memberID does not
// belong to the given household.
const pinMemberFK = "member_pin_member_fk"

// PINRepository is the pgx-backed authdomain.PINRepository. UUIDs are
// passed and scanned as text, mirroring CredentialRepository's and
// MFARepository's convention (no pgx UUID codec registration).
type PINRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ authdomain.PINRepository = (*PINRepository)(nil)

// NewPINRepository constructs the repository with an injected query
// executor (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewPINRepository(dbtx db.TX) *PINRepository {
	if dbtx == nil {
		panic("adapter: NewPINRepository requires a non-nil db.TX")
	}
	return &PINRepository{dbtx: dbtx}
}

// SetPIN upserts memberID's PIN hash. Unlike member_mfa, a PIN has no
// separate confirmed/unconfirmed lifecycle — it takes effect immediately on
// set — so a plain INSERT ... ON CONFLICT DO UPDATE is sufficient; there is
// no concurrent-enrollment race to close the way BeginEnrollment's
// SELECT ... FOR UPDATE closes one.
//
// Returns household.ErrMemberNotFound when memberID does not belong to
// householdID. On the INSERT path the composite tenant FK is what catches
// that; the conflict path needs its own predicate, because Postgres
// re-checks a referencing row's FK only when the key columns change, and
// DO UPDATE rewrites neither household_id nor member_id. Without the WHERE
// below, a re-set from a foreign household would update the row's hash and
// leave a perfectly valid (member, original household) reference behind for
// the FK to approve.
func (r *PINRepository) SetPIN(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID, pinHash string) error {
	const q = `
		INSERT INTO identity.member_pin (member_id, household_id, pin_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (member_id) DO UPDATE
			SET pin_hash = EXCLUDED.pin_hash, updated_at = now()
			WHERE identity.member_pin.household_id = EXCLUDED.household_id`

	tag, err := r.dbtx.Exec(ctx, q, memberID.String(), householdID.String(), pinHash)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == pinMemberFK {
			return household.ErrMemberNotFound
		}
		return fmt.Errorf("set pin: %w", err)
	}
	// A conflict whose existing row belongs to another household updates
	// nothing, so a foreign member id stays indistinguishable from an
	// unknown one — the same answer the INSERT path's FK gives, and the same
	// shape ClearPIN uses.
	if tag.RowsAffected() == 0 {
		return household.ErrMemberNotFound
	}
	return nil
}

// GetPINHash returns memberID's stored argon2id hash, or
// authdomain.ErrPINNotEnrolled when no row exists.
func (r *PINRepository) GetPINHash(ctx context.Context, memberID household.MemberID) (string, error) {
	const q = `SELECT pin_hash FROM identity.member_pin WHERE member_id = $1`

	var hash string
	err := r.dbtx.QueryRow(ctx, q, memberID.String()).Scan(&hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", authdomain.ErrPINNotEnrolled
		}
		return "", fmt.Errorf("get pin hash: %w", err)
	}
	return hash, nil
}

// ClearPIN deletes memberID's PIN row, returning
// authdomain.ErrPINNotEnrolled when no row exists to delete.
func (r *PINRepository) ClearPIN(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID) error {
	// household_id is part of the predicate, not just the row: member_id is
	// this table's whole primary key, so an unscoped DELETE would let an
	// admin of ANY household clear any member's PIN given only their id.
	// SetPIN states its tenant too — via the composite FK on its INSERT path
	// and an explicit predicate on its conflict path — and a DELETE has no FK
	// to lean on at all, so it states the tenant itself.
	const q = `DELETE FROM identity.member_pin WHERE member_id = $1 AND household_id = $2`

	tag, err := r.dbtx.Exec(ctx, q, memberID.String(), householdID.String())
	if err != nil {
		return fmt.Errorf("clear pin: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return authdomain.ErrPINNotEnrolled
	}
	return nil
}

// EnrolledMembers returns every member id with a PIN row in householdID,
// backing the settings page's admin member list (which members already
// have a PIN, so the UI can offer "set" vs "reset").
func (r *PINRepository) EnrolledMembers(ctx context.Context, householdID household.HouseholdID) ([]household.MemberID, error) {
	const q = `SELECT member_id FROM identity.member_pin WHERE household_id = $1`

	rows, err := r.dbtx.Query(ctx, q, householdID.String())
	if err != nil {
		return nil, fmt.Errorf("list enrolled members: %w", err)
	}
	defer rows.Close()

	var ids []household.MemberID
	for rows.Next() {
		var idStr string
		if err := rows.Scan(&idStr); err != nil {
			return nil, fmt.Errorf("list enrolled members: scan: %w", err)
		}
		id, err := household.ParseMemberID(idStr)
		if err != nil {
			return nil, fmt.Errorf("list enrolled members: parse id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list enrolled members: %w", err)
	}
	return ids, nil
}
