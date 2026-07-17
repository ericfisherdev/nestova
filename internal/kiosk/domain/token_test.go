package domain_test

import (
	"testing"

	"github.com/ericfisherdev/nestova/internal/kiosk/domain"
)

func TestGenerateToken_ProducesDistinctHighEntropyTokens(t *testing.T) {
	a, err := domain.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	b, err := domain.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if a == b {
		t.Fatal("two calls to GenerateToken produced the same token")
	}
	// 32 bytes hex-encoded is 64 characters.
	if len(a) != 64 {
		t.Errorf("GenerateToken() length = %d, want 64", len(a))
	}
}

func TestHashToken_Deterministic(t *testing.T) {
	raw := "fixed-raw-token-value"
	first := domain.HashToken(raw)
	second := domain.HashToken(raw)
	if first != second {
		t.Fatalf("HashToken is not deterministic for the same input: %q != %q", first, second)
	}
	if first == domain.HashToken("different-value") {
		t.Fatal("HashToken produced the same hash for different inputs")
	}
	// SHA-256 hex digest is 64 characters.
	if len(first) != 64 {
		t.Errorf("HashToken length = %d, want 64", len(first))
	}
}

func TestTokensMatch(t *testing.T) {
	raw, err := domain.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	hash := domain.HashToken(raw)

	if !domain.TokensMatch(raw, hash) {
		t.Error("TokensMatch(raw, HashToken(raw)) = false, want true")
	}
	if domain.TokensMatch("wrong-token", hash) {
		t.Error("TokensMatch(wrong, hash) = true, want false")
	}
	if domain.TokensMatch(raw, "not-a-real-hash") {
		t.Error("TokensMatch against a malformed hash should never match")
	}
}
