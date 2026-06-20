package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/ericfisherdev/nestova/internal/calendar/app"
	calendardomain "github.com/ericfisherdev/nestova/internal/calendar/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

type fakeAccountRepo struct {
	created       *calendardomain.CalendarAccount
	existing      *calendardomain.CalendarAccount // GetByMemberProvider result
	getResult     *calendardomain.CalendarAccount // Get result
	updatedTokens bool
	syncCalled    bool
	syncAccessEnc []byte
}

func (f *fakeAccountRepo) Create(_ context.Context, a *calendardomain.CalendarAccount) error {
	f.created = a
	return nil
}

func (f *fakeAccountRepo) Get(context.Context, calendardomain.CalendarAccountID) (*calendardomain.CalendarAccount, error) {
	if f.getResult != nil {
		return f.getResult, nil
	}
	return nil, calendardomain.ErrCalendarAccountNotFound
}

func (f *fakeAccountRepo) GetByMemberProvider(context.Context, household.MemberID, calendardomain.Provider) (*calendardomain.CalendarAccount, error) {
	if f.existing != nil {
		return f.existing, nil
	}
	return nil, calendardomain.ErrCalendarAccountNotFound
}

func (f *fakeAccountRepo) UpdateTokens(context.Context, calendardomain.CalendarAccountID, []byte, []byte, time.Time, []string) error {
	f.updatedTokens = true
	return nil
}

func (f *fakeAccountRepo) UpdateSyncState(_ context.Context, _ calendardomain.CalendarAccountID, accessTokenEnc []byte, _ time.Time, _ *string) error {
	f.syncCalled = true
	f.syncAccessEnc = accessTokenEnc
	return nil
}

func (f *fakeAccountRepo) ListByHousehold(context.Context, household.HouseholdID) ([]*calendardomain.CalendarAccount, error) {
	return nil, nil
}

func (f *fakeAccountRepo) ListAll(context.Context) ([]*calendardomain.CalendarAccount, error) {
	return nil, nil
}

type fakeExchanger struct {
	exchangeTok *oauth2.Token
	exchangeErr error
	sourceTok   *oauth2.Token // what TokenSource().Token() returns; defaults to the seed
}

func (f *fakeExchanger) AuthCodeURL(state string) string { return "https://google/auth?state=" + state }

func (f *fakeExchanger) Exchange(context.Context, string) (*oauth2.Token, error) {
	if f.exchangeErr != nil {
		return nil, f.exchangeErr
	}
	return f.exchangeTok, nil
}

func (f *fakeExchanger) TokenSource(_ context.Context, seed *oauth2.Token) oauth2.TokenSource {
	if f.sourceTok != nil {
		return oauth2.StaticTokenSource(f.sourceTok)
	}
	return oauth2.StaticTokenSource(seed)
}

func mustService(t *testing.T, repo calendardomain.CalendarAccountRepository, exch *fakeExchanger, cipher *crypto.Cipher) *app.AccountService {
	t.Helper()
	signer, err := app.NewOAuthStateSigner([]byte("state-key"))
	if err != nil {
		t.Fatalf("NewOAuthStateSigner: %v", err)
	}
	svc, err := app.NewAccountService(repo, cipher, exch, signer, discardLogger())
	if err != nil {
		t.Fatalf("NewAccountService: %v", err)
	}
	return svc
}

func TestConnectStoresEncryptedTokens(t *testing.T) {
	cipher := testCipher(t)
	repo := &fakeAccountRepo{}
	exch := &fakeExchanger{exchangeTok: &oauth2.Token{
		AccessToken: "the-access-token", RefreshToken: "the-refresh-token", Expiry: time.Now().Add(time.Hour),
	}}
	svc := mustService(t, repo, exch, cipher)

	memberID, householdID := household.NewMemberID(), household.NewHouseholdID()
	if _, err := svc.Connect(context.Background(), memberID, householdID, "code"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if repo.created == nil {
		t.Fatal("Connect did not create an account")
	}
	// Stored ciphertext must not be the plaintext, and must decrypt back.
	if string(repo.created.AccessTokenEnc) == "the-access-token" {
		t.Fatal("access token stored as plaintext")
	}
	access, err := cipher.Decrypt(repo.created.AccessTokenEnc)
	if err != nil || string(access) != "the-access-token" {
		t.Fatalf("decrypt access = %q, %v; want the-access-token", access, err)
	}
	refresh, err := cipher.Decrypt(repo.created.RefreshTokenEnc)
	if err != nil || string(refresh) != "the-refresh-token" {
		t.Fatalf("decrypt refresh = %q, %v; want the-refresh-token", refresh, err)
	}
}

func TestConnectUpdatesExistingAccount(t *testing.T) {
	cipher := testCipher(t)
	existing := &calendardomain.CalendarAccount{ID: calendardomain.NewCalendarAccountID()}
	repo := &fakeAccountRepo{existing: existing}
	exch := &fakeExchanger{exchangeTok: &oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}}
	svc := mustService(t, repo, exch, cipher)

	id, err := svc.Connect(context.Background(), household.NewMemberID(), household.NewHouseholdID(), "code")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !repo.updatedTokens || repo.created != nil {
		t.Fatal("Connect on an existing account should UpdateTokens, not Create")
	}
	if id != existing.ID {
		t.Fatalf("Connect returned id %s, want existing %s", id, existing.ID)
	}
}

func TestConnectRejectsMissingRefreshToken(t *testing.T) {
	repo := &fakeAccountRepo{}
	exch := &fakeExchanger{exchangeTok: &oauth2.Token{AccessToken: "a", Expiry: time.Now().Add(time.Hour)}}
	svc := mustService(t, repo, exch, testCipher(t))
	if _, err := svc.Connect(context.Background(), household.NewMemberID(), household.NewHouseholdID(), "code"); !errors.Is(err, app.ErrNoRefreshToken) {
		t.Fatalf("Connect without refresh token = %v, want ErrNoRefreshToken", err)
	}
}

func storedAccount(t *testing.T, cipher *crypto.Cipher, access, refresh string, expiry time.Time) *calendardomain.CalendarAccount {
	t.Helper()
	accessEnc, err := cipher.Encrypt([]byte(access))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	refreshEnc, err := cipher.Encrypt([]byte(refresh))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return &calendardomain.CalendarAccount{
		ID:              calendardomain.NewCalendarAccountID(),
		AccessTokenEnc:  accessEnc,
		RefreshTokenEnc: refreshEnc,
		TokenExpiry:     expiry,
	}
}

func TestValidAccessTokenRefreshesAndRepersists(t *testing.T) {
	cipher := testCipher(t)
	account := storedAccount(t, cipher, "old-access", "refresh", time.Now().Add(-time.Hour)) // expired
	repo := &fakeAccountRepo{getResult: account}
	// The token source rotates the access token (simulating a refresh).
	exch := &fakeExchanger{sourceTok: &oauth2.Token{AccessToken: "new-access", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}}
	svc := mustService(t, repo, exch, cipher)

	tok, err := svc.ValidAccessToken(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("ValidAccessToken: %v", err)
	}
	if tok != "new-access" {
		t.Fatalf("ValidAccessToken = %q, want new-access", tok)
	}
	if !repo.syncCalled {
		t.Fatal("rotated token was not re-persisted via UpdateSyncState")
	}
	got, err := cipher.Decrypt(repo.syncAccessEnc)
	if err != nil || string(got) != "new-access" {
		t.Fatalf("re-persisted access = %q, %v; want new-access", got, err)
	}
}

func TestValidAccessTokenNoRotationNoRepersist(t *testing.T) {
	cipher := testCipher(t)
	account := storedAccount(t, cipher, "current-access", "refresh", time.Now().Add(time.Hour)) // valid
	repo := &fakeAccountRepo{getResult: account}
	// Token source returns the same access token (no refresh needed).
	exch := &fakeExchanger{sourceTok: &oauth2.Token{AccessToken: "current-access", RefreshToken: "refresh", Expiry: account.TokenExpiry}}
	svc := mustService(t, repo, exch, cipher)

	tok, err := svc.ValidAccessToken(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("ValidAccessToken: %v", err)
	}
	if tok != "current-access" {
		t.Fatalf("ValidAccessToken = %q, want current-access", tok)
	}
	if repo.syncCalled {
		t.Fatal("UpdateSyncState should not be called when the token did not rotate")
	}
}
