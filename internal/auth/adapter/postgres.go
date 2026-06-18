// Package adapter contains the auth context's outbound adapters: the Postgres
// CredentialRepository and the HTTP inbound adapters (session manager, auth
// middleware, and login/logout handlers).
package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

const (
	// uniqueViolation is the PostgreSQL SQLSTATE for a unique-constraint violation.
	uniqueViolation = "23505"
	// memberEmailUnique is the unique constraint on member.email (named in the
	// 00002_auth migration).
	memberEmailUnique = "member_email_unique"
)

// CredentialRepository is the pgx-backed implementation of
// authdomain.CredentialRepository. UUIDs are passed and scanned as text to
// match the household adapter convention (no pgx UUID codec registration).
type CredentialRepository struct {
	pool *pgxpool.Pool
}

// Compile-time assurance the adapter satisfies the port.
var _ authdomain.CredentialRepository = (*CredentialRepository)(nil)

// NewCredentialRepository constructs the repository with an injected pgx pool.
func NewCredentialRepository(pool *pgxpool.Pool) *CredentialRepository {
	if pool == nil {
		panic("adapter: NewCredentialRepository requires a non-nil pool")
	}
	return &CredentialRepository{pool: pool}
}

// FindByEmail returns the Credential for the given email address, or
// authdomain.ErrInvalidCredentials when no member with that email and a
// non-null password_hash exists (preventing user enumeration).
func (r *CredentialRepository) FindByEmail(ctx context.Context, email string) (*authdomain.Credential, error) {
	const q = `
		SELECT id, password_hash
		  FROM member
		 WHERE email = $1
		   AND password_hash IS NOT NULL`

	var (
		idStr        string
		passwordHash string
	)
	err := r.pool.QueryRow(ctx, q, email).Scan(&idStr, &passwordHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, authdomain.ErrInvalidCredentials
		}
		return nil, fmt.Errorf("find by email: %w", err)
	}

	memberID, err := household.ParseMemberID(idStr)
	if err != nil {
		return nil, fmt.Errorf("find by email: parse member id: %w", err)
	}

	return &authdomain.Credential{
		MemberID:     memberID,
		PasswordHash: passwordHash,
	}, nil
}

// SetPassword stores (or replaces) the email and password hash on the member
// row identified by memberID. It returns household.ErrMemberNotFound when the
// member does not exist, and authdomain.ErrEmailAlreadyInUse when the email is
// already assigned to another member.
func (r *CredentialRepository) SetPassword(ctx context.Context, memberID household.MemberID, email, passwordHash string) error {
	const q = `
		UPDATE member
		   SET email         = $2,
		       password_hash = $3,
		       updated_at    = now()
		 WHERE id = $1`

	tag, err := r.pool.Exec(ctx, q, memberID.String(), email, passwordHash)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == memberEmailUnique {
			return authdomain.ErrEmailAlreadyInUse
		}
		return fmt.Errorf("set password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return household.ErrMemberNotFound
	}
	return nil
}
