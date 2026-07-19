package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// FK constraint names on the member_credential table (00034). The household
// FK is an inline column reference, so Postgres auto-names it
// <table>_<column>_fkey; the member FK is the explicitly named composite
// tenant constraint — mirroring calendarAccountHouseholdFK/
// calendarAccountMemberFK's own split.
const (
	// webauthnCredentialHouseholdFK is the auto-named FK
	// member_credential.household_id -> household(id); a violation means
	// householdID itself does not exist.
	webauthnCredentialHouseholdFK = "member_credential_household_id_fkey"
	// webauthnCredentialMemberFK is the composite tenant FK on
	// member_credential; a violation means memberID does not belong to
	// householdID, mirroring mfaMemberFK.
	webauthnCredentialMemberFK = "member_credential_member_fk"
)

// WebAuthnCredentialRepository is the pgx-backed
// authdomain.WebAuthnCredentialRepository. UUIDs are passed and scanned as
// text, mirroring MFARepository's and CredentialRepository's convention (no
// pgx UUID codec registration) — except AAGUID, which pgx's native uuid.UUID
// support scans directly since it is never used as a lookup key, only
// stored/returned opaquely.
type WebAuthnCredentialRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ authdomain.WebAuthnCredentialRepository = (*WebAuthnCredentialRepository)(nil)

// NewWebAuthnCredentialRepository constructs the repository with an injected
// query executor (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewWebAuthnCredentialRepository(dbtx db.TX) *WebAuthnCredentialRepository {
	if dbtx == nil {
		panic("adapter: NewWebAuthnCredentialRepository requires a non-nil db.TX")
	}
	return &WebAuthnCredentialRepository{dbtx: dbtx}
}

// ListByMember returns every credential registered by memberID, oldest
// first — ties on created_at (two credentials registered within the same
// clock tick) broken deterministically by id, its own tie-break secondary
// key. A member with none returns an empty slice, never an error.
func (r *WebAuthnCredentialRepository) ListByMember(ctx context.Context, memberID household.MemberID) ([]authdomain.WebAuthnCredential, error) {
	const q = `
		SELECT id, household_id, credential_id, public_key, sign_count, transports,
		       aaguid, nickname, user_handle, created_at, last_used_at
		  FROM member_credential
		 WHERE member_id = $1
		 ORDER BY created_at, id`

	rows, err := r.dbtx.Query(ctx, q, memberID.String())
	if err != nil {
		return nil, fmt.Errorf("list webauthn credentials: %w", err)
	}
	defer rows.Close()

	var creds []authdomain.WebAuthnCredential
	for rows.Next() {
		var (
			idStr          string
			householdIDStr string
			aaguid         *uuid.UUID
			cred           = authdomain.WebAuthnCredential{MemberID: memberID}
		)
		if err := rows.Scan(
			&idStr, &householdIDStr, &cred.CredentialID, &cred.PublicKey, &cred.SignCount, &cred.Transports,
			&aaguid, &cred.Nickname, &cred.UserHandle, &cred.CreatedAt, &cred.LastUsedAt,
		); err != nil {
			return nil, fmt.Errorf("list webauthn credentials: scan: %w", err)
		}
		id, err := authdomain.ParseWebAuthnCredentialID(idStr)
		if err != nil {
			return nil, fmt.Errorf("list webauthn credentials: parse id: %w", err)
		}
		householdID, err := household.ParseHouseholdID(householdIDStr)
		if err != nil {
			return nil, fmt.Errorf("list webauthn credentials: parse household id: %w", err)
		}
		cred.ID = id
		cred.HouseholdID = householdID
		cred.AAGUID = aaguid
		creds = append(creds, cred)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list webauthn credentials: %w", err)
	}
	return creds, nil
}

// Create persists a newly registered credential. Returns
// household.ErrHouseholdNotFound when householdID does not exist, and
// household.ErrMemberNotFound when cred.MemberID does not belong to
// householdID (FK violations).
func (r *WebAuthnCredentialRepository) Create(ctx context.Context, householdID household.HouseholdID, cred *authdomain.WebAuthnCredential) error {
	const q = `
		INSERT INTO member_credential
			(id, household_id, member_id, credential_id, public_key, sign_count,
			 transports, aaguid, nickname, user_handle)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

	_, err := r.dbtx.Exec(ctx, q,
		cred.ID.String(), householdID.String(), cred.MemberID.String(), cred.CredentialID, cred.PublicKey, cred.SignCount,
		cred.Transports, cred.AAGUID, cred.Nickname, cred.UserHandle,
	)
	if err != nil {
		if mapped := mapWebAuthnCredentialFKViolation(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("create webauthn credential: %w", err)
	}
	return nil
}

// mapWebAuthnCredentialFKViolation maps a member_credential FK violation to
// its domain sentinel, or nil when err is not a recognized FK violation —
// mirroring calendar/adapter's mapFKViolation.
func mapWebAuthnCredentialFKViolation(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != foreignKeyViolation {
		return nil
	}
	switch pgErr.ConstraintName {
	case webauthnCredentialHouseholdFK:
		return household.ErrHouseholdNotFound
	case webauthnCredentialMemberFK:
		return household.ErrMemberNotFound
	default:
		return nil
	}
}

// Rename updates the nickname on the credential identified by id, scoped to
// memberID and householdID. Returns authdomain.ErrWebAuthnCredentialNotFound
// when no row matches all three.
func (r *WebAuthnCredentialRepository) Rename(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID, id authdomain.WebAuthnCredentialID, nickname string) error {
	const q = `
		UPDATE member_credential
		   SET nickname = $4
		 WHERE id = $1 AND member_id = $2 AND household_id = $3`

	tag, err := r.dbtx.Exec(ctx, q, id.String(), memberID.String(), householdID.String(), nickname)
	if err != nil {
		return fmt.Errorf("rename webauthn credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return authdomain.ErrWebAuthnCredentialNotFound
	}
	return nil
}

// Delete removes the credential identified by id, scoped to memberID and
// householdID. Returns authdomain.ErrWebAuthnCredentialNotFound when no row
// matches all three.
func (r *WebAuthnCredentialRepository) Delete(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID, id authdomain.WebAuthnCredentialID) error {
	const q = `DELETE FROM member_credential WHERE id = $1 AND member_id = $2 AND household_id = $3`

	tag, err := r.dbtx.Exec(ctx, q, id.String(), memberID.String(), householdID.String())
	if err != nil {
		return fmt.Errorf("delete webauthn credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return authdomain.ErrWebAuthnCredentialNotFound
	}
	return nil
}

// FindByUserHandle resolves handle to its owning member and every one of
// that member's registered credentials — the usernameless login lookup
// (NES-137), driven by member_credential_user_handle_idx. Returns
// household.ErrMemberNotFound when handle matches no row.
func (r *WebAuthnCredentialRepository) FindByUserHandle(ctx context.Context, handle []byte) (household.MemberID, []authdomain.WebAuthnCredential, error) {
	const q = `
		SELECT id, household_id, member_id, credential_id, public_key, sign_count, transports,
		       aaguid, nickname, user_handle, created_at, last_used_at
		  FROM member_credential
		 WHERE user_handle = $1
		 ORDER BY created_at, id`

	rows, err := r.dbtx.Query(ctx, q, handle)
	if err != nil {
		return household.MemberID{}, nil, fmt.Errorf("find webauthn credentials by user handle: %w", err)
	}
	defer rows.Close()

	var (
		memberID household.MemberID
		creds    []authdomain.WebAuthnCredential
	)
	for rows.Next() {
		var (
			idStr          string
			householdIDStr string
			memberIDStr    string
			aaguid         *uuid.UUID
			cred           authdomain.WebAuthnCredential
		)
		if err := rows.Scan(
			&idStr, &householdIDStr, &memberIDStr, &cred.CredentialID, &cred.PublicKey, &cred.SignCount, &cred.Transports,
			&aaguid, &cred.Nickname, &cred.UserHandle, &cred.CreatedAt, &cred.LastUsedAt,
		); err != nil {
			return household.MemberID{}, nil, fmt.Errorf("find webauthn credentials by user handle: scan: %w", err)
		}
		id, err := authdomain.ParseWebAuthnCredentialID(idStr)
		if err != nil {
			return household.MemberID{}, nil, fmt.Errorf("find webauthn credentials by user handle: parse id: %w", err)
		}
		householdID, err := household.ParseHouseholdID(householdIDStr)
		if err != nil {
			return household.MemberID{}, nil, fmt.Errorf("find webauthn credentials by user handle: parse household id: %w", err)
		}
		parsedMemberID, err := household.ParseMemberID(memberIDStr)
		if err != nil {
			return household.MemberID{}, nil, fmt.Errorf("find webauthn credentials by user handle: parse member id: %w", err)
		}
		cred.ID = id
		cred.HouseholdID = householdID
		cred.MemberID = parsedMemberID
		cred.AAGUID = aaguid
		memberID = parsedMemberID
		creds = append(creds, cred)
	}
	if err := rows.Err(); err != nil {
		return household.MemberID{}, nil, fmt.Errorf("find webauthn credentials by user handle: %w", err)
	}
	if len(creds) == 0 {
		return household.MemberID{}, nil, household.ErrMemberNotFound
	}
	return memberID, creds, nil
}

// UpdateAfterAssertion persists the authenticator's new signature counter
// and last-used timestamp for the credential identified by its raw
// WebAuthn credential id (credential_id, globally unique — not this row's
// own WebAuthnCredentialID) — but ONLY when usedAt is not older than
// whatever last_used_at is already on file (NULL, on a credential's first
// assertion, always qualifies). This is a monotonic guard against a race
// between two concurrently successful assertions on the SAME credential
// (e.g. two devices asserting near-simultaneously, or a retried request
// racing its own original): without it, a later-completing but
// OLDER-in-real-time assertion could overwrite a newer sign_count/
// last_used_at pair a faster concurrent assertion already recorded,
// silently regressing state clone-detection depends on — mirroring
// MFARepository.RecordLoginStep's own last_totp_step guard, though the
// caller contract differs (see below).
//
// Returns authdomain.ErrWebAuthnCredentialNotFound only when credentialID
// matches no row AT ALL. When the row exists but the guard skipped the
// write (a newer assertion already won the race), this returns nil, NOT an
// error: unlike RecordLoginStep (where losing an equivalent race means the
// TOTP code itself is rejected as a replay), the assertion that reaches
// this method has ALREADY been cryptographically verified by
// WebAuthnService before this call — losing the bookkeeping race here must
// never fail an otherwise-valid login or step-up, only skip a write that
// would have regressed stored state backward in time.
func (r *WebAuthnCredentialRepository) UpdateAfterAssertion(ctx context.Context, credentialID []byte, signCount uint32, usedAt time.Time) error {
	const q = `
		UPDATE member_credential
		   SET sign_count = $2, last_used_at = $3
		 WHERE credential_id = $1
		   AND (last_used_at IS NULL OR last_used_at <= $3)`

	tag, err := r.dbtx.Exec(ctx, q, credentialID, signCount, usedAt)
	if err != nil {
		return fmt.Errorf("update webauthn credential after assertion: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}

	const existsQ = `SELECT EXISTS (SELECT 1 FROM member_credential WHERE credential_id = $1)`
	var exists bool
	if err := r.dbtx.QueryRow(ctx, existsQ, credentialID).Scan(&exists); err != nil {
		return fmt.Errorf("update webauthn credential after assertion: check existence: %w", err)
	}
	if !exists {
		return authdomain.ErrWebAuthnCredentialNotFound
	}
	return nil
}
