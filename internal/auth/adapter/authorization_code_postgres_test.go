package adapter_test

import (
	"errors"
	"testing"
	"time"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func newAuthorizationCode(memberID household.MemberID, clientID, redirectURI, rawCode string, expiresAt time.Time) *authdomain.AuthorizationCode {
	return &authdomain.AuthorizationCode{
		ID:          authdomain.NewAuthorizationCodeID(),
		MemberID:    memberID,
		ClientID:    clientID,
		RedirectURI: redirectURI,
		CodeHash:    authdomain.HashAuthorizationCode(rawCode),
		ExpiresAt:   expiresAt,
	}
}

func TestAuthorizationCodeRepositoryCreateAndConsume(t *testing.T) {
	_, hhRepo, pool := newTestRepos(t)
	repo := authadapter.NewAuthorizationCodeRepository(pool)
	memberID := seedMember(t, hhRepo)
	ctx := testCtx(t)

	code := newAuthorizationCode(memberID, "nestorage-household-1", "https://nestorage.example.ts.net/federation/callback", "raw-code-1", time.Now().Add(authdomain.AuthorizationCodeTTL))
	if err := repo.Create(ctx, code); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if code.CreatedAt.IsZero() {
		t.Fatal("Create did not populate CreatedAt")
	}

	consumed, err := repo.Consume(ctx, authdomain.HashAuthorizationCode("raw-code-1"), time.Now())
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if consumed.ID != code.ID || consumed.MemberID != memberID ||
		consumed.ClientID != "nestorage-household-1" || consumed.RedirectURI != "https://nestorage.example.ts.net/federation/callback" {
		t.Fatalf("Consume = %+v", consumed)
	}
	if consumed.UsedAt == nil {
		t.Fatal("Consume did not report the code as used")
	}
}

func TestAuthorizationCodeRepositoryConsumeUnknownCode(t *testing.T) {
	_, _, pool := newTestRepos(t)
	repo := authadapter.NewAuthorizationCodeRepository(pool)
	ctx := testCtx(t)

	if _, err := repo.Consume(ctx, authdomain.HashAuthorizationCode("never-issued"), time.Now()); !errors.Is(err, authdomain.ErrAuthorizationCodeNotFound) {
		t.Errorf("Consume(unknown) error = %v, want ErrAuthorizationCodeNotFound", err)
	}
}

func TestAuthorizationCodeRepositoryConsumeCannotBeReplayed(t *testing.T) {
	_, hhRepo, pool := newTestRepos(t)
	repo := authadapter.NewAuthorizationCodeRepository(pool)
	memberID := seedMember(t, hhRepo)
	ctx := testCtx(t)

	code := newAuthorizationCode(memberID, "nestorage-household-1", "https://nestorage.example.ts.net/federation/callback", "raw-code-2", time.Now().Add(authdomain.AuthorizationCodeTTL))
	if err := repo.Create(ctx, code); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := repo.Consume(ctx, authdomain.HashAuthorizationCode("raw-code-2"), time.Now()); err != nil {
		t.Fatalf("first Consume: %v", err)
	}
	if _, err := repo.Consume(ctx, authdomain.HashAuthorizationCode("raw-code-2"), time.Now()); !errors.Is(err, authdomain.ErrAuthorizationCodeUsed) {
		t.Errorf("second Consume of the same code error = %v, want ErrAuthorizationCodeUsed", err)
	}
}

func TestAuthorizationCodeRepositoryConsumeExpiredCode(t *testing.T) {
	_, hhRepo, pool := newTestRepos(t)
	repo := authadapter.NewAuthorizationCodeRepository(pool)
	memberID := seedMember(t, hhRepo)
	ctx := testCtx(t)

	// ExpiresAt in the past: the code is created already expired.
	code := newAuthorizationCode(memberID, "nestorage-household-1", "https://nestorage.example.ts.net/federation/callback", "raw-code-3", time.Now().Add(-time.Minute))
	if err := repo.Create(ctx, code); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := repo.Consume(ctx, authdomain.HashAuthorizationCode("raw-code-3"), time.Now()); !errors.Is(err, authdomain.ErrAuthorizationCodeExpired) {
		t.Errorf("Consume(expired) error = %v, want ErrAuthorizationCodeExpired", err)
	}
}

func TestAuthorizationCodeRepositoryCreateUnknownMemberFails(t *testing.T) {
	_, _, pool := newTestRepos(t)
	repo := authadapter.NewAuthorizationCodeRepository(pool)
	ctx := testCtx(t)

	code := newAuthorizationCode(household.NewMemberID(), "nestorage-household-1", "https://nestorage.example.ts.net/federation/callback", "raw-code-4", time.Now().Add(authdomain.AuthorizationCodeTTL))
	if err := repo.Create(ctx, code); !errors.Is(err, household.ErrMemberNotFound) {
		t.Errorf("Create with unknown member error = %v, want ErrMemberNotFound", err)
	}
}
