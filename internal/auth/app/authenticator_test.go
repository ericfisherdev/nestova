package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/crypto/cryptotest"

	"github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
)

// fakeRepo is an in-memory CredentialRepository used for hermetic unit tests.
type fakeRepo struct {
	// credentials maps lowercase email → Credential.
	credentials map[string]*authdomain.Credential
}

func (f *fakeRepo) FindByEmail(_ context.Context, email string) (*authdomain.Credential, error) {
	c, ok := f.credentials[email]
	if !ok {
		return nil, authdomain.ErrInvalidCredentials
	}
	return c, nil
}

func (f *fakeRepo) FindByMemberID(_ context.Context, memberID household.MemberID) (*authdomain.Credential, error) {
	for _, c := range f.credentials {
		if c.MemberID == memberID {
			return c, nil
		}
	}
	return nil, authdomain.ErrInvalidCredentials
}

func (f *fakeRepo) SetPassword(_ context.Context, _ household.MemberID, _, _ string) error {
	return nil
}

// newFixture creates a fakeRepo with one seeded credential for email using the
// provided plaintext password. The fixture is hashed at cheap test parameters:
// it is still a realistic PHC string, and Verify reads the cost back out of it,
// so the login paths under test behave exactly as they do in production.
func newFixture(t *testing.T, email, password string) (*fakeRepo, household.MemberID) {
	t.Helper()
	hash, err := cryptotest.Hasher().Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	memberID := household.NewMemberID()
	repo := &fakeRepo{
		credentials: map[string]*authdomain.Credential{
			email: {MemberID: memberID, PasswordHash: hash},
		},
	}
	return repo, memberID
}

func TestLoginSuccess(t *testing.T) {
	t.Parallel()
	const (
		email    = "alice@example.com"
		password = "correct-password"
	)
	repo, wantID := newFixture(t, email, password)
	authn := app.New(repo, cryptotest.Hasher())

	gotID, err := authn.Login(context.Background(), email, password)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if gotID != wantID {
		t.Errorf("Login MemberID = %v, want %v", gotID, wantID)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	t.Parallel()
	repo, _ := newFixture(t, "bob@example.com", "rightpassword")
	authn := app.New(repo, cryptotest.Hasher())

	_, err := authn.Login(context.Background(), "bob@example.com", "wrongpassword")
	if !errors.Is(err, authdomain.ErrInvalidCredentials) {
		t.Errorf("Login(wrong password) error = %v, want ErrInvalidCredentials", err)
	}
}

func TestLoginUnknownEmail(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{credentials: make(map[string]*authdomain.Credential)}
	authn := app.New(repo, cryptotest.Hasher())

	_, err := authn.Login(context.Background(), "nobody@example.com", "anypassword")
	if !errors.Is(err, authdomain.ErrInvalidCredentials) {
		t.Errorf("Login(unknown email) error = %v, want ErrInvalidCredentials", err)
	}
}

// countingHasher wraps a real hasher and records how many derivations it is
// asked to perform, so tests can assert on argon2 usage rather than on wall
// time (which would be flaky).
type countingHasher struct {
	inner    *crypto.Hasher
	hashes   int
	verifies int
}

func newCountingHasher() *countingHasher {
	return &countingHasher{inner: cryptotest.Hasher()}
}

func (c *countingHasher) Hash(password string) (string, error) {
	c.hashes++
	return c.inner.Hash(password)
}

func (c *countingHasher) Verify(password, encoded string) (bool, error) {
	c.verifies++
	return c.inner.Verify(password, encoded)
}

// TestNewDerivesTimingDummyOncePerAuthenticator pins the fix for the dummy hash
// having been a package-level var: it used to be derived at package init, so
// merely importing this package cost a 64 MiB argon2 derivation whether or not
// any login ran. It is now derived once per Authenticator, from the injected
// hasher.
func TestNewDerivesTimingDummyOncePerAuthenticator(t *testing.T) {
	t.Parallel()
	counter := newCountingHasher()
	repo := &fakeRepo{credentials: make(map[string]*authdomain.Credential)}

	app.New(repo, counter)

	if counter.hashes != 1 {
		t.Errorf("New performed %d derivations, want exactly 1 (the timing dummy)", counter.hashes)
	}
}

// TestLoginUnknownEmailStillVerifiesForTiming guards the user-enumeration
// defence. The unknown-email path must perform a verification against the dummy
// hash so it costs about as much as the wrong-password path; if a refactor
// dropped that call, the two paths would become distinguishable by response
// time. Asserting on the call count rather than on elapsed time keeps this
// deterministic.
func TestLoginUnknownEmailStillVerifiesForTiming(t *testing.T) {
	t.Parallel()
	counter := newCountingHasher()
	repo := &fakeRepo{credentials: make(map[string]*authdomain.Credential)}
	authn := app.New(repo, counter)

	before := counter.verifies
	_, err := authn.Login(context.Background(), "nobody@example.com", "anypassword")
	if !errors.Is(err, authdomain.ErrInvalidCredentials) {
		t.Fatalf("Login(unknown email) error = %v, want ErrInvalidCredentials", err)
	}
	if got := counter.verifies - before; got != 1 {
		t.Errorf("unknown-email path performed %d verifications, want 1 (the timing equalizer)", got)
	}
}
