package setup_test

import (
	"errors"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/setup"
)

// fakeGenerator counts calls and returns a fixed token or error.
type fakeGenerator struct {
	token string
	err   error
	calls int
}

func (g *fakeGenerator) generate() (string, error) {
	g.calls++
	return g.token, g.err
}

func lookupOf(value string) func(string) string {
	return func(key string) string {
		if key != setup.SetupTokenEnv {
			return ""
		}
		return value
	}
}

func TestResolveToken_EnvValueWinsTrimmed(t *testing.T) {
	gen := &fakeGenerator{token: "generated"}
	token, generated, err := setup.ResolveToken(lookupOf("  pinned-token \n"), gen.generate)
	if err != nil {
		t.Fatalf("ResolveToken: %v", err)
	}
	if token != "pinned-token" || generated {
		t.Fatalf("got (%q, %v), want (pinned-token, false)", token, generated)
	}
	if gen.calls != 0 {
		t.Fatalf("generator called %d times, want 0", gen.calls)
	}
}

func TestResolveToken_BlankEnvGenerates(t *testing.T) {
	for _, env := range []string{"", "   ", "\t\n"} {
		gen := &fakeGenerator{token: "generated"}
		token, generated, err := setup.ResolveToken(lookupOf(env), gen.generate)
		if err != nil {
			t.Fatalf("env %q: ResolveToken: %v", env, err)
		}
		if token != "generated" || !generated {
			t.Fatalf("env %q: got (%q, %v), want (generated, true)", env, token, generated)
		}
		if gen.calls != 1 {
			t.Fatalf("env %q: generator called %d times, want 1", env, gen.calls)
		}
	}
}

func TestResolveToken_GeneratorErrorPropagated(t *testing.T) {
	boom := errors.New("entropy exhausted")
	gen := &fakeGenerator{err: boom}
	token, generated, err := setup.ResolveToken(lookupOf(""), gen.generate)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if token != "" || generated {
		t.Fatalf("got (%q, %v), want empty token and generated=false on error", token, generated)
	}
}
