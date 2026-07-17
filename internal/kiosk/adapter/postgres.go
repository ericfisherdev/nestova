// Package adapter is the kiosk bounded context's infrastructure layer: the
// Postgres repository, the device-cookie auth middleware, and the HTTP
// handlers for kiosk provisioning (parent settings) and the kiosk shell.
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

// KioskDeviceRepository is the pgx-backed domain.KioskDeviceRepository. UUIDs
// are passed and scanned as text, matching the other adapters.
type KioskDeviceRepository struct {
	dbtx db.TX
}

var _ domain.KioskDeviceRepository = (*KioskDeviceRepository)(nil)

// NewKioskDeviceRepository constructs the repository with an injected query executor.
func NewKioskDeviceRepository(dbtx db.TX) *KioskDeviceRepository {
	if dbtx == nil {
		panic("kiosk/adapter: NewKioskDeviceRepository requires a non-nil db.TX")
	}
	return &KioskDeviceRepository{dbtx: dbtx}
}

const kioskDeviceColumns = `SELECT id, household_id, token_hash, name, created_at, revoked_at FROM kiosk_device`

// Create inserts a kiosk device and populates its created_at, mapping an
// unknown household to household.ErrHouseholdNotFound.
func (r *KioskDeviceRepository) Create(ctx context.Context, device *domain.KioskDevice) error {
	if device == nil {
		return errors.New("kiosk/adapter: create device: nil device")
	}
	if err := insertKioskDevice(ctx, r.dbtx, device); err != nil {
		if mapped := mapFKViolation(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("create kiosk device: %w", err)
	}
	return nil
}

// insertKioskDevice is the raw INSERT shared by KioskDeviceRepository.Create
// and ActivationCodeRepository.Redeem (which inserts the newly minted device
// as part of its own transaction, using the same tx as its db.TX executor).
// It populates device.CreatedAt on success and returns the raw driver error
// unmapped, so each caller applies its own error-mapping context.
func insertKioskDevice(ctx context.Context, dbtx db.TX, device *domain.KioskDevice) error {
	const q = `
		INSERT INTO kiosk_device (id, household_id, token_hash, name)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at`
	return dbtx.QueryRow(ctx, q,
		device.ID.String(), device.HouseholdID.String(), device.TokenHash, device.Name,
	).Scan(&device.CreatedAt)
}

// revokeActiveDevices revokes every currently active device for householdID,
// shared by ActivationCodeRepository.Redeem (which revokes the household's
// previous device as part of its atomic redemption transaction). Unlike
// KioskDeviceRepository.Revoke, this is not id-scoped and does not report an
// error when nothing was active — Redeem's household may have zero or one
// active device, and "nothing to revoke" is an expected, successful case.
func revokeActiveDevices(ctx context.Context, dbtx db.TX, householdID household.HouseholdID, revokedAt time.Time) error {
	const q = `UPDATE kiosk_device SET revoked_at = $2 WHERE household_id = $1 AND revoked_at IS NULL`
	_, err := dbtx.Exec(ctx, q, householdID.String(), revokedAt)
	return err
}

// GetByTokenHash returns the device whose token hash matches, regardless of
// revocation state. Returns domain.ErrKioskDeviceNotFound when no device
// matches.
func (r *KioskDeviceRepository) GetByTokenHash(ctx context.Context, tokenHash string) (*domain.KioskDevice, error) {
	device, err := scanKioskDevice(r.dbtx.QueryRow(ctx, kioskDeviceColumns+` WHERE token_hash = $1`, tokenHash))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrKioskDeviceNotFound
		}
		return nil, fmt.Errorf("get kiosk device by token hash: %w", err)
	}
	return device, nil
}

// Revoke sets revoked_at for the device within householdID. Returns
// domain.ErrKioskDeviceNotFound when the id is unknown in that household or is
// already revoked (the WHERE clause's revoked_at IS NULL guard is what makes a
// second Revoke on the same device report not-found instead of silently
// overwriting the original revocation timestamp).
func (r *KioskDeviceRepository) Revoke(ctx context.Context, householdID household.HouseholdID, id domain.KioskDeviceID, revokedAt time.Time) error {
	const q = `
		UPDATE kiosk_device SET revoked_at = $3
		 WHERE id = $1 AND household_id = $2 AND revoked_at IS NULL
		RETURNING id`
	var scanned string
	err := r.dbtx.QueryRow(ctx, q, id.String(), householdID.String(), revokedAt).Scan(&scanned)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrKioskDeviceNotFound
		}
		return fmt.Errorf("revoke kiosk device: %w", err)
	}
	return nil
}

// ListByHousehold returns the household's devices newest-first, or an empty
// slice when none exist.
func (r *KioskDeviceRepository) ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*domain.KioskDevice, error) {
	rows, err := r.dbtx.Query(ctx, kioskDeviceColumns+` WHERE household_id = $1 ORDER BY created_at DESC`, householdID.String())
	if err != nil {
		return nil, fmt.Errorf("list kiosk devices: %w", err)
	}
	defer rows.Close()

	devices := make([]*domain.KioskDevice, 0)
	for rows.Next() {
		device, err := scanKioskDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("list kiosk devices: scan: %w", err)
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list kiosk devices: %w", err)
	}
	return devices, nil
}

func scanKioskDevice(r row) (*domain.KioskDevice, error) {
	var (
		device       domain.KioskDevice
		idStr, hhStr string
	)
	if err := r.Scan(&idStr, &hhStr, &device.TokenHash, &device.Name, &device.CreatedAt, &device.RevokedAt); err != nil {
		return nil, err
	}
	id, err := domain.ParseKioskDeviceID(idStr)
	if err != nil {
		return nil, fmt.Errorf("parse kiosk device id: %w", err)
	}
	hh, err := household.ParseHouseholdID(hhStr)
	if err != nil {
		return nil, fmt.Errorf("parse household id: %w", err)
	}
	device.ID = id
	device.HouseholdID = hh
	return &device, nil
}
