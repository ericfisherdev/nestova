package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// memberContactFK is the auto-named FK member_contact.member_id ->
// identity.member(id); a violation means memberID does not exist.
const memberContactFK = "member_contact_member_id_fkey"

// PostgresContactDirectory is the pgx-backed implementation of
// domain.ContactDirectory (NES-139). It reads and writes nestova's own
// member_contact table (NSTR-115: rehomed here from columns on the
// now-dropped member table) — the ONLY adapter in the codebase that does —
// deliberately keeping identity.member itself channel-agnostic; see
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
// domain.ErrMemberContactNotFound when memberID is unknown. member_contact
// is sparse (a member who has never touched phone/opt-in settings has no
// row at all), so the LEFT JOIN against identity.member is what lets an
// unknown member still be distinguished from a known one with no contact
// row yet — the latter returns a zero-value MemberContact, not an error.
func (r *PostgresContactDirectory) GetContact(ctx context.Context, memberID household.MemberID) (*domain.MemberContact, error) {
	const q = `
		SELECT c.phone_e164, c.sms_opted_in_at
		  FROM identity.member m
		  LEFT JOIN member_contact c ON c.member_id = m.id
		 WHERE m.id = $1`
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

// SetPhone replaces memberID's phone number (nil clears it), upserting the
// member_contact row (a member's first phone/opt-in write has no
// pre-existing row). Returns domain.ErrMemberContactNotFound when memberID
// is unknown (an FK violation on insert).
//
// The IS DISTINCT FROM guard implements the port's own
// same-number-is-a-no-op-for-consent contract in one round trip: opt-in
// state resets to NULL only when the stored number actually changes
// (including a change TO or FROM NULL, or a first-ever insert), never when
// a member resubmits their already-current number unchanged.
func (r *PostgresContactDirectory) SetPhone(ctx context.Context, memberID household.MemberID, phone *domain.E164Phone) error {
	var phoneStr *string
	if phone != nil {
		s := phone.String()
		phoneStr = &s
	}
	const q = `
		INSERT INTO member_contact (member_id, phone_e164, sms_opted_in_at)
		VALUES ($1, $2, NULL)
		ON CONFLICT (member_id) DO UPDATE
		   SET phone_e164 = EXCLUDED.phone_e164,
		       sms_opted_in_at = CASE
		           WHEN member_contact.phone_e164 IS DISTINCT FROM EXCLUDED.phone_e164 THEN NULL
		           ELSE member_contact.sms_opted_in_at
		       END`
	if _, err := r.pool.Exec(ctx, q, memberID.String(), phoneStr); err != nil {
		if mapped := mapMemberContactFKViolation(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("set member phone: %w", err)
	}
	return nil
}

// mapMemberContactFKViolation maps a member_contact FK violation to
// domain.ErrMemberContactNotFound, or nil when err is not that violation.
func mapMemberContactFKViolation(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == memberContactFK {
		return domain.ErrMemberContactNotFound
	}
	return nil
}

// SetOptedIn sets memberID's SMS opt-in state. Setting true stamps
// sms_opted_in_at to now() and requires a phone number already on file
// (domain.ErrPhoneRequiredForOptIn otherwise, including when memberID has
// no member_contact row at all); setting false always succeeds for a known
// member, including one with no row yet (already, trivially, opted out).
// Returns domain.ErrMemberContactNotFound when memberID is unknown.
func (r *PostgresContactDirectory) SetOptedIn(ctx context.Context, memberID household.MemberID, optIn bool) error {
	if !optIn {
		const q = `UPDATE member_contact SET sms_opted_in_at = NULL WHERE member_id = $1`
		tag, err := r.pool.Exec(ctx, q, memberID.String())
		if err != nil {
			return fmt.Errorf("set member sms opt-in: %w", err)
		}
		if tag.RowsAffected() > 0 {
			return nil
		}
		exists, err := r.memberExists(ctx, memberID)
		if err != nil {
			return err
		}
		if !exists {
			return domain.ErrMemberContactNotFound
		}
		return nil
	}

	const q = `UPDATE member_contact SET sms_opted_in_at = now() WHERE member_id = $1 AND phone_e164 IS NOT NULL`
	tag, err := r.pool.Exec(ctx, q, memberID.String())
	if err != nil {
		return fmt.Errorf("set member sms opt-in: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	// No rows updated is ambiguous (unknown member vs. no phone on file yet,
	// including no member_contact row at all) — disambiguate with one cheap
	// existence check, paid only on this failure path, not on every
	// successful opt-in.
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
	const q = `SELECT EXISTS(SELECT 1 FROM identity.member WHERE id = $1)`
	var exists bool
	if err := r.pool.QueryRow(ctx, q, memberID.String()).Scan(&exists); err != nil {
		return false, fmt.Errorf("check member exists: %w", err)
	}
	return exists, nil
}
