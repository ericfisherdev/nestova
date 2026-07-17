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
