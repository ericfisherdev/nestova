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
	gotConn Conn
	called  bool
	err     error
}

func (f *fakePinger) Ping(_ context.Context, conn Conn) error {
	f.called = true
	f.gotConn = conn
	return f.err
}

type fakeMigrator struct {
	gotConn Conn
	called  bool
	err     error
}

func (f *fakeMigrator) MigrateUp(_ context.Context, conn Conn) error {
	f.called = true
	f.gotConn = conn
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
	if pinger.gotConn.DSN != migrator.gotConn.DSN || migrator.gotConn.DSN != store.saved.DatabaseURL {
		t.Fatalf("DSN mismatch: ping=%q migrate=%q save=%q", pinger.gotConn.DSN, migrator.gotConn.DSN, store.saved.DatabaseURL)
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

func TestApply_Supabase_PersistsProviderAndPoolMode(t *testing.T) {
	t.Setenv("SESSION_SECRET", "x")
	t.Setenv("ENCRYPTION_KEY", "y")

	pinger := &fakePinger{}
	migrator := &fakeMigrator{}
	store := &fakeStore{}
	svc := newService(pinger, migrator, store)

	in := Input{
		Host: "db.supabase.co", Port: "6543", Database: "postgres", User: "postgres",
		Password: "pw", SSLMode: "require",
		Provider: "supabase", PoolMode: "transaction", SSLRootCert: "/etc/ssl/ca.crt",
	}
	if err := svc.Apply(context.Background(), in); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// The provider/pooler/TLS settings — including the SSL root cert — must reach
	// the ping and migrate steps so the wizard validates the path the server uses.
	wantConn := Conn{
		DSN:         "postgres://postgres:pw@db.supabase.co:6543/postgres?sslmode=require",
		Provider:    "supabase",
		PoolMode:    "transaction",
		SSLRootCert: "/etc/ssl/ca.crt",
	}
	if pinger.gotConn != wantConn {
		t.Fatalf("ping conn = %+v, want %+v", pinger.gotConn, wantConn)
	}
	if migrator.gotConn != wantConn {
		t.Fatalf("migrate conn = %+v, want %+v", migrator.gotConn, wantConn)
	}
	// ...and be persisted so the post-restart boot exports DB_PROVIDER et al.
	if store.saved.Provider != "supabase" || store.saved.PoolMode != "transaction" || store.saved.SSLRootCert != "/etc/ssl/ca.crt" {
		t.Fatalf("saved state = %+v, want supabase/transaction/cert", store.saved)
	}
}

func TestApply_Supabase_RejectsDisabledTLS(t *testing.T) {
	pinger := &fakePinger{}
	migrator := &fakeMigrator{}
	store := &fakeStore{}
	svc := newService(pinger, migrator, store)

	in := Input{Host: "db.supabase.co", Database: "postgres", User: "postgres", SSLMode: "disable", Provider: "supabase"}
	err := svc.Apply(context.Background(), in)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
	if pinger.called || migrator.called || store.called {
		t.Fatal("Supabase with sslmode=disable must be rejected before ping/migrate/persist")
	}
}

func TestApply_Postgres_NoProviderOverride(t *testing.T) {
	t.Setenv("SESSION_SECRET", "x")
	t.Setenv("ENCRYPTION_KEY", "y")

	store := &fakeStore{}
	svc := newService(&fakePinger{}, &fakeMigrator{}, store)

	// Explicit postgres provider must carry no override, so DB_PROVIDER defaults.
	in := Input{Host: "localhost", Database: "app", User: "svc", Provider: "postgres"}
	if err := svc.Apply(context.Background(), in); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if store.saved.Provider != "" || store.saved.PoolMode != "" || store.saved.SSLRootCert != "" {
		t.Fatalf("postgres path persisted a provider override: %+v", store.saved)
	}
}

func TestBuildConn(t *testing.T) {
	cases := []struct {
		name    string
		in      Input
		wantErr bool
		want    Conn
	}{
		{
			name: "postgres has no override",
			in:   Input{Host: "h", Database: "d", User: "u", Provider: "postgres"},
			want: Conn{DSN: "postgres://u:@h:5432/d?sslmode=disable"},
		},
		{
			name: "empty provider defaults to postgres",
			in:   Input{Host: "h", Database: "d", User: "u"},
			want: Conn{DSN: "postgres://u:@h:5432/d?sslmode=disable"},
		},
		{
			name: "supabase defaults to session pooler",
			in:   Input{Host: "h", Database: "d", User: "u", SSLMode: "require", Provider: "supabase"},
			want: Conn{DSN: "postgres://u:@h:5432/d?sslmode=require", Provider: "supabase", PoolMode: "session"},
		},
		{
			name: "supabase port 6543 infers transaction pooler",
			in:   Input{Host: "h", Port: "6543", Database: "d", User: "u", SSLMode: "require", Provider: "supabase"},
			want: Conn{DSN: "postgres://u:@h:6543/d?sslmode=require", Provider: "supabase", PoolMode: "transaction"},
		},
		{name: "supabase port 6543 rejects session mode", in: Input{Host: "h", Port: "6543", Database: "d", User: "u", SSLMode: "require", Provider: "supabase", PoolMode: "session"}, wantErr: true},
		{name: "supabase port 5432 rejects transaction mode", in: Input{Host: "h", Database: "d", User: "u", SSLMode: "require", Provider: "supabase", PoolMode: "transaction"}, wantErr: true},
		{name: "supabase rejects sslmode disable", in: Input{Host: "h", Database: "d", User: "u", SSLMode: "disable", Provider: "supabase"}, wantErr: true},
		{name: "supabase rejects non-enforcing sslmode prefer", in: Input{Host: "h", Database: "d", User: "u", SSLMode: "prefer", Provider: "supabase"}, wantErr: true},
		{
			name: "supabase raw dsn infers transaction from 6543",
			in:   Input{RawDSN: "postgres://u:p@db.supabase.co:6543/postgres?sslmode=require", Provider: "supabase"},
			want: Conn{DSN: "postgres://u:p@db.supabase.co:6543/postgres?sslmode=require", Provider: "supabase", PoolMode: "transaction"},
		},
		{name: "supabase raw dsn rejects sslmode disable", in: Input{RawDSN: "postgres://u:p@db.supabase.co:5432/postgres?sslmode=disable", Provider: "supabase"}, wantErr: true},
		{name: "supabase raw dsn rejects absent sslmode", in: Input{RawDSN: "postgres://u:p@db.supabase.co:5432/postgres", Provider: "supabase"}, wantErr: true},
		// The wizard accepts only URL-form DSNs (validatePostgresDSN requires the
		// postgres:// scheme), so a keyword/value DSN is rejected before the
		// provider checks regardless of its sslmode.
		{name: "supabase keyword/value raw dsn rejected (url-only)", in: Input{RawDSN: "host=db.supabase.co port=6543 sslmode=require", Provider: "supabase"}, wantErr: true},
		{name: "supabase rejects unknown pool mode", in: Input{Host: "h", Database: "d", User: "u", SSLMode: "require", Provider: "supabase", PoolMode: "bogus"}, wantErr: true},
		{name: "unsupported provider", in: Input{Host: "h", Database: "d", User: "u", Provider: "mysql"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildConn(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildConn: %v", err)
			}
			if got != tc.want {
				t.Fatalf("buildConn = %+v, want %+v", got, tc.want)
			}
		})
	}
}
