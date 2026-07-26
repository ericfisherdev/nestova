package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// federationAuthorizationCodeMemberFK is the auto-named FK
// federation_authorization_code.member_id -> member(id) (00037); a violation
// means memberID does not exist.
const federationAuthorizationCodeMemberFK = "federation_authorization_code_member_id_fkey"

// AuthorizationCodeRepository is the pgx-backed
// authdomain.AuthorizationCodeRepository.
type AuthorizationCodeRepository struct {
	dbtx db.TX
}

var _ authdomain.AuthorizationCodeRepository = (*AuthorizationCodeRepository)(nil)

// NewAuthorizationCodeRepository constructs the repository with an injected
// query executor (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewAuthorizationCodeRepository(dbtx db.TX) *AuthorizationCodeRepository {
	if dbtx == nil {
		panic("adapter: NewAuthorizationCodeRepository requires a non-nil db.TX")
	}
	return &AuthorizationCodeRepository{dbtx: dbtx}
}

// Create inserts an authorization code and populates its created_at, mapping
// an unknown member to household.ErrMemberNotFound.
func (r *AuthorizationCodeRepository) Create(ctx context.Context, code *authdomain.AuthorizationCode) error {
	if code == nil {
		return errors.New("auth/adapter: create authorization code: nil code")
	}
	const q = `
		INSERT INTO federation_authorization_code (id, member_id, client_id, redirect_uri, code_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at`
	err := r.dbtx.QueryRow(ctx, q,
		code.ID.String(), code.MemberID.String(), code.ClientID, code.RedirectURI, code.CodeHash, code.ExpiresAt,
	).Scan(&code.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == federationAuthorizationCodeMemberFK {
			return household.ErrMemberNotFound
		}
		return fmt.Errorf("create authorization code: %w", err)
	}
	return nil
}

// Consume atomically validates codeHash against now, marks the code used,
// and returns the resolved row — see authdomain.AuthorizationCodeRepository's
// doc for the full contract. SELECT ... FOR UPDATE locks the row for the
// remainder of this transaction, so a second, concurrent Consume of the same
// code blocks here until the first commits (and then correctly observes
// used_at set) or rolls back — closing the check-then-mark-used race a plain
// SELECT would leave open, mirroring kiosk's ActivationCodeRepository.Redeem.
func (r *AuthorizationCodeRepository) Consume(ctx context.Context, codeHash string, now time.Time) (*authdomain.AuthorizationCode, error) {
	beginner, ok := r.dbtx.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return nil, errors.New("consume authorization code: executor does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("consume authorization code: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const lookup = `
		SELECT id, member_id, client_id, redirect_uri, created_at, expires_at, used_at
		  FROM federation_authorization_code
		 WHERE code_hash = $1
		   FOR UPDATE`
	var (
		idStr, memberIDStr, clientID, redirectURI string
		createdAt, expiresAt                      time.Time
		usedAt                                    *time.Time
	)
	err = tx.QueryRow(ctx, lookup, codeHash).Scan(&idStr, &memberIDStr, &clientID, &redirectURI, &createdAt, &expiresAt, &usedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, authdomain.ErrAuthorizationCodeNotFound
		}
		return nil, fmt.Errorf("consume authorization code: lookup: %w", err)
	}
	if usedAt != nil {
		return nil, authdomain.ErrAuthorizationCodeUsed
	}
	if !now.Before(expiresAt) {
		return nil, authdomain.ErrAuthorizationCodeExpired
	}

	if _, err := tx.Exec(ctx, `UPDATE federation_authorization_code SET used_at = $2 WHERE id = $1`, idStr, now); err != nil {
		return nil, fmt.Errorf("consume authorization code: mark used: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("consume authorization code: commit: %w", err)
	}

	id, err := authdomain.ParseAuthorizationCodeID(idStr)
	if err != nil {
		return nil, fmt.Errorf("consume authorization code: parse id: %w", err)
	}
	memberID, err := household.ParseMemberID(memberIDStr)
	if err != nil {
		return nil, fmt.Errorf("consume authorization code: parse member id: %w", err)
	}

	return &authdomain.AuthorizationCode{
		ID:          id,
		MemberID:    memberID,
		ClientID:    clientID,
		RedirectURI: redirectURI,
		CodeHash:    codeHash,
		CreatedAt:   createdAt,
		ExpiresAt:   expiresAt,
		UsedAt:      &now,
	}, nil
}
