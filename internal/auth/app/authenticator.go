// Package app contains the auth context's application services. The
// Authenticator orchestrates credential lookup and password verification
// without depending on any infrastructure package directly.
package app

import (
	"context"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
)

// Authenticator verifies login credentials and returns the authenticated
// MemberID. It is the sole entry point for the password-based login flow.
type Authenticator struct {
	repo authdomain.CredentialRepository
}

// New constructs an Authenticator with the supplied credential repository.
func New(repo authdomain.CredentialRepository) *Authenticator {
	if repo == nil {
		panic("app: New requires a non-nil CredentialRepository")
	}
	return &Authenticator{repo: repo}
}

// dummyHash is a precomputed argon2id hash used to normalize Login timing when
// the email is unknown: verifying against it makes the "no such user" path take
// about as long as the "wrong password" path, preventing user enumeration via
// response timing. The plaintext is irrelevant and never matches a real login.
var dummyHash = func() string {
	h, _ := crypto.Hash("nestova-timing-equalizer")
	return h
}()

// Login looks up the credential for email, verifies password against the
// stored hash, and returns the authenticated MemberID on success.
//
// On any failure — unknown email, wrong password, or internal error — Login
// returns authdomain.ErrInvalidCredentials with no further detail to prevent
// user enumeration.
func (a *Authenticator) Login(ctx context.Context, email, password string) (household.MemberID, error) {
	cred, err := a.repo.FindByEmail(ctx, email)
	if err != nil {
		// Run a dummy verification so the unknown-email path costs roughly the
		// same as the wrong-password path, preventing timing-based enumeration.
		_, _ = crypto.Verify(password, dummyHash)
		// FindByEmail already returns ErrInvalidCredentials for missing accounts;
		// collapse any other error to the same sentinel to keep the surface generic.
		return household.MemberID{}, authdomain.ErrInvalidCredentials
	}

	ok, err := crypto.Verify(password, cred.PasswordHash)
	if err != nil || !ok {
		return household.MemberID{}, authdomain.ErrInvalidCredentials
	}

	return cred.MemberID, nil
}
