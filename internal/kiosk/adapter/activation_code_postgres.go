package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/kiosk/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// kioskHouseholdLockNamespace is the first key of the two-integer form of
// Redeem's per-household pg_advisory_xact_lock. Postgres tracks the
// single-bigint form (e.g. cmd/server/provisioning.go's onboardingAdvisoryLock)
// and the two-integer form as genuinely separate lock spaces — the bigint
// form's pg_locks row has objsubid=1, the two-integer form's has objsubid=2
// (postgresql.org/docs/current/view-pg-locks.html) — so this cannot collide
// with that lock regardless of numeric value. Chosen as the ASCII bytes of
// "KIOS" purely as a memorable, human-readable tag.
const kioskHouseholdLockNamespace int32 = 0x4B494F53

// ActivationCodeRepository is the pgx-backed domain.ActivationCodeRepository.
type ActivationCodeRepository struct {
	dbtx db.TX
}

var _ domain.ActivationCodeRepository = (*ActivationCodeRepository)(nil)

// NewActivationCodeRepository constructs the repository with an injected query executor.
func NewActivationCodeRepository(dbtx db.TX) *ActivationCodeRepository {
	if dbtx == nil {
		panic("kiosk/adapter: NewActivationCodeRepository requires a non-nil db.TX")
	}
	return &ActivationCodeRepository{dbtx: dbtx}
}

// Create inserts an activation code and populates its created_at, mapping an
// unknown household to household.ErrHouseholdNotFound.
func (r *ActivationCodeRepository) Create(ctx context.Context, code *domain.ActivationCode) error {
	if code == nil {
		return errors.New("kiosk/adapter: create activation code: nil code")
	}
	const q = `
		INSERT INTO kiosk_activation_code (id, household_id, code_hash, name, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`
	err := r.dbtx.QueryRow(ctx, q,
		code.ID.String(), code.HouseholdID.String(), code.CodeHash, code.Name, code.ExpiresAt,
	).Scan(&code.CreatedAt)
	if err != nil {
		if mapped := mapFKViolation(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("create activation code: %w", err)
	}
	return nil
}

// Redeem atomically validates codeHash, marks it used, revokes the code's
// household's previously active device, and inserts device — see the
// domain.ActivationCodeRepository doc comment for the full contract. Every
// step after the initial lookup runs inside one transaction (opened directly
// against the pool this repository was constructed with, mirroring
// tasksadapter.RecurringTaskRepository.CreateWithRotation's pattern for a
// self-contained, single-bounded-context atomic write): if the device insert
// fails, the whole transaction rolls back, so the code is NOT left used and
// the previous device (if any) is NOT left revoked.
func (r *ActivationCodeRepository) Redeem(ctx context.Context, codeHash string, now time.Time, device *domain.KioskDevice) error {
	if device == nil {
		return errors.New("kiosk/adapter: redeem activation code: nil device")
	}

	beginner, ok := r.dbtx.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return errors.New("redeem activation code: executor does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("redeem activation code: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// FOR UPDATE locks the row for the remainder of this transaction, so a
	// second, concurrent Redeem of the same code blocks here until the first
	// commits (and then correctly observes used_at set) or rolls back —
	// closing the check-then-mark-used race a plain SELECT would leave open.
	const lookup = `
		SELECT id, household_id, name, expires_at, used_at
		  FROM kiosk_activation_code
		 WHERE code_hash = $1
		   FOR UPDATE`
	var (
		codeIDStr, hhStr, name string
		expiresAt              time.Time
		usedAt                 *time.Time
	)
	err = tx.QueryRow(ctx, lookup, codeHash).Scan(&codeIDStr, &hhStr, &name, &expiresAt, &usedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrActivationCodeNotFound
		}
		return fmt.Errorf("redeem activation code: lookup: %w", err)
	}
	if usedAt != nil {
		return domain.ErrActivationCodeUsed
	}
	if !now.Before(expiresAt) {
		return domain.ErrActivationCodeExpired
	}

	hh, err := household.ParseHouseholdID(hhStr)
	if err != nil {
		return fmt.Errorf("redeem activation code: parse household id: %w", err)
	}

	// Serialize the revoke-then-insert replacement per household: two
	// activation codes for the SAME household redeemed concurrently (two
	// different browser tabs, or a retried request racing the original) must
	// never both revoke and insert interleaved — that could leave two active
	// devices, or revoke the very device the other transaction just created.
	// pg_advisory_xact_lock blocks the second transaction here until the
	// first commits or rolls back, auto-releasing at the end of this
	// transaction either way. The two-key form scopes this lock to a private
	// namespace (kioskHouseholdLockNamespace) distinct from any other
	// advisory lock in the app (e.g. onboarding's own single-key lock in
	// cmd/server/provisioning.go), keyed by hashtext(household_id) so
	// different households never contend with each other.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, kioskHouseholdLockNamespace, hh.String()); err != nil {
		return fmt.Errorf("redeem activation code: acquire household lock: %w", err)
	}

	if _, err := tx.Exec(ctx, `UPDATE kiosk_activation_code SET used_at = $2 WHERE id = $1`, codeIDStr, now); err != nil {
		return fmt.Errorf("redeem activation code: mark used: %w", err)
	}

	if err := revokeActiveDevices(ctx, tx, hh, now); err != nil {
		return fmt.Errorf("redeem activation code: revoke previous device: %w", err)
	}

	// Redeem owns HouseholdID/Name on the inserted device — see the
	// domain.ActivationCodeRepository.Redeem contract: the caller only
	// supplies ID and TokenHash.
	device.HouseholdID = hh
	device.Name = name
	if err := insertKioskDevice(ctx, tx, device); err != nil {
		return fmt.Errorf("redeem activation code: insert device: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("redeem activation code: commit: %w", err)
	}
	return nil
}
