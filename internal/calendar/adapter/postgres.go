package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ericfisherdev/nestova/internal/calendar/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// CalendarAccountRepository is the pgx-backed domain.CalendarAccountRepository.
// UUIDs are passed and scanned as text; the encrypted tokens are bytea and the
// selected calendar ids are a text[] handled by pgx directly.
type CalendarAccountRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.CalendarAccountRepository = (*CalendarAccountRepository)(nil)

// NewCalendarAccountRepository constructs the repository with an injected query
// executor (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewCalendarAccountRepository(dbtx db.TX) *CalendarAccountRepository {
	if dbtx == nil {
		panic("adapter: NewCalendarAccountRepository requires a non-nil db.TX")
	}
	return &CalendarAccountRepository{dbtx: dbtx}
}

// Create inserts a calendar account and populates its timestamps. It maps tenant
// FK violations to household.ErrHouseholdNotFound (unknown household) and
// household.ErrMemberNotFound (unknown member in the household).
func (r *CalendarAccountRepository) Create(ctx context.Context, account *domain.CalendarAccount) error {
	if account == nil {
		return errors.New("adapter: create calendar account: nil account")
	}
	const q = `
		INSERT INTO calendar_account
			(id, member_id, household_id, provider, access_token_enc, refresh_token_enc,
			 token_expiry, sync_token, calendar_ids)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING created_at, updated_at`
	err := r.dbtx.QueryRow(ctx, q,
		account.ID.String(), account.MemberID.String(), account.HouseholdID.String(),
		account.Provider.String(), account.AccessTokenEnc, account.RefreshTokenEnc,
		account.TokenExpiry, account.SyncToken, account.CalendarIDs,
	).Scan(&account.CreatedAt, &account.UpdatedAt)
	if err != nil {
		if mapped := mapFKViolation(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("create calendar account: %w", err)
	}
	return nil
}

// Get returns the account, or domain.ErrCalendarAccountNotFound.
func (r *CalendarAccountRepository) Get(ctx context.Context, id domain.CalendarAccountID) (*domain.CalendarAccount, error) {
	const q = selectAccountColumns + ` WHERE id = $1`
	account, err := scanAccount(r.dbtx.QueryRow(ctx, q, id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrCalendarAccountNotFound
		}
		return nil, fmt.Errorf("get calendar account: %w", err)
	}
	return account, nil
}

// GetByMemberProvider returns the member's account for a provider, or
// domain.ErrCalendarAccountNotFound.
func (r *CalendarAccountRepository) GetByMemberProvider(ctx context.Context, memberID household.MemberID, provider domain.Provider) (*domain.CalendarAccount, error) {
	const q = selectAccountColumns + ` WHERE member_id = $1 AND provider = $2`
	account, err := scanAccount(r.dbtx.QueryRow(ctx, q, memberID.String(), provider.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrCalendarAccountNotFound
		}
		return nil, fmt.Errorf("get calendar account by member/provider: %w", err)
	}
	return account, nil
}

// UpdateTokens rewrites the full token set and selected calendars, resetting the
// sync token. It returns domain.ErrCalendarAccountNotFound when the id is unknown.
func (r *CalendarAccountRepository) UpdateTokens(ctx context.Context, id domain.CalendarAccountID, accessTokenEnc, refreshTokenEnc []byte, tokenExpiry time.Time, calendarIDs []string) error {
	const q = `
		UPDATE calendar_account
		   SET access_token_enc = $2, refresh_token_enc = $3, token_expiry = $4,
		       calendar_ids = $5, sync_token = NULL, updated_at = now()
		 WHERE id = $1`
	tag, err := r.dbtx.Exec(ctx, q, id.String(), accessTokenEnc, refreshTokenEnc, tokenExpiry, calendarIDs)
	if err != nil {
		return fmt.Errorf("update calendar account tokens: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrCalendarAccountNotFound
	}
	return nil
}

// UpdateSyncState persists a refreshed access token + expiry and the latest sync
// token. A nil refreshTokenEnc leaves the stored refresh token unchanged (via
// COALESCE); a non-nil value replaces it. It returns
// domain.ErrCalendarAccountNotFound when the id is unknown.
func (r *CalendarAccountRepository) UpdateSyncState(ctx context.Context, id domain.CalendarAccountID, accessTokenEnc, refreshTokenEnc []byte, tokenExpiry time.Time, syncToken *string) error {
	const q = `
		UPDATE calendar_account
		   SET access_token_enc = $2,
		       refresh_token_enc = COALESCE($3::bytea, refresh_token_enc),
		       token_expiry = $4, sync_token = $5, updated_at = now()
		 WHERE id = $1`
	tag, err := r.dbtx.Exec(ctx, q, id.String(), accessTokenEnc, refreshTokenEnc, tokenExpiry, syncToken)
	if err != nil {
		return fmt.Errorf("update calendar account sync state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrCalendarAccountNotFound
	}
	return nil
}

// ListByHousehold returns the household's connected accounts ordered by member.
func (r *CalendarAccountRepository) ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*domain.CalendarAccount, error) {
	const q = selectAccountColumns + ` WHERE household_id = $1 ORDER BY member_id, provider`
	return r.queryAccounts(ctx, "list calendar accounts by household", q, householdID.String())
}

// ListAll returns every connected account across households, ordered stably.
func (r *CalendarAccountRepository) ListAll(ctx context.Context) ([]*domain.CalendarAccount, error) {
	const q = selectAccountColumns + ` ORDER BY household_id, member_id, provider`
	return r.queryAccounts(ctx, "list all calendar accounts", q)
}

const selectAccountColumns = `
	SELECT id, member_id, household_id, provider, access_token_enc, refresh_token_enc,
	       token_expiry, sync_token, calendar_ids, created_at, updated_at
	  FROM calendar_account`

func (r *CalendarAccountRepository) queryAccounts(ctx context.Context, op, q string, args ...any) ([]*domain.CalendarAccount, error) {
	rows, err := r.dbtx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()

	accounts := make([]*domain.CalendarAccount, 0)
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", op, err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return accounts, nil
}

// row abstracts pgx.Row and pgx.Rows for the shared scan helper.
type row interface {
	Scan(dest ...any) error
}

func scanAccount(r row) (*domain.CalendarAccount, error) {
	var (
		account            domain.CalendarAccount
		idStr, memStr      string
		hhStr, providerStr string
	)
	if err := r.Scan(
		&idStr, &memStr, &hhStr, &providerStr, &account.AccessTokenEnc,
		&account.RefreshTokenEnc, &account.TokenExpiry, &account.SyncToken,
		&account.CalendarIDs, &account.CreatedAt, &account.UpdatedAt,
	); err != nil {
		return nil, err
	}
	id, err := domain.ParseCalendarAccountID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan calendar account: %w", err)
	}
	memberID, err := household.ParseMemberID(memStr)
	if err != nil {
		return nil, fmt.Errorf("scan calendar account: %w", err)
	}
	hhID, err := household.ParseHouseholdID(hhStr)
	if err != nil {
		return nil, fmt.Errorf("scan calendar account: %w", err)
	}
	provider, err := domain.ParseProvider(providerStr)
	if err != nil {
		return nil, fmt.Errorf("scan calendar account: %w", err)
	}
	account.ID, account.MemberID, account.HouseholdID, account.Provider = id, memberID, hhID, provider
	return &account, nil
}

// mapFKViolation maps a calendar_account FK violation to its domain sentinel, or
// nil when err is not a recognized FK violation.
func mapFKViolation(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != foreignKeyViolation {
		return nil
	}
	switch pgErr.ConstraintName {
	case calendarAccountHouseholdFK:
		return household.ErrHouseholdNotFound
	case calendarAccountMemberFK:
		return household.ErrMemberNotFound
	default:
		return nil
	}
}
