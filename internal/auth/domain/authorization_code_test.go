package domain_test

import (
	"errors"
	"testing"
	"time"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func TestGenerateAuthorizationCode_ProducesDistinctHighEntropyCodes(t *testing.T) {
	a, err := authdomain.GenerateAuthorizationCode()
	if err != nil {
		t.Fatalf("GenerateAuthorizationCode: %v", err)
	}
	b, err := authdomain.GenerateAuthorizationCode()
	if err != nil {
		t.Fatalf("GenerateAuthorizationCode: %v", err)
	}
	if a == b {
		t.Fatal("two calls to GenerateAuthorizationCode produced the same code")
	}
	// 32 bytes hex-encoded is 64 characters.
	if len(a) != 64 {
		t.Errorf("GenerateAuthorizationCode() length = %d, want 64", len(a))
	}
}

func TestHashAuthorizationCode_Deterministic(t *testing.T) {
	raw := "fixed-raw-authorization-code"
	first := authdomain.HashAuthorizationCode(raw)
	second := authdomain.HashAuthorizationCode(raw)
	if first != second {
		t.Fatalf("HashAuthorizationCode is not deterministic for the same input: %q != %q", first, second)
	}
	if first == authdomain.HashAuthorizationCode("different-value") {
		t.Fatal("HashAuthorizationCode produced the same hash for different inputs")
	}
	// SHA-256 hex digest is 64 characters.
	if len(first) != 64 {
		t.Errorf("HashAuthorizationCode length = %d, want 64", len(first))
	}
}

func validAuthorizationCode() *authdomain.AuthorizationCode {
	return &authdomain.AuthorizationCode{
		ID:          authdomain.NewAuthorizationCodeID(),
		MemberID:    household.NewMemberID(),
		ClientID:    "nestorage-household-1",
		RedirectURI: "https://nestorage.example.ts.net/federation/callback",
		CodeHash:    authdomain.HashAuthorizationCode("some-raw-code"),
		ExpiresAt:   time.Now().Add(authdomain.AuthorizationCodeTTL),
	}
}

func TestAuthorizationCodeValidate(t *testing.T) {
	if err := validAuthorizationCode().Validate(); err != nil {
		t.Fatalf("valid code rejected: %v", err)
	}

	zeroID := validAuthorizationCode()
	zeroID.ID = authdomain.AuthorizationCodeID{}
	if !errors.Is(zeroID.Validate(), authdomain.ErrInvalidAuthorizationCode) {
		t.Error("zero id accepted")
	}

	zeroMember := validAuthorizationCode()
	zeroMember.MemberID = household.MemberID{}
	if !errors.Is(zeroMember.Validate(), authdomain.ErrInvalidAuthorizationCode) {
		t.Error("zero member id accepted")
	}

	blankClientID := validAuthorizationCode()
	blankClientID.ClientID = "   "
	if !errors.Is(blankClientID.Validate(), authdomain.ErrInvalidAuthorizationCode) {
		t.Error("blank client id accepted")
	}

	blankRedirect := validAuthorizationCode()
	blankRedirect.RedirectURI = "   "
	if !errors.Is(blankRedirect.Validate(), authdomain.ErrInvalidAuthorizationCode) {
		t.Error("blank redirect uri accepted")
	}

	blankHash := validAuthorizationCode()
	blankHash.CodeHash = ""
	if !errors.Is(blankHash.Validate(), authdomain.ErrInvalidAuthorizationCode) {
		t.Error("blank code hash accepted")
	}

	zeroExpiry := validAuthorizationCode()
	zeroExpiry.ExpiresAt = time.Time{}
	if !errors.Is(zeroExpiry.Validate(), authdomain.ErrInvalidAuthorizationCode) {
		t.Error("zero expires at accepted")
	}
}

func TestAuthorizationCodeUsable(t *testing.T) {
	now := time.Now()

	fresh := validAuthorizationCode()
	fresh.ExpiresAt = now.Add(time.Minute)
	if !fresh.Usable(now) {
		t.Error("a freshly issued, unused code should be usable")
	}

	expired := validAuthorizationCode()
	expired.ExpiresAt = now.Add(-time.Second)
	if expired.Usable(now) {
		t.Error("an expired code should not be usable")
	}

	used := validAuthorizationCode()
	used.ExpiresAt = now.Add(time.Minute)
	usedAt := now.Add(-time.Second)
	used.UsedAt = &usedAt
	if used.Usable(now) {
		t.Error("an already-used code should not be usable")
	}
}
