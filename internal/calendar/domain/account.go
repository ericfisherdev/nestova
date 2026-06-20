package domain

import (
	"context"
	"errors"
	"fmt"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// ErrCalendarAccountNotFound is returned when a calendar account does not exist.
var ErrCalendarAccountNotFound = errors.New("calendar: calendar account not found")

// ErrInvalidCalendarAccount is returned by Validate for a malformed account.
var ErrInvalidCalendarAccount = errors.New("calendar: invalid calendar account")

// CalendarAccount is a member's connected calendar provider. It carries the
// OAuth tokens as ciphertext only — AccessTokenEnc/RefreshTokenEnc are the
// AES-GCM-encrypted bytes persisted in the *_enc columns; the OAuth and sync
// layers encrypt/decrypt at use, so the domain never holds plaintext tokens.
// SyncToken is the provider's incremental-sync cursor (nil until the first sync).
// CalendarIDs are the provider calendar ids this account syncs.
type CalendarAccount struct {
	ID              CalendarAccountID
	MemberID        household.MemberID
	HouseholdID     household.HouseholdID
	Provider        Provider
	AccessTokenEnc  []byte
	RefreshTokenEnc []byte
	TokenExpiry     time.Time
	SyncToken       *string
	CalendarIDs     []string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Validate reports whether the account is well-formed, wrapping
// ErrInvalidCalendarAccount with detail. It checks the invariants the domain
// owns; the encrypted token bytes must be present (encryption is the OAuth
// layer's responsibility) and the expiry must be set.
func (a CalendarAccount) Validate() error {
	if a.ID == (CalendarAccountID{}) {
		return fmt.Errorf("%w: id is required", ErrInvalidCalendarAccount)
	}
	if a.MemberID == (household.MemberID{}) {
		return fmt.Errorf("%w: member id is required", ErrInvalidCalendarAccount)
	}
	if a.HouseholdID == (household.HouseholdID{}) {
		return fmt.Errorf("%w: household id is required", ErrInvalidCalendarAccount)
	}
	if !a.Provider.Valid() {
		return fmt.Errorf("%w: unknown provider %q", ErrInvalidCalendarAccount, a.Provider)
	}
	if len(a.AccessTokenEnc) == 0 {
		return fmt.Errorf("%w: encrypted access token is required", ErrInvalidCalendarAccount)
	}
	if len(a.RefreshTokenEnc) == 0 {
		return fmt.Errorf("%w: encrypted refresh token is required", ErrInvalidCalendarAccount)
	}
	if a.TokenExpiry.IsZero() {
		return fmt.Errorf("%w: token expiry is required", ErrInvalidCalendarAccount)
	}
	return nil
}

// CalendarAccountRepository persists connected calendar accounts.
//
// Persistence contracts (the caller sets identity and valid field values; the
// store sets timestamps):
//   - Create expects a validated CalendarAccount with ID, MemberID, and
//     HouseholdID; it populates CreatedAt/UpdatedAt.
//   - UpdateSyncState rewrites the volatile sync fields — the encrypted access
//     token, token expiry, and sync token — after a token refresh or a sync
//     pass, refreshing UpdatedAt. It does not touch the refresh token or the
//     selected calendar ids.
//
// Error contracts:
//   - Get, GetByMemberProvider, and UpdateSyncState return
//     ErrCalendarAccountNotFound when the account is unknown.
//   - A Create whose HouseholdID/MemberID is unknown returns
//     household.ErrHouseholdNotFound / household.ErrMemberNotFound (mapped from
//     the tenant FK violation by the adapter).
//   - ListByHousehold and ListAll return an empty slice (not an error) when
//     nothing matches.
type CalendarAccountRepository interface {
	Create(ctx context.Context, account *CalendarAccount) error
	Get(ctx context.Context, id CalendarAccountID) (*CalendarAccount, error)
	// GetByMemberProvider returns the member's account for a provider, used by the
	// connect flow to update an existing connection in place rather than duplicate
	// it (the (member_id, provider) unique key). Returns ErrCalendarAccountNotFound
	// when the member has no account for that provider.
	GetByMemberProvider(ctx context.Context, memberID household.MemberID, provider Provider) (*CalendarAccount, error)
	// UpdateTokens rewrites the full token set after a (re)connect — the encrypted
	// access and refresh tokens, the expiry, and the selected calendar ids — and
	// resets the sync token to NULL so the next sync starts fresh. It returns
	// ErrCalendarAccountNotFound when the id is unknown.
	UpdateTokens(ctx context.Context, id CalendarAccountID, accessTokenEnc, refreshTokenEnc []byte, tokenExpiry time.Time, calendarIDs []string) error
	// UpdateSyncState persists a refreshed access token + expiry and the latest
	// sync token for the account (the refresh-token rotation and sync paths). A
	// nil refreshTokenEnc leaves the stored refresh token unchanged; a non-nil
	// value replaces it (some providers rotate the refresh token on refresh). It
	// does not touch the selected calendar ids.
	UpdateSyncState(ctx context.Context, id CalendarAccountID, accessTokenEnc, refreshTokenEnc []byte, tokenExpiry time.Time, syncToken *string) error
	// ListByHousehold returns the household's connected accounts.
	ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*CalendarAccount, error)
	// ListAll returns every connected account across all households; the sync
	// scheduler iterates these.
	ListAll(ctx context.Context) ([]*CalendarAccount, error)
}
