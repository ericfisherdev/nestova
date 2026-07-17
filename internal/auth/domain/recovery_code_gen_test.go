package domain_test

import (
	"strings"
	"testing"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
)

// recoveryCodeSymbolAlphabet mirrors domain's unexported recoveryCodeAlphabet
// for black-box assertions (Crockford's Base32, no 0/O or 1/I/L confusion).
const recoveryCodeSymbolAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func TestGenerateRecoveryCode_FormatIsFourDashSeparatedGroupsOfFour(t *testing.T) {
	code, err := authdomain.GenerateRecoveryCode()
	if err != nil {
		t.Fatalf("GenerateRecoveryCode: %v", err)
	}

	// 16 Base32 symbols (80 bits / 5 bits-per-symbol) + 3 dashes = 19 chars.
	// This is the direct regression check for the entropy bug: a
	// byte-per-symbol scheme would produce only 10 symbols (50 bits) here,
	// not 16 (80 bits).
	if len(code) != 19 {
		t.Fatalf("GenerateRecoveryCode length = %d (%q), want 19 (four groups of four symbols)", len(code), code)
	}
	groups := strings.Split(code, "-")
	if len(groups) != 4 {
		t.Fatalf("GenerateRecoveryCode has %d dash-separated groups (%q), want 4", len(groups), code)
	}
	for _, g := range groups {
		if len(g) != 4 {
			t.Errorf("group %q has length %d, want 4", g, len(g))
		}
	}
}

func TestGenerateRecoveryCode_UsesOnlyTheCrockfordAlphabet(t *testing.T) {
	code, err := authdomain.GenerateRecoveryCode()
	if err != nil {
		t.Fatalf("GenerateRecoveryCode: %v", err)
	}
	for _, r := range strings.ReplaceAll(code, "-", "") {
		if !strings.ContainsRune(recoveryCodeSymbolAlphabet, r) {
			t.Errorf("GenerateRecoveryCode produced symbol %q not in the Crockford alphabet: %q", r, code)
		}
	}
}

func TestGenerateRecoveryCode_CallsProduceDistinctCodes(t *testing.T) {
	const n = 1000
	seen := make(map[string]bool, n)
	for range n {
		code, err := authdomain.GenerateRecoveryCode()
		if err != nil {
			t.Fatalf("GenerateRecoveryCode: %v", err)
		}
		if seen[code] {
			t.Fatalf("GenerateRecoveryCode produced a duplicate: %q (collision within %d calls would indicate far less than 80 bits of real entropy)", code, n)
		}
		seen[code] = true
	}
}

func TestNormalizeRecoveryCode_RoundTripsCaseAndDashVariants(t *testing.T) {
	code, err := authdomain.GenerateRecoveryCode()
	if err != nil {
		t.Fatalf("GenerateRecoveryCode: %v", err)
	}
	want := strings.ReplaceAll(code, "-", "")

	variants := []string{
		code,
		strings.ToLower(code),
		strings.ReplaceAll(code, "-", ""),
		strings.ReplaceAll(strings.ToLower(code), "-", " "),
		"  " + code + "  ",
	}
	for _, v := range variants {
		got := authdomain.NormalizeRecoveryCode(v)
		if got != want {
			t.Errorf("NormalizeRecoveryCode(%q) = %q, want %q", v, got, want)
		}
	}
}

func TestNormalizeRecoveryCode_FoldsVisuallyConfusableCharacters(t *testing.T) {
	tests := map[string]string{
		"o":    "0",
		"i":    "1",
		"l":    "1",
		"OIL0": "0110",
	}
	for input, want := range tests {
		if got := authdomain.NormalizeRecoveryCode(input); got != want {
			t.Errorf("NormalizeRecoveryCode(%q) = %q, want %q", input, got, want)
		}
	}
}
