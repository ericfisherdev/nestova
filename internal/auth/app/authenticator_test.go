package app_test

import (
	"context"
	"errors"
	"testing"

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

func (f *fakeRepo) SetPassword(_ context.Context, _ household.MemberID, _, _ string) error {
	return nil
}

// newFixture creates a fakeRepo with one seeded credential for email using the
// provided plaintext password. crypto.Hash is called here so the fixture is a
// realistic PHC string.
func newFixture(t *testing.T, email, password string) (*fakeRepo, household.MemberID) {
	t.Helper()
	hash, err := crypto.Hash(password)
	if err != nil {
		t.Fatalf("crypto.Hash: %v", err)
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
	authn := app.New(repo)

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
	authn := app.New(repo)

	_, err := authn.Login(context.Background(), "bob@example.com", "wrongpassword")
	if !errors.Is(err, authdomain.ErrInvalidCredentials) {
		t.Errorf("Login(wrong password) error = %v, want ErrInvalidCredentials", err)
	}
}

func TestLoginUnknownEmail(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{credentials: make(map[string]*authdomain.Credential)}
	authn := app.New(repo)

	_, err := authn.Login(context.Background(), "nobody@example.com", "anypassword")
	if !errors.Is(err, authdomain.ErrInvalidCredentials) {
		t.Errorf("Login(unknown email) error = %v, want ErrInvalidCredentials", err)
	}
}
