package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/oauth2"

	"github.com/ericfisherdev/nestova/internal/calendar/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// defaultCalendarID is the calendar synced by default after connecting; a
// per-calendar selection UI is future work.
const defaultCalendarID = "primary"

// AccountService errors.
var (
	// ErrNoRefreshToken is returned by Connect when Google does not return a
	// refresh token (e.g. the user previously consented and re-consent was not
	// forced), since without one the access token cannot be refreshed later.
	ErrNoRefreshToken = errors.New("calendar: google did not return a refresh token")
)

// tokenExchanger is the slice of the OAuth client the service depends on (ISP),
// satisfied by adapter.GoogleOAuthClient and faked in tests.
type tokenExchanger interface {
	AuthCodeURL(state string) string
	Exchange(ctx context.Context, code string) (*oauth2.Token, error)
	TokenSource(ctx context.Context, tok *oauth2.Token) oauth2.TokenSource
}

// secretCipher is the slice of the crypto cipher the service depends on,
// satisfied by *crypto.Cipher.
type secretCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// AccountService runs the Google OAuth connect flow and supplies valid access
// tokens for sync. Tokens are encrypted at rest with the injected cipher and the
// OAuth state is HMAC-signed; no token or secret is ever logged (only ids).
type AccountService struct {
	repo      domain.CalendarAccountRepository
	cipher    secretCipher
	exchanger tokenExchanger
	signer    *OAuthStateSigner
	logger    *slog.Logger
}

// NewAccountService constructs the service with injected dependencies.
func NewAccountService(repo domain.CalendarAccountRepository, cipher secretCipher, exchanger tokenExchanger, signer *OAuthStateSigner, logger *slog.Logger) (*AccountService, error) {
	if repo == nil {
		return nil, errors.New("calendar: NewAccountService requires a non-nil repository")
	}
	if cipher == nil {
		return nil, errors.New("calendar: NewAccountService requires a non-nil cipher")
	}
	if exchanger == nil {
		return nil, errors.New("calendar: NewAccountService requires a non-nil exchanger")
	}
	if signer == nil {
		return nil, errors.New("calendar: NewAccountService requires a non-nil state signer")
	}
	if logger == nil {
		return nil, errors.New("calendar: NewAccountService requires a non-nil logger")
	}
	return &AccountService{repo: repo, cipher: cipher, exchanger: exchanger, signer: signer, logger: logger}, nil
}

// AuthURL returns the Google consent URL for memberID, carrying a signed state
// that binds the eventual callback to that member.
func (s *AccountService) AuthURL(memberID household.MemberID, now time.Time) string {
	return s.exchanger.AuthCodeURL(s.signer.Sign(memberID.String(), now))
}

// VerifyState validates the callback state as of now and returns the member id
// it carries, or ErrInvalidState.
func (s *AccountService) VerifyState(state string, now time.Time) (household.MemberID, error) {
	memberStr, err := s.signer.Verify(state, now)
	if err != nil {
		return household.MemberID{}, err
	}
	return household.ParseMemberID(memberStr)
}

// Connect exchanges an authorization code for tokens and stores them encrypted
// for the member's Google account, creating it or updating an existing
// connection in place. It returns ErrNoRefreshToken when Google omits the
// refresh token (re-consent is required to obtain one).
func (s *AccountService) Connect(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID, code string) (domain.CalendarAccountID, error) {
	tok, err := s.exchanger.Exchange(ctx, code)
	if err != nil {
		return domain.CalendarAccountID{}, fmt.Errorf("exchange authorization code: %w", err)
	}
	if tok.RefreshToken == "" {
		return domain.CalendarAccountID{}, ErrNoRefreshToken
	}

	accessEnc, err := s.cipher.Encrypt([]byte(tok.AccessToken))
	if err != nil {
		return domain.CalendarAccountID{}, fmt.Errorf("encrypt access token: %w", err)
	}
	refreshEnc, err := s.cipher.Encrypt([]byte(tok.RefreshToken))
	if err != nil {
		return domain.CalendarAccountID{}, fmt.Errorf("encrypt refresh token: %w", err)
	}
	calendarIDs := []string{defaultCalendarID}

	existing, err := s.repo.GetByMemberProvider(ctx, memberID, domain.ProviderGoogle)
	switch {
	case err == nil:
		if err := s.repo.UpdateTokens(ctx, existing.ID, accessEnc, refreshEnc, tok.Expiry, calendarIDs); err != nil {
			return domain.CalendarAccountID{}, fmt.Errorf("update calendar tokens: %w", err)
		}
		s.logger.InfoContext(ctx, "calendar account reconnected", "account_id", existing.ID.String(), "member_id", memberID.String())
		return existing.ID, nil
	case errors.Is(err, domain.ErrCalendarAccountNotFound):
		account := &domain.CalendarAccount{
			ID:              domain.NewCalendarAccountID(),
			MemberID:        memberID,
			HouseholdID:     householdID,
			Provider:        domain.ProviderGoogle,
			AccessTokenEnc:  accessEnc,
			RefreshTokenEnc: refreshEnc,
			TokenExpiry:     tok.Expiry,
			CalendarIDs:     calendarIDs,
		}
		if err := account.Validate(); err != nil {
			return domain.CalendarAccountID{}, err
		}
		if err := s.repo.Create(ctx, account); err != nil {
			return domain.CalendarAccountID{}, fmt.Errorf("create calendar account: %w", err)
		}
		s.logger.InfoContext(ctx, "calendar account connected", "account_id", account.ID.String(), "member_id", memberID.String())
		return account.ID, nil
	default:
		return domain.CalendarAccountID{}, fmt.Errorf("look up existing calendar account: %w", err)
	}
}

// ValidAccessToken returns a currently-valid Google access token for the
// account, decrypting the stored tokens, refreshing transparently when the
// access token has expired, and re-persisting the rotated access token + expiry.
func (s *AccountService) ValidAccessToken(ctx context.Context, accountID domain.CalendarAccountID) (string, error) {
	account, err := s.repo.Get(ctx, accountID)
	if err != nil {
		return "", err
	}
	access, err := s.cipher.Decrypt(account.AccessTokenEnc)
	if err != nil {
		return "", fmt.Errorf("decrypt access token: %w", err)
	}
	refresh, err := s.cipher.Decrypt(account.RefreshTokenEnc)
	if err != nil {
		return "", fmt.Errorf("decrypt refresh token: %w", err)
	}

	stored := &oauth2.Token{
		AccessToken:  string(access),
		RefreshToken: string(refresh),
		Expiry:       account.TokenExpiry,
	}
	fresh, err := s.exchanger.TokenSource(ctx, stored).Token()
	if err != nil {
		return "", fmt.Errorf("obtain valid access token: %w", err)
	}

	if fresh.AccessToken != stored.AccessToken {
		newAccessEnc, err := s.cipher.Encrypt([]byte(fresh.AccessToken))
		if err != nil {
			return "", fmt.Errorf("encrypt refreshed access token: %w", err)
		}
		// Re-persist only the access token + expiry; the sync token is unchanged.
		if err := s.repo.UpdateSyncState(ctx, account.ID, newAccessEnc, fresh.Expiry, account.SyncToken); err != nil {
			return "", fmt.Errorf("persist refreshed access token: %w", err)
		}
		s.logger.InfoContext(ctx, "calendar access token refreshed", "account_id", account.ID.String())
	}
	return fresh.AccessToken, nil
}
