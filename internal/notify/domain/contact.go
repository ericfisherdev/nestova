package domain

import (
	"context"
	"errors"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// MemberContact is the notify context's own read/write model for a
// member's SMS contact details (NES-139) — phone_e164 and
// sms_opted_in_at, physically columns on the member table but
// deliberately absent from household.Member: only ContactDirectory
// implementations (and their callers here in notify) know these columns
// exist, keeping the household bounded context's own domain model
// channel-agnostic. See internal/platform/db/migrate/migrations/00036's
// own doc for the full reasoning.
type MemberContact struct {
	MemberID household.MemberID
	// Phone is nil when the member has never entered a phone number, or
	// after it was cleared (ContactDirectory.ClearPhone).
	Phone *E164Phone
	// SMSOptedIn is true only after the member has explicitly checked the
	// SMS opt-in box (docs/aws-sms.md's production consent gate: this
	// flag, together with the timestamp ContactDirectory records
	// alongside it, IS the express-written-consent record). Always false
	// when Phone is nil — a phone number and its consent are set/cleared
	// together (see ContactDirectory.SetPhone's own doc).
	SMSOptedIn bool
}

// ReadyForSMS reports whether c has both a phone number on file and
// current opt-in consent — the precondition SMSNotificationSender.Send
// (NES-139) checks before ever calling domain.SMSSender.
func (c MemberContact) ReadyForSMS() bool {
	return c.Phone != nil && c.SMSOptedIn
}

// Domain errors for member contact operations.
var (
	// ErrMemberContactNotFound is returned when memberID does not exist.
	ErrMemberContactNotFound = errors.New("notify: member contact not found")
	// ErrPhoneRequiredForOptIn is returned by SetOptedIn(true) when the
	// member has no phone number on file — consent cannot attach to a
	// number that does not exist.
	ErrPhoneRequiredForOptIn = errors.New("notify: cannot opt in to sms without a phone number on file")
	// ErrInvalidPhoneFormat wraps a ParseE164Phone failure at the
	// app-service boundary (app.SettingsService.UpdatePhone), giving the
	// web handler a single sentinel to check for the "show an inline
	// validation message" case, regardless of exactly which format rule
	// the raw input violated.
	ErrInvalidPhoneFormat = errors.New("notify: invalid phone number format")
)

// ContactDirectory is the notify context's narrow port onto a member's SMS
// contact details (NES-139). It is implemented in the adapter layer
// against the member table's phone_e164/sms_opted_in_at columns.
//
// Error contracts:
//   - GetContact returns ErrMemberContactNotFound when memberID is unknown.
//   - SetPhone returns ErrMemberContactNotFound when memberID is unknown.
//   - SetOptedIn(ctx, memberID, true) returns ErrPhoneRequiredForOptIn when
//     the member currently has no phone number on file, and
//     ErrMemberContactNotFound when memberID is unknown.
type ContactDirectory interface {
	// GetContact returns memberID's current contact details.
	GetContact(ctx context.Context, memberID household.MemberID) (*MemberContact, error)

	// SetPhone replaces memberID's phone number. Passing nil clears it.
	// Setting a DIFFERENT phone number (including clearing the old one)
	// always resets SMSOptedIn to false and sms_opted_in_at to NULL —
	// consent is tied to the specific number a member verified control
	// of at opt-in time (docs/aws-sms.md), not carried over to a new one
	// entered later. Setting the SAME phone number the member already
	// has on file is a no-op with respect to opt-in state (does not
	// require re-consent).
	SetPhone(ctx context.Context, memberID household.MemberID, phone *E164Phone) error

	// SetOptedIn sets memberID's SMS opt-in state. Setting true stamps
	// sms_opted_in_at to now(); setting false clears it. Returns
	// ErrPhoneRequiredForOptIn when optIn is true and the member
	// currently has no phone number on file.
	SetOptedIn(ctx context.Context, memberID household.MemberID, optIn bool) error
}
