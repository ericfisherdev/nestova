package setup

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/bootstrap"
)

type fakePinger struct {
	gotDSN string
	err    error
}

func (f *fakePinger) Ping(_ context.Context, dsn string) error {
	f.gotDSN = dsn
	return f.err
}

type fakeMigrator struct {
	gotDSN string
	called bool
	err    error
}

func (f *fakeMigrator) MigrateUp(_ context.Context, dsn string) error {
	f.called = true
	f.gotDSN = dsn
	return f.err
}

type fakeStore struct {
	saved  *bootstrap.State
	called bool
	err    error
}

func (f *fakeStore) Save(state *bootstrap.State) error {
	f.called = true
	f.saved = state
	return f.err
}

// newService wires a Service over the given fakes with a deterministic secret
// generator so assertions on generated secrets are stable.
func newService(p Pinger, m Migrator, s StateStore) *Service {
	svc := NewService(p, m, s)
	svc.genSecret = func() (string, error) { return "generated-secret", nil }
	return svc
}

func TestApply_Success_BuildsDSNMigratesAndPersists(t *testing.T) {
	// No operator-provided secrets, so both should be generated and persisted.
	t.Setenv("SESSION_SECRET", "")
	t.Setenv("ENCRYPTION_KEY", "")

	pinger := &fakePinger{}
	migrator := &fakeMigrator{}
	store := &fakeStore{}
	svc := newService(pinger, migrator, store)

	in := Input{Host: "localhost", Port: "5434", Database: "nestova_test", User: "nestova", Password: "p@ss word", SSLMode: "disable"}
	if err := svc.Apply(context.Background(), in); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !migrator.called {
		t.Fatal("migrations were not run")
	}
	if !store.called || store.saved == nil {
		t.Fatal("state was not persisted")
	}
	// The same DSN must be pinged, migrated, and saved.
	if pinger.gotDSN != migrator.gotDSN || migrator.gotDSN != store.saved.DatabaseURL {
		t.Fatalf("DSN mismatch: ping=%q migrate=%q save=%q", pinger.gotDSN, migrator.gotDSN, store.saved.DatabaseURL)
	}
	// The assembled DSN must round-trip the fields, including a space-bearing password.
	u, err := url.Parse(store.saved.DatabaseURL)
	if err != nil {
		t.Fatalf("saved DSN is not a URL: %v", err)
	}
	if u.Hostname() != "localhost" || u.Port() != "5434" {
		t.Fatalf("host/port = %s:%s, want localhost:5434", u.Hostname(), u.Port())
	}
	if pw, _ := u.User.Password(); pw != "p@ss word" {
		t.Fatalf("password did not round-trip: %q", pw)
	}
	if store.saved.SessionSecret != "generated-secret" || store.saved.EncryptionKey != "generated-secret" {
		t.Fatalf("secrets not generated: %+v", store.saved)
	}
}

func TestApply_RespectsOperatorProvidedSecrets(t *testing.T) {
	t.Setenv("SESSION_SECRET", "operator-session")
	t.Setenv("ENCRYPTION_KEY", "operator-key")

	store := &fakeStore{}
	svc := newService(&fakePinger{}, &fakeMigrator{}, store)
	if err := svc.Apply(context.Background(), Input{RawDSN: "postgres://u@h:5432/db"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// When the operator set the env vars, no secret is persisted (env wins at load).
	if store.saved.SessionSecret != "" || store.saved.EncryptionKey != "" {
		t.Fatalf("secrets should not be generated when env is set: %+v", store.saved)
	}
}

func TestApply_PingFailure_DoesNotMigrateOrPersist(t *testing.T) {
	migrator := &fakeMigrator{}
	store := &fakeStore{}
	svc := newService(&fakePinger{err: errors.New("connection refused")}, migrator, store)

	err := svc.Apply(context.Background(), Input{Host: "h", Database: "d", User: "u"})
	if !errors.Is(err, ErrConnect) {
		t.Fatalf("error = %v, want ErrConnect", err)
	}
	if migrator.called {
		t.Fatal("migrations ran despite a failed ping")
	}
	if store.called {
		t.Fatal("state persisted despite a failed ping")
	}
}

func TestApply_MigrateFailure_DoesNotPersist(t *testing.T) {
	store := &fakeStore{}
	svc := newService(&fakePinger{}, &fakeMigrator{err: errors.New("goose boom")}, store)

	err := svc.Apply(context.Background(), Input{Host: "h", Database: "d", User: "u"})
	if !errors.Is(err, ErrMigrate) {
		t.Fatalf("error = %v, want ErrMigrate", err)
	}
	if store.called {
		t.Fatal("state persisted despite a failed migration")
	}
}

func TestApply_InvalidInput(t *testing.T) {
	svc := newService(&fakePinger{}, &fakeMigrator{}, &fakeStore{})
	err := svc.Apply(context.Background(), Input{Host: "", Database: "", User: ""})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
}

func TestBuildDSN(t *testing.T) {
	cases := []struct {
		name    string
		in      Input
		wantErr bool
		check   func(t *testing.T, dsn string)
	}{
		{
			name: "fields with defaults",
			in:   Input{Host: "db.local", Database: "app", User: "svc"},
			check: func(t *testing.T, dsn string) {
				if !strings.Contains(dsn, "db.local:5432") {
					t.Fatalf("default port missing: %q", dsn)
				}
				if !strings.Contains(dsn, "sslmode=disable") {
					t.Fatalf("default sslmode missing: %q", dsn)
				}
			},
		},
		{
			name: "raw dsn passthrough",
			in:   Input{RawDSN: "postgresql://u:p@h:6543/db?sslmode=require"},
			check: func(t *testing.T, dsn string) {
				if dsn != "postgresql://u:p@h:6543/db?sslmode=require" {
					t.Fatalf("raw DSN altered: %q", dsn)
				}
			},
		},
		{name: "missing required fields", in: Input{Host: "h"}, wantErr: true},
		{name: "non-numeric port", in: Input{Host: "h", Database: "d", User: "u", Port: "abc"}, wantErr: true},
		{name: "out-of-range port", in: Input{Host: "h", Database: "d", User: "u", Port: "70000"}, wantErr: true},
		{name: "bad sslmode", in: Input{Host: "h", Database: "d", User: "u", SSLMode: "bogus"}, wantErr: true},
		{name: "raw dsn wrong scheme", in: Input{RawDSN: "mysql://h/db"}, wantErr: true},
		{name: "raw dsn missing database", in: Input{RawDSN: "postgres://u@h:5432"}, wantErr: true},
		{name: "raw dsn database via dbname param", in: Input{RawDSN: "postgres://u@h:5432?dbname=app"}, check: func(t *testing.T, dsn string) {
			if dsn != "postgres://u@h:5432?dbname=app" {
				t.Fatalf("raw DSN altered: %q", dsn)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn, err := buildDSN(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got dsn %q", dsn)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildDSN: %v", err)
			}
			if tc.check != nil {
				tc.check(t, dsn)
			}
		})
	}
}
