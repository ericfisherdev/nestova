package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// rememberedDeviceMemberFK is the member FK on remembered_device (00043); a
// violation means the member does not exist.
const rememberedDeviceMemberFK = "remembered_device_member_id_fkey"

// RememberedDeviceRepository is the pgx-backed
// authdomain.RememberedDeviceRepository over nestova.remembered_device.
type RememberedDeviceRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ authdomain.RememberedDeviceRepository = (*RememberedDeviceRepository)(nil)

// NewRememberedDeviceRepository constructs the repository with an injected
// query executor.
func NewRememberedDeviceRepository(dbtx db.TX) *RememberedDeviceRepository {
	if dbtx == nil {
		panic("adapter: NewRememberedDeviceRepository requires a non-nil db.TX")
	}
	return &RememberedDeviceRepository{dbtx: dbtx}
}

// Create inserts device and sweeps the member's already-expired rows, so the
// table does not accumulate dead devices. Returns household.ErrMemberNotFound
// when the member does not exist.
func (r *RememberedDeviceRepository) Create(ctx context.Context, device *authdomain.RememberedDevice) error {
	const sweep = `DELETE FROM remembered_device WHERE member_id = $1 AND expires_at <= $2`
	if _, err := r.dbtx.Exec(ctx, sweep, device.MemberID.String(), device.CreatedAt); err != nil {
		return fmt.Errorf("sweep expired remembered devices: %w", err)
	}

	const ins = `
		INSERT INTO remembered_device (id, member_id, token_hash, user_agent, created_at, expires_at, last_used_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := r.dbtx.Exec(ctx, ins,
		device.ID.String(), device.MemberID.String(), device.TokenHash, device.UserAgent,
		device.CreatedAt, device.ExpiresAt, device.LastUsedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == rememberedDeviceMemberFK {
			return household.ErrMemberNotFound
		}
		return fmt.Errorf("insert remembered device: %w", err)
	}
	return nil
}

// MarkUsed stamps last_used_at on the member's live row for tokenHash, or
// returns authdomain.ErrRememberedDeviceNotFound when there is none.
func (r *RememberedDeviceRepository) MarkUsed(ctx context.Context, memberID household.MemberID, tokenHash []byte, now time.Time) error {
	const q = `
		UPDATE remembered_device
		   SET last_used_at = $3
		 WHERE member_id = $1
		   AND token_hash = $2
		   AND expires_at > $3`
	tag, err := r.dbtx.Exec(ctx, q, memberID.String(), tokenHash, now)
	if err != nil {
		return fmt.Errorf("mark remembered device used: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return authdomain.ErrRememberedDeviceNotFound
	}
	return nil
}

// RevokeAllForMember deletes every remembered device of memberID.
func (r *RememberedDeviceRepository) RevokeAllForMember(ctx context.Context, memberID household.MemberID) error {
	const q = `DELETE FROM remembered_device WHERE member_id = $1`
	if _, err := r.dbtx.Exec(ctx, q, memberID.String()); err != nil {
		return fmt.Errorf("revoke remembered devices: %w", err)
	}
	return nil
}
