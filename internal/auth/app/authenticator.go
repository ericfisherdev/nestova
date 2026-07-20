// Package app contains the auth context's application services. The
// Authenticator orchestrates credential lookup and password verification
// without depending on any infrastructure package directly.
package app

import (
	"context"
	"errors"
	"fmt"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// passwordHasher is the minimal seam over argon2id password hashing (ISP): the
// auth services need only to derive and check hashes, not to know the cost
// parameters or the PHC encoding. Satisfied by *crypto.Hasher.
//
// It exists so tests can inject a cheap-cost hasher. Hashing at the production
// cost — 64 MiB per derivation, ten derivations per recovery-code batch —
// dominated this package's test runtime. Because the cost is recorded in the
// PHC string and read back by Verify, a cheaply-hashed fixture verifies through
// exactly the same code path as a production hash.
type passwordHasher interface {
	Hash(password string) (string, error)
	Verify(password, encoded string) (bool, error)
}

// Authenticator verifies login credentials and returns the authenticated
// MemberID. It is the sole entry point for the password-based login flow.
type Authenticator struct {
	repo   authdomain.CredentialRepository
	hasher passwordHasher
	// dummyHash normalizes Login timing when the email is unknown: verifying
	// against it makes the "no such user" path cost about as much as the
	// "wrong password" path, preventing user enumeration via response timing.
	// The plaintext is irrelevant and never matches a real login.
	//
	// Derived once per Authenticator in New rather than once per process in a
	// package-level var, so that merely importing this package costs no argon2
	// derivation and so that its cost tracks the injected hasher.
	dummyHash string
}

// New constructs an Authenticator with the supplied credential repository and
// password hasher. Production callers pass
// crypto.NewHasher(crypto.DefaultParams()).
func New(repo authdomain.CredentialRepository, hasher passwordHasher) *Authenticator {
	if repo == nil {
		panic("app: New requires a non-nil CredentialRepository")
	}
	if hasher == nil {
		panic("app: New requires a non-nil password hasher")
	}
	dummy, err := hasher.Hash("nestova-timing-equalizer")
	if err != nil {
		// Hash only fails if the system RNG is unavailable; that is fatal at
		// startup and must not be silently ignored.
		panic("app: failed to initialize argon2 dummy hash: " + err.Error())
	}
	return &Authenticator{repo: repo, hasher: hasher, dummyHash: dummy}
}

// Login looks up the credential for email, verifies password against the
// stored hash, and returns the authenticated MemberID on success.
//
// On any failure — unknown email, wrong password, or internal error — Login
// returns authdomain.ErrInvalidCredentials with no further detail to prevent
// user enumeration.
func (a *Authenticator) Login(ctx context.Context, email, password string) (household.MemberID, error) {
	cred, err := a.repo.FindByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, authdomain.ErrInvalidCredentials) {
			// Unknown email: run a dummy verification so this path costs about
			// the same as the wrong-password path, preventing timing-based
			// enumeration. Then return the generic sentinel.
			_, _ = a.hasher.Verify(password, a.dummyHash)
			return household.MemberID{}, authdomain.ErrInvalidCredentials
		}
		// A genuine lookup failure (e.g. database outage) is surfaced rather than
		// masked as invalid credentials, so the handler can return 500 not 401.
		return household.MemberID{}, fmt.Errorf("authenticate: %w", err)
	}

	ok, err := a.hasher.Verify(password, cred.PasswordHash)
	if err != nil || !ok {
		return household.MemberID{}, authdomain.ErrInvalidCredentials
	}

	return cred.MemberID, nil
}
