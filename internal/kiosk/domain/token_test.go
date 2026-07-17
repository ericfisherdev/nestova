package domain_test

import (
	"strings"
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

func TestGenerateActivationCode_ProducesDistinctHyphenatedCodes(t *testing.T) {
	a, err := domain.GenerateActivationCode()
	if err != nil {
		t.Fatalf("GenerateActivationCode: %v", err)
	}
	b, err := domain.GenerateActivationCode()
	if err != nil {
		t.Fatalf("GenerateActivationCode: %v", err)
	}
	if a == b {
		t.Fatal("two calls to GenerateActivationCode produced the same code")
	}
	// 10 alphabet characters plus 2 separating hyphens (grouped 4-4-2).
	if len(a) != 12 {
		t.Errorf("GenerateActivationCode() length = %d, want 12 (XXXX-XXXX-XX): %q", len(a), a)
	}
	for _, r := range a {
		if r == '-' {
			continue
		}
		if strings.ContainsRune("OIL", r) {
			t.Errorf("GenerateActivationCode() must never itself produce a confusable O/I/L: %q", a)
		}
	}
}

func TestNormalizeActivationCode(t *testing.T) {
	tests := map[string]struct {
		in   string
		want string
	}{
		"already normalized":       {"ABCDEFGHJK", "ABCDEFGHJK"},
		"lowercase":                {"abcd-efgh-jk", "ABCDEFGHJK"},
		"mixed spacing and case":   {" AbCd EfGh Jk ", "ABCDEFGHJK"},
		"hyphens and spaces mixed": {"ABCD- EFGH -JK", "ABCDEFGHJK"},
		"O alias for 0":            {"AB0D-EFGH-JK", "AB0DEFGHJK"}, // baseline, no O present
		"O typed for 0":            {"ABOD-EFGH-JK", "AB0DEFGHJK"},
		"lowercase o typed for 0":  {"abod-efgh-jk", "AB0DEFGHJK"},
		"I typed for 1":            {"AB1D-EFGH-JK", "AB1DEFGHJK"}, // baseline, no I present
		"I typed for 1 (alias)":    {"ABID-EFGH-JK", "AB1DEFGHJK"},
		"L typed for 1 (alias)":    {"ABLD-EFGH-JK", "AB1DEFGHJK"},
		"lowercase l typed for 1":  {"abld-efgh-jk", "AB1DEFGHJK"},
		"empty":                    {"", ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := domain.NormalizeActivationCode(tt.in); got != tt.want {
				t.Errorf("NormalizeActivationCode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNormalizeActivationCode_AliasesMatchGeneratedCode is the round-trip
// regression test for Crockford aliasing: a code containing O/I/L in place of
// 0/1 (as a person might mistype from the generated code) must still hash
// identically to the code as generated and shown on the settings page.
func TestNormalizeActivationCode_AliasesMatchGeneratedCode(t *testing.T) {
	generated := "AB0D-EF1H-JK" // synthetic stand-in containing a 0 and a 1
	typedWithAliases := "ABOD-EFIH-JK"
	if domain.NormalizeActivationCode(generated) != domain.NormalizeActivationCode(typedWithAliases) {
		t.Errorf("normalized forms diverge: %q vs %q",
			domain.NormalizeActivationCode(generated), domain.NormalizeActivationCode(typedWithAliases))
	}
}
