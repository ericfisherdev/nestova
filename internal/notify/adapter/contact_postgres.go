package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// PostgresContactDirectory is the pgx-backed implementation of
// domain.ContactDirectory (NES-139). It reads and writes the member
// table's phone_e164/sms_opted_in_at columns directly — the ONLY adapter
// in the codebase that does — deliberately keeping
// internal/household/domain.Member itself channel-agnostic; see
// internal/notify/domain/contact.go's own doc for the full reasoning.
type PostgresContactDirectory struct {
	pool *pgxpool.Pool
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.ContactDirectory = (*PostgresContactDirectory)(nil)

// NewPostgresContactDirectory constructs the directory with an injected
// pgx pool.
func NewPostgresContactDirectory(pool *pgxpool.Pool) *PostgresContactDirectory {
	if pool == nil {
		panic("adapter: NewPostgresContactDirectory requires a non-nil pool")
	}
	return &PostgresContactDirectory{pool: pool}
}

// GetContact returns memberID's current contact details, or
// domain.ErrMemberContactNotFound when memberID is unknown.
func (r *PostgresContactDirectory) GetContact(ctx context.Context, memberID household.MemberID) (*domain.MemberContact, error) {
	const q = `SELECT phone_e164, sms_opted_in_at FROM member WHERE id = $1`
	var (
		phone     *string
		optedInAt *time.Time
	)
	if err := r.pool.QueryRow(ctx, q, memberID.String()).Scan(&phone, &optedInAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrMemberContactNotFound
		}
		return nil, fmt.Errorf("get member contact: %w", err)
	}

	contact := &domain.MemberContact{MemberID: memberID, SMSOptedIn: optedInAt != nil}
	if phone != nil {
		p, err := domain.ParseE164Phone(*phone)
		if err != nil {
			return nil, fmt.Errorf("get member contact: stored phone: %w", err)
		}
		contact.Phone = &p
	}
	return contact, nil
}

// SetPhone replaces memberID's phone number (nil clears it), returning
// domain.ErrMemberContactNotFound when memberID is unknown.
//
// The UPDATE's IS DISTINCT FROM guard implements the port's own
// same-number-is-a-no-op-for-consent contract in one round trip: opt-in
// state resets to NULL only when the stored number actually changes
// (including a change TO or FROM NULL), never when a member resubmits
// their already-current number unchanged.
func (r *PostgresContactDirectory) SetPhone(ctx context.Context, memberID household.MemberID, phone *domain.E164Phone) error {
	var phoneStr *string
	if phone != nil {
		s := phone.String()
		phoneStr = &s
	}
	const q = `
		UPDATE member
		SET phone_e164 = $2,
		    sms_opted_in_at = CASE WHEN phone_e164 IS DISTINCT FROM $2 THEN NULL ELSE sms_opted_in_at END
		WHERE id = $1`
	tag, err := r.pool.Exec(ctx, q, memberID.String(), phoneStr)
	if err != nil {
		return fmt.Errorf("set member phone: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrMemberContactNotFound
	}
	return nil
}

// SetOptedIn sets memberID's SMS opt-in state. Setting true stamps
// sms_opted_in_at to now() and requires a phone number already on file
// (domain.ErrPhoneRequiredForOptIn otherwise); setting false always
// succeeds and clears the timestamp. Returns
// domain.ErrMemberContactNotFound when memberID is unknown.
func (r *PostgresContactDirectory) SetOptedIn(ctx context.Context, memberID household.MemberID, optIn bool) error {
	if !optIn {
		const q = `UPDATE member SET sms_opted_in_at = NULL WHERE id = $1`
		tag, err := r.pool.Exec(ctx, q, memberID.String())
		if err != nil {
			return fmt.Errorf("set member sms opt-in: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrMemberContactNotFound
		}
		return nil
	}

	const q = `UPDATE member SET sms_opted_in_at = now() WHERE id = $1 AND phone_e164 IS NOT NULL`
	tag, err := r.pool.Exec(ctx, q, memberID.String())
	if err != nil {
		return fmt.Errorf("set member sms opt-in: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	// No rows updated is ambiguous (unknown member vs. no phone on file
	// yet) — disambiguate with one cheap existence check, paid only on
	// this failure path, not on every successful opt-in.
	exists, err := r.memberExists(ctx, memberID)
	if err != nil {
		return err
	}
	if !exists {
		return domain.ErrMemberContactNotFound
	}
	return domain.ErrPhoneRequiredForOptIn
}

func (r *PostgresContactDirectory) memberExists(ctx context.Context, memberID household.MemberID) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM member WHERE id = $1)`
	var exists bool
	if err := r.pool.QueryRow(ctx, q, memberID.String()).Scan(&exists); err != nil {
		return false, fmt.Errorf("check member exists: %w", err)
	}
	return exists, nil
}
