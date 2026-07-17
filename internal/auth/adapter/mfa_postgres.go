package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// mfaMemberFK is the composite tenant FK on member_mfa (00031); a violation
// means memberID does not belong to the given household.
const mfaMemberFK = "member_mfa_member_fk"

// MFARepository is the pgx-backed authdomain.MFARepository. UUIDs are passed
// and scanned as text, mirroring CredentialRepository's convention (no pgx
// UUID codec registration).
type MFARepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ authdomain.MFARepository = (*MFARepository)(nil)

// NewMFARepository constructs the repository with an injected query executor
// (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewMFARepository(dbtx db.TX) *MFARepository {
	if dbtx == nil {
		panic("adapter: NewMFARepository requires a non-nil db.TX")
	}
	return &MFARepository{dbtx: dbtx}
}

// GetEnrollment returns memberID's enrollment (confirmed or not), or
// authdomain.ErrMFANotEnrolled when no row exists.
func (r *MFARepository) GetEnrollment(ctx context.Context, memberID household.MemberID) (*authdomain.MFAEnrollment, error) {
	const q = `
		SELECT household_id, totp_secret_enc, confirmed_at, created_at, updated_at
		  FROM member_mfa
		 WHERE member_id = $1`

	var (
		householdIDStr string
		enrollment     = &authdomain.MFAEnrollment{MemberID: memberID}
	)
	err := r.dbtx.QueryRow(ctx, q, memberID.String()).Scan(
		&householdIDStr, &enrollment.TOTPSecretEnc, &enrollment.ConfirmedAt,
		&enrollment.CreatedAt, &enrollment.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, authdomain.ErrMFANotEnrolled
		}
		return nil, fmt.Errorf("get mfa enrollment: %w", err)
	}
	householdID, err := household.ParseHouseholdID(householdIDStr)
	if err != nil {
		return nil, fmt.Errorf("get mfa enrollment: parse household id: %w", err)
	}
	enrollment.HouseholdID = householdID
	return enrollment, nil
}

// mfaTxBeginner is the slice of a pgx executor BeginEnrollment and
// ConfirmEnrollmentWithCodes need to open their own transaction, satisfied
// by both *pgxpool.Pool and pgx.Tx (mirroring
// ActivationCodeRepository.Redeem's and ReplaceRecoveryCodes' own use of
// the same type-assertion pattern).
type mfaTxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// BeginEnrollment upserts an unconfirmed enrollment for memberID, inside a
// transaction that locks any existing row (SELECT ... FOR UPDATE) before
// deciding how to proceed — closing the race a plain
// INSERT ... ON CONFLICT DO UPDATE would leave open between two concurrent
// BeginEnrollment calls, while still distinguishing WHY a conflicting row
// blocks the write (see the two error returns below), which a single
// ON CONFLICT ... WHERE clause cannot do from its zero-rows-returned result
// alone.
//
// Returns authdomain.ErrMFAAlreadyEnrolled when the existing row is already
// CONFIRMED, and household.ErrMemberNotFound both for a genuinely unknown
// member/household (FK violation on insert) and when an existing row
// belongs to a DIFFERENT household than householdID — a defense-in-depth
// tenant guard: this method must never overwrite another household's
// pending secret, and reports both cases identically so neither leaks
// which one occurred.
func (r *MFARepository) BeginEnrollment(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID, secretEnc []byte) error {
	beginner, ok := r.dbtx.(mfaTxBeginner)
	if !ok {
		return errors.New("begin mfa enrollment: executor does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin mfa enrollment: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		existingHouseholdID string
		confirmedAt         *time.Time
	)
	lookupErr := tx.QueryRow(ctx, `SELECT household_id, confirmed_at FROM member_mfa WHERE member_id = $1 FOR UPDATE`, memberID.String()).
		Scan(&existingHouseholdID, &confirmedAt)
	switch {
	case errors.Is(lookupErr, pgx.ErrNoRows):
		const insert = `
			INSERT INTO member_mfa (member_id, household_id, totp_secret_enc, confirmed_at)
			VALUES ($1, $2, $3, NULL)`
		if _, err := tx.Exec(ctx, insert, memberID.String(), householdID.String(), secretEnc); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == mfaMemberFK {
				return household.ErrMemberNotFound
			}
			return fmt.Errorf("begin mfa enrollment: insert: %w", err)
		}
	case lookupErr != nil:
		return fmt.Errorf("begin mfa enrollment: lookup: %w", lookupErr)
	case existingHouseholdID != householdID.String():
		// The row exists but under a DIFFERENT household than the caller
		// supplied — never touch it. Reported the same as an unknown
		// member so no household-boundary information leaks.
		return household.ErrMemberNotFound
	case confirmedAt != nil:
		return authdomain.ErrMFAAlreadyEnrolled
	default:
		const update = `
			UPDATE member_mfa
			   SET totp_secret_enc = $2,
			       confirmed_at    = NULL,
			       updated_at      = now()
			 WHERE member_id = $1`
		if _, err := tx.Exec(ctx, update, memberID.String(), secretEnc); err != nil {
			return fmt.Errorf("begin mfa enrollment: update: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("begin mfa enrollment: commit: %w", err)
	}
	return nil
}

// ConfirmEnrollmentWithCodes atomically confirms memberID's enrollment and
// replaces their recovery codes with one fresh row per hash, in a single
// transaction that locks the row (SELECT ... FOR UPDATE) before confirming
// — see the port doc for why this must be one atomic operation rather than
// a separate ConfirmEnrollment + ReplaceRecoveryCodes: it is what makes two
// concurrent callers racing to confirm the SAME still-unconfirmed
// enrollment resolve to exactly one winner, with the loser's hashes never
// persisted at all.
func (r *MFARepository) ConfirmEnrollmentWithCodes(ctx context.Context, memberID household.MemberID, recoveryCodeHashes []string) error {
	beginner, ok := r.dbtx.(mfaTxBeginner)
	if !ok {
		return errors.New("confirm mfa enrollment: executor does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("confirm mfa enrollment: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var confirmedAt *time.Time
	lookupErr := tx.QueryRow(ctx, `SELECT confirmed_at FROM member_mfa WHERE member_id = $1 FOR UPDATE`, memberID.String()).Scan(&confirmedAt)
	switch {
	case errors.Is(lookupErr, pgx.ErrNoRows):
		return authdomain.ErrMFANotEnrolled
	case lookupErr != nil:
		return fmt.Errorf("confirm mfa enrollment: lookup: %w", lookupErr)
	case confirmedAt != nil:
		// Already confirmed — including by a concurrent racing confirm
		// that committed first while this call waited on the row lock.
		return authdomain.ErrMFAAlreadyEnrolled
	}

	if _, err := tx.Exec(ctx, `UPDATE member_mfa SET confirmed_at = now(), updated_at = now() WHERE member_id = $1`, memberID.String()); err != nil {
		return fmt.Errorf("confirm mfa enrollment: confirm: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM member_recovery_code WHERE member_id = $1`, memberID.String()); err != nil {
		return fmt.Errorf("confirm mfa enrollment: delete existing recovery codes: %w", err)
	}
	const insert = `
		INSERT INTO member_recovery_code (id, member_id, code_hash)
		VALUES ($1, $2, $3)`
	for _, hash := range recoveryCodeHashes {
		id := authdomain.NewRecoveryCodeID()
		if _, err := tx.Exec(ctx, insert, id.String(), memberID.String(), hash); err != nil {
			return fmt.Errorf("confirm mfa enrollment: insert recovery code: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("confirm mfa enrollment: commit: %w", err)
	}
	return nil
}

// DeleteEnrollment removes memberID's enrollment (confirmed or not),
// cascading its recovery codes, scoped to householdID as a defense-in-depth
// tenant check. Returns authdomain.ErrMFANotEnrolled when no row exists in
// that household.
func (r *MFARepository) DeleteEnrollment(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID) error {
	const q = `DELETE FROM member_mfa WHERE member_id = $1 AND household_id = $2`

	tag, err := r.dbtx.Exec(ctx, q, memberID.String(), householdID.String())
	if err != nil {
		return fmt.Errorf("delete mfa enrollment: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return authdomain.ErrMFANotEnrolled
	}
	return nil
}

// ReplaceRecoveryCodes atomically deletes every existing recovery code for
// memberID and inserts one fresh row per hash, in a single transaction
// (opened directly against the pool this repository was constructed with,
// mirroring ActivationCodeRepository.Redeem's self-contained transaction
// pattern), so a failure partway through leaves the previous set intact.
func (r *MFARepository) ReplaceRecoveryCodes(ctx context.Context, memberID household.MemberID, hashes []string) error {
	beginner, ok := r.dbtx.(mfaTxBeginner)
	if !ok {
		return errors.New("replace recovery codes: executor does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("replace recovery codes: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM member_recovery_code WHERE member_id = $1`, memberID.String()); err != nil {
		return fmt.Errorf("replace recovery codes: delete existing: %w", err)
	}

	const insert = `
		INSERT INTO member_recovery_code (id, member_id, code_hash)
		VALUES ($1, $2, $3)`
	for _, hash := range hashes {
		id := authdomain.NewRecoveryCodeID()
		if _, err := tx.Exec(ctx, insert, id.String(), memberID.String(), hash); err != nil {
			return fmt.Errorf("replace recovery codes: insert: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("replace recovery codes: commit: %w", err)
	}
	return nil
}

// ListUnusedRecoveryCodes returns every not-yet-used recovery code for
// memberID, oldest first.
func (r *MFARepository) ListUnusedRecoveryCodes(ctx context.Context, memberID household.MemberID) ([]authdomain.RecoveryCode, error) {
	const q = `
		SELECT id, code_hash, created_at
		  FROM member_recovery_code
		 WHERE member_id = $1
		   AND used_at IS NULL
		 ORDER BY created_at`

	rows, err := r.dbtx.Query(ctx, q, memberID.String())
	if err != nil {
		return nil, fmt.Errorf("list unused recovery codes: %w", err)
	}
	defer rows.Close()

	var codes []authdomain.RecoveryCode
	for rows.Next() {
		var (
			idStr string
			code  authdomain.RecoveryCode
		)
		if err := rows.Scan(&idStr, &code.CodeHash, &code.CreatedAt); err != nil {
			return nil, fmt.Errorf("list unused recovery codes: scan: %w", err)
		}
		id, err := authdomain.ParseRecoveryCodeID(idStr)
		if err != nil {
			return nil, fmt.Errorf("list unused recovery codes: parse id: %w", err)
		}
		code.ID = id
		code.MemberID = memberID
		codes = append(codes, code)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list unused recovery codes: %w", err)
	}
	return codes, nil
}

// MarkRecoveryCodeUsed sets used_at = now() on codeID.
func (r *MFARepository) MarkRecoveryCodeUsed(ctx context.Context, codeID authdomain.RecoveryCodeID) error {
	const q = `UPDATE member_recovery_code SET used_at = now() WHERE id = $1 AND used_at IS NULL`

	tag, err := r.dbtx.Exec(ctx, q, codeID.String())
	if err != nil {
		return fmt.Errorf("mark recovery code used: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark recovery code used: %s: %w", codeID.String(), authdomain.ErrRecoveryCodeInvalid)
	}
	return nil
}
