package config

import (
	"strings"
	"testing"
)

// TestValidate_PublicBaseURLRejectsBareTrailingSlash is a white-box test
// (package config, not config_test) of the unexported validate() method
// directly: it exercises the NES-136 tightening that closed the "a bare
// trailing slash origin passes validation" gap CodeRabbit flagged. This
// path is UNREACHABLE through the only production caller, Load() (which
// already strips a trailing slash via strings.TrimRight before validate()
// ever runs — see TestLoadValid's own "PUBLIC_BASE_URL is trimmed of a
// trailing slash" cases in config_test.go), so this test constructs a
// Config directly and calls validate() itself to confirm the tightened
// check actually rejects the value it is meant to reject, as
// defense-in-depth against a future caller that does not pre-trim.
func TestValidate_PublicBaseURLRejectsBareTrailingSlash(t *testing.T) {
	cfg := Config{
		Server: ServerConfig{PublicBaseURL: "https://example.org/"},
	}
	errs := cfg.validate()

	found := false
	for _, err := range errs {
		if strings.Contains(err.Error(), "PUBLIC_BASE_URL") && strings.Contains(err.Error(), "origin only") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("validate() with a bare trailing-slash PublicBaseURL did not report an origin-only error; got: %v", errs)
	}
}

// TestValidate_PublicBaseURLWithoutTrailingSlashPasses confirms the
// tightened check does not reject a genuinely clean origin — the fix must
// only close the trailing-slash gap, not become overly strict.
func TestValidate_PublicBaseURLWithoutTrailingSlashPasses(t *testing.T) {
	cfg := Config{
		Server: ServerConfig{PublicBaseURL: "https://example.org"},
	}
	errs := cfg.validate()

	for _, err := range errs {
		if strings.Contains(err.Error(), "PUBLIC_BASE_URL") {
			t.Errorf("validate() with a clean origin PublicBaseURL reported a PUBLIC_BASE_URL error: %v", err)
		}
	}
}
