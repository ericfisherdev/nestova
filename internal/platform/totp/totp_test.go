package totp_test

import (
	"testing"
	"time"

	gotp "github.com/pquerna/otp"
	pquernatotp "github.com/pquerna/otp/totp"

	"github.com/ericfisherdev/nestova/internal/platform/totp"
)

func TestGenerateSecret_ReturnsUsableSecretAndOTPAuthURL(t *testing.T) {
	p := totp.NewProvider()
	secret, otpauthURL, err := p.GenerateSecret("Nestova", "alice")
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if secret == "" {
		t.Fatal("GenerateSecret returned an empty secret")
	}

	key, err := gotp.NewKeyFromURL(otpauthURL)
	if err != nil {
		t.Fatalf("parse otpauth URL %q: %v", otpauthURL, err)
	}
	if key.Type() != "totp" {
		t.Errorf("otpauth URL type = %q, want totp", key.Type())
	}
	if key.Issuer() != "Nestova" {
		t.Errorf("otpauth URL issuer = %q, want Nestova", key.Issuer())
	}
	if key.Secret() != secret {
		t.Errorf("otpauth URL secret parameter = %q, want the generated secret %q", key.Secret(), secret)
	}
}

func TestGenerateSecret_EachCallProducesADifferentSecret(t *testing.T) {
	p := totp.NewProvider()
	secret1, _, err := p.GenerateSecret("Nestova", "alice")
	if err != nil {
		t.Fatalf("GenerateSecret (1): %v", err)
	}
	secret2, _, err := p.GenerateSecret("Nestova", "alice")
	if err != nil {
		t.Fatalf("GenerateSecret (2): %v", err)
	}
	if secret1 == secret2 {
		t.Error("two GenerateSecret calls returned the same secret; each enrollment must get a fresh random secret")
	}
}

func TestValidate_AcceptsCurrentCodeRejectsWrongCode(t *testing.T) {
	p := totp.NewProvider()
	secret, _, err := p.GenerateSecret("Nestova", "alice")
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}

	code, err := pquernatotp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("pquerna/otp GenerateCode: %v", err)
	}
	if !p.Validate(code, secret) {
		t.Error("Validate rejected a currently-valid code")
	}

	wrongCode := mutateFirstDigit(code)
	if p.Validate(wrongCode, secret) {
		t.Errorf("Validate accepted mutated code %q (real code was %q)", wrongCode, code)
	}
}

// mutateFirstDigit returns code with its first character replaced by a
// decimal digit guaranteed to differ from the original, so the result is
// always a genuinely wrong code — unlike comparing against a fixed literal
// like "000000", which could coincidentally equal the real code.
func mutateFirstDigit(code string) string {
	replacement := byte('0')
	if code[0] == '0' {
		replacement = '1'
	}
	return string(replacement) + code[1:]
}

func TestMatchStep_AcceptsCurrentCodeAndReportsStep(t *testing.T) {
	p := totp.NewProvider()
	secret, _, err := p.GenerateSecret("Nestova", "alice")
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}

	now := time.Now()
	code, err := pquernatotp.GenerateCode(secret, now)
	if err != nil {
		t.Fatalf("pquerna/otp GenerateCode: %v", err)
	}

	step, ok := p.MatchStep(code, secret)
	if !ok {
		t.Fatal("MatchStep rejected a currently-valid code")
	}
	wantStep := now.UTC().Unix() / 30
	if step != wantStep {
		t.Errorf("MatchStep step = %d, want %d", step, wantStep)
	}
}

func TestMatchStep_RejectsWrongCode(t *testing.T) {
	p := totp.NewProvider()
	secret, _, err := p.GenerateSecret("Nestova", "alice")
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	code, err := pquernatotp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}

	if _, ok := p.MatchStep(mutateFirstDigit(code), secret); ok {
		t.Error("MatchStep accepted a mutated (wrong) code")
	}
}

// TestMatchStep_AcceptsAdjacentSkewSteps mirrors Validate's own ±1-period
// tolerance: a code generated for the PREVIOUS or NEXT 30-second step must
// still match (accounting for clock drift between the server and the
// member's authenticator app), and MatchStep must report that step's OWN
// number, not the current step.
func TestMatchStep_AcceptsAdjacentSkewSteps(t *testing.T) {
	p := totp.NewProvider()
	secret, _, err := p.GenerateSecret("Nestova", "alice")
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}

	now := time.Now()
	currentStep := now.UTC().Unix() / 30

	for _, delta := range []int64{-1, 1} {
		stepTime := time.Unix((currentStep+delta)*30, 0).UTC()
		code, err := pquernatotp.GenerateCode(secret, stepTime)
		if err != nil {
			t.Fatalf("GenerateCode at delta %d: %v", delta, err)
		}
		step, ok := p.MatchStep(code, secret)
		if !ok {
			t.Fatalf("MatchStep rejected a code from the adjacent step (delta=%d)", delta)
		}
		if step != currentStep+delta {
			t.Errorf("MatchStep step (delta=%d) = %d, want %d", delta, step, currentStep+delta)
		}
	}
}

// TestMatchStep_RejectsBeyondSkewWindow mirrors Validate's own rejection of
// a code two or more periods away from now.
func TestMatchStep_RejectsBeyondSkewWindow(t *testing.T) {
	p := totp.NewProvider()
	secret, _, err := p.GenerateSecret("Nestova", "alice")
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}

	farTime := time.Now().Add(-5 * time.Minute)
	code, err := pquernatotp.GenerateCode(secret, farTime)
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	if _, ok := p.MatchStep(code, secret); ok {
		t.Error("MatchStep accepted a code from 5 minutes ago, outside the skew window")
	}
}

func TestValidate_RejectsCodeForADifferentSecret(t *testing.T) {
	p := totp.NewProvider()
	secretA, _, err := p.GenerateSecret("Nestova", "alice")
	if err != nil {
		t.Fatalf("GenerateSecret (A): %v", err)
	}
	secretB, _, err := p.GenerateSecret("Nestova", "bob")
	if err != nil {
		t.Fatalf("GenerateSecret (B): %v", err)
	}

	code, err := pquernatotp.GenerateCode(secretA, time.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	if p.Validate(code, secretB) {
		t.Error("Validate accepted secret A's code against secret B")
	}
}
