// Package setup implements the first-run setup wizard: the HTTP form that
// collects database connection details before any database exists, validates
// connectivity, runs the embedded migrations, and persists the resulting
// configuration plus generated secrets.
//
// It is reached only while the app is unconfigured (see
// internal/platform/bootstrap). Once setup completes, the app boots normally and
// these routes are never mounted. Outbound dependencies are expressed as small
// ports (Pinger, Migrator, StateStore) and injected via the constructor, so the
// service stays testable without a real database and the package never imports a
// bounded context.
package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/ericfisherdev/nestova/internal/platform/bootstrap"
)

// Pinger verifies that a database is reachable at the given DSN. It abstracts
// db.New so the service is testable without a real database.
type Pinger interface {
	Ping(ctx context.Context, conn Conn) error
}

// Migrator applies all pending schema migrations against the given connection.
// It abstracts migrate.Up.
type Migrator interface {
	MigrateUp(ctx context.Context, conn Conn) error
}

// Conn is the resolved connection descriptor the setup service validates and
// persists: the DSN plus the optional provider/pooler/TLS settings the server
// applies at boot. Provider is empty for self-hosted Postgres.
type Conn struct {
	DSN         string
	Provider    string // "" (postgres) or "supabase"
	PoolMode    string // "session" | "transaction" (supabase only)
	SSLRootCert string // optional CA-bundle path (supabase verify-full)
}

// StateStore persists the first-run configuration. It abstracts
// bootstrap.SaveState bound to a path.
type StateStore interface {
	Save(state *bootstrap.State) error
}

// Stage classifies which step of Apply failed so the handler can show a targeted
// message without leaking internals. Errors from Apply wrap one of the sentinels
// below; callers match with errors.Is.
var (
	// ErrInvalidInput means the submitted fields could not form a valid DSN.
	ErrInvalidInput = errors.New("invalid database connection details")
	// ErrConnect means the database was unreachable with the supplied DSN.
	ErrConnect = errors.New("could not connect to the database")
	// ErrMigrate means connectivity succeeded but migrations failed to apply.
	ErrMigrate = errors.New("could not initialize the database schema")
)

// allowedSSLModes is the set of libpq sslmode values the wizard accepts. The
// list mirrors what pgx understands; an unknown value is rejected up front
// rather than surfacing as an opaque connection error.
var allowedSSLModes = map[string]struct{}{
	"disable":     {},
	"allow":       {},
	"prefer":      {},
	"require":     {},
	"verify-ca":   {},
	"verify-full": {},
}

// Input is the raw setup form. Either RawDSN is provided directly (advanced) or
// the DSN is assembled from the discrete fields.
type Input struct {
	Host     string
	Port     string
	Database string
	User     string
	Password string
	SSLMode  string
	// RawDSN, when non-empty, is used verbatim and the discrete fields are ignored.
	RawDSN string
	// Provider selects the backend: "" / "postgres" (self-hosted) or "supabase".
	Provider string
	// PoolMode is the Supabase pooler mode ("session" | "transaction"); used only
	// when Provider is supabase.
	PoolMode string
	// SSLRootCert is an optional CA-bundle path (Supabase verify-full).
	SSLRootCert string
}

// secretGenerator produces a random secret. It defaults to
// bootstrap.GenerateSecret and is injectable for deterministic tests.
type secretGenerator func() (string, error)

// Service performs the setup action: build a DSN, validate connectivity, run
// migrations, and persist the configuration.
type Service struct {
	pinger    Pinger
	migrator  Migrator
	store     StateStore
	genSecret secretGenerator
}

// NewService constructs a Service. All dependencies are required; a missing one
// panics at construction (fail-fast), not at request time.
func NewService(pinger Pinger, migrator Migrator, store StateStore) *Service {
	if pinger == nil {
		panic("setup: NewService requires a non-nil Pinger")
	}
	if migrator == nil {
		panic("setup: NewService requires a non-nil Migrator")
	}
	if store == nil {
		panic("setup: NewService requires a non-nil StateStore")
	}
	return &Service{
		pinger:    pinger,
		migrator:  migrator,
		store:     store,
		genSecret: bootstrap.GenerateSecret,
	}
}

// Apply runs the full first-run sequence: assemble + validate a DSN, verify
// connectivity, apply migrations, generate any missing secrets, and persist the
// state. Failures wrap ErrInvalidInput, ErrConnect, or ErrMigrate so the handler
// can report the failing stage.
func (s *Service) Apply(ctx context.Context, in Input) error {
	conn, err := buildConn(in)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.pinger.Ping(ctx, conn); err != nil {
		return fmt.Errorf("%w: %v", ErrConnect, err)
	}
	if err := s.migrator.MigrateUp(ctx, conn); err != nil {
		return fmt.Errorf("%w: %v", ErrMigrate, err)
	}

	// Generate the secrets the running app needs but the operator has not
	// supplied via the environment. The environment still wins at load time
	// (bootstrap.ExportToEnv), so an operator-provided secret is respected.
	state := &bootstrap.State{
		DatabaseURL: conn.DSN,
		Provider:    conn.Provider,
		PoolMode:    conn.PoolMode,
		SSLRootCert: conn.SSLRootCert,
	}
	if os.Getenv("SESSION_SECRET") == "" {
		if state.SessionSecret, err = s.genSecret(); err != nil {
			return fmt.Errorf("setup: generate session secret: %w", err)
		}
	}
	if os.Getenv("ENCRYPTION_KEY") == "" {
		if state.EncryptionKey, err = s.genSecret(); err != nil {
			return fmt.Errorf("setup: generate encryption key: %w", err)
		}
	}
	if err := s.store.Save(state); err != nil {
		return fmt.Errorf("setup: persist configuration: %w", err)
	}
	return nil
}

// Database providers the wizard can configure.
const (
	providerPostgres = "postgres"
	providerSupabase = "supabase"
)

// allowedPoolModes is the set of Supabase pooler modes the wizard accepts.
var allowedPoolModes = map[string]struct{}{
	"session":     {},
	"transaction": {},
}

// supabaseTLSModes are the sslmode values that actually enforce TLS. disable
// turns it off; allow and prefer can silently downgrade to plaintext, so they
// are excluded for the Supabase provider.
var supabaseTLSModes = map[string]bool{
	"require":     true,
	"verify-ca":   true,
	"verify-full": true,
}

// buildConn resolves the form into a Conn: it builds and validates the DSN, then
// applies provider-specific rules. Self-hosted Postgres carries no provider
// override; Supabase requires an enforced-TLS sslmode, a pooler mode consistent
// with the DSN port (inferred when unset), and an optional SSL root cert.
func buildConn(in Input) (Conn, error) {
	dsn, err := buildDSN(in)
	if err != nil {
		return Conn{}, err
	}

	switch provider := strings.ToLower(strings.TrimSpace(in.Provider)); provider {
	case "", providerPostgres:
		// Self-hosted Postgres: no provider override, no pooler — leave the
		// post-restart boot to default DB_PROVIDER to postgres.
		return Conn{DSN: dsn}, nil
	case providerSupabase:
		// Supabase must enforce TLS. sslmode=disable turns it off, and allow/prefer
		// can silently downgrade to plaintext, so require an enforcing mode.
		if !supabaseTLSModes[dsnSSLMode(dsn)] {
			return Conn{}, errors.New("the Supabase provider requires an enforced-TLS sslmode (require, verify-ca, or verify-full)")
		}
		poolMode := strings.ToLower(strings.TrimSpace(in.PoolMode))
		// Supabase's transaction pooler listens on 6543; the session pooler and
		// direct connection on 5432. Infer the mode from the port when the operator
		// left it unset, then require the mode and the port to agree: the
		// transaction pooler needs transaction mode (else pgx holds prepared
		// statements it cannot keep across multiplexed transactions), and only the
		// 6543 endpoint provides it.
		transactionPort := dsnPort(dsn) == "6543"
		if poolMode == "" {
			if transactionPort {
				poolMode = "transaction"
			} else {
				poolMode = "session"
			}
		}
		if _, ok := allowedPoolModes[poolMode]; !ok {
			return Conn{}, fmt.Errorf("unsupported pool mode %q", poolMode)
		}
		if transactionPort && poolMode == "session" {
			return Conn{}, errors.New("port 6543 is the Supabase transaction pooler — select the transaction pool mode, or use the session pooler on port 5432")
		}
		if !transactionPort && poolMode == "transaction" {
			return Conn{}, errors.New("the transaction pool mode requires the Supabase transaction pooler on port 6543")
		}
		return Conn{
			DSN:         dsn,
			Provider:    providerSupabase,
			PoolMode:    poolMode,
			SSLRootCert: strings.TrimSpace(in.SSLRootCert),
		}, nil
	default:
		return Conn{}, fmt.Errorf("unsupported provider %q", provider)
	}
}

// dsnSSLMode returns the lowercased sslmode query parameter of dsn, or "" when
// absent or unparsable. For field-built DSNs buildDSN always sets it; a raw DSN
// may omit it (libpq then defaults to prefer, which is TLS-capable).
func dsnSSLMode(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Query().Get("sslmode"))
}

// dsnPort returns the port of a URL-form dsn, or "" when absent or unparsable
// (e.g. a keyword/value DSN), in which case the pooler mode cannot be inferred.
func dsnPort(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	return u.Port()
}

// buildDSN returns the Postgres DSN from in: the raw DSN when supplied,
// otherwise a postgres:// URL assembled from the discrete fields. It validates
// required fields and that the result is a well-formed postgres:// URL, so a
// malformed value fails here rather than at Ping.
func buildDSN(in Input) (string, error) {
	if raw := strings.TrimSpace(in.RawDSN); raw != "" {
		if err := validatePostgresDSN(raw); err != nil {
			return "", err
		}
		return raw, nil
	}

	host := strings.TrimSpace(in.Host)
	dbName := strings.TrimSpace(in.Database)
	user := strings.TrimSpace(in.User)
	if host == "" || dbName == "" || user == "" {
		return "", errors.New("host, database, and user are required")
	}

	port := strings.TrimSpace(in.Port)
	if port == "" {
		port = "5432"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("port must be a number between 1 and 65535, got %q", port)
	}

	sslMode := strings.TrimSpace(in.SSLMode)
	if sslMode == "" {
		sslMode = "disable"
	}
	if _, ok := allowedSSLModes[sslMode]; !ok {
		return "", fmt.Errorf("unsupported sslmode %q", sslMode)
	}

	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, in.Password),
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + dbName,
	}
	q := url.Values{}
	q.Set("sslmode", sslMode)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validatePostgresDSN checks that a raw DSN is an absolute postgres:// URL with a
// host, rejecting obvious mistakes before a connection is attempted.
func validatePostgresDSN(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("scheme must be postgres://, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	// Require an explicit target database (path segment or dbname query param) so a
	// DSN like "postgres://u@h:5432" cannot silently run migrations against the
	// connection's default database.
	if strings.Trim(u.Path, "/") == "" && strings.TrimSpace(u.Query().Get("dbname")) == "" {
		return errors.New("missing database name")
	}
	return nil
}
