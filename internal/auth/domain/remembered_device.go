package domain

import (
	"context"
	"errors"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// MaxUserAgentLength bounds RememberedDevice.UserAgent. It must match the
// CHECK on nestova.remembered_device.user_agent (NES-200).
const MaxUserAgentLength = 255

// ErrRememberedDeviceNotFound is returned by
// RememberedDeviceRepository.MarkUsed when no live row matches the presented
// token for the member: the token is unknown, expired, revoked, or belongs to
// a different member. It is deliberately one sentinel so a caller cannot tell
// those apart.
var ErrRememberedDeviceNotFound = errors.New("auth: remembered device not found")

// RememberedDevice is one server-side "remember this device" record (NES-200).
// TokenHash is the SHA-256 of the opaque cookie value; the raw token is never
// stored. UserAgent is display metadata only and is not used to authorize.
type RememberedDevice struct {
	ID         RememberedDeviceID
	MemberID   household.MemberID
	TokenHash  []byte
	UserAgent  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt time.Time
}

// RememberedDeviceRepository is the outbound port for remembered-device
// records. Implementations live in the adapter package.
//
// Error contracts:
//   - Create returns household.ErrMemberNotFound when MemberID does not exist.
//   - MarkUsed atomically checks that a row with tokenHash exists for
//     memberID and has not expired as of now, stamps last_used_at, and returns
//     ErrRememberedDeviceNotFound otherwise.
//   - RevokeAllForMember deletes every device of memberID, live or expired,
//     and succeeds when there are none.
type RememberedDeviceRepository interface {
	Create(ctx context.Context, device *RememberedDevice) error
	MarkUsed(ctx context.Context, memberID household.MemberID, tokenHash []byte, now time.Time) error
	RevokeAllForMember(ctx context.Context, memberID household.MemberID) error
}
