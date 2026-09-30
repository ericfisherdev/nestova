package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// RememberDeviceTTL bounds how long a "remember this device" cookie skips
// the login MFA step (NES-135's acceptance criterion: 30 days). A
// remembered device is exempt from the LOGIN-time prompt only — it is not
// exempt from RequireStepUp's own freshness check on a security-sensitive
// action.
const RememberDeviceTTL = 30 * 24 * time.Hour

// rememberTokenBytes is the entropy of a remember-device token: 256 bits, far
// beyond guessing, so a plain SHA-256 (no salt or stretching) is the right
// at-rest hash.
const rememberTokenBytes = 32

// RememberDeviceService issues, checks and revokes the server-side
// "remember this device" tokens (NES-200). The cookie value is a random opaque
// token; only its SHA-256 is stored, so each use is a database lookup and
// revocation is a delete. A token is a bearer credential for the login MFA
// prompt only — it is not bound to a user agent or address, because both
// change legitimately (browser updates, roaming between networks) and the user
// agent is attacker-controlled anyway. Revocation, not binding, is the
// defense: see MFAService for the events that revoke.
type RememberDeviceService struct {
	repo authdomain.RememberedDeviceRepository
}

// NewRememberDeviceService constructs the service with its repository.
func NewRememberDeviceService(repo authdomain.RememberedDeviceRepository) (*RememberDeviceService, error) {
	if repo == nil {
		return nil, errors.New("auth: NewRememberDeviceService requires a non-nil RememberedDeviceRepository")
	}
	return &RememberDeviceService{repo: repo}, nil
}

// Issue records a new remembered device for memberID, valid until
// now+RememberDeviceTTL, and returns the raw token to hand to the browser.
// userAgent is stored (truncated to authdomain.MaxUserAgentLength runes) for
// display only.
//
// Returns authdomain.ErrMFANotEnrolled (wrapped) when memberID has no confirmed
// MFA enrollment at write time, e.g. an owner reset raced the login; nothing
// is stored then.
func (s *RememberDeviceService) Issue(ctx context.Context, memberID household.MemberID, userAgent string, now time.Time) (string, error) {
	raw := make([]byte, rememberTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("remember device: generate token: %w", err)
	}
	device := &authdomain.RememberedDevice{
		ID:         authdomain.NewRememberedDeviceID(),
		MemberID:   memberID,
		TokenHash:  hashRememberToken(raw),
		UserAgent:  truncateRunes(userAgent, authdomain.MaxUserAgentLength),
		CreatedAt:  now,
		ExpiresAt:  now.Add(RememberDeviceTTL),
		LastUsedAt: now,
	}
	if err := s.repo.Create(ctx, device); err != nil {
		return "", fmt.Errorf("remember device: issue: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// IsRemembered reports whether token is a live remembered-device token for
// memberID as of now, stamping its last-used time when it is. A malformed,
// unknown, expired, revoked or other-member's token is simply false — never an
// error — so a caller cannot distinguish them. A non-nil error means the
// lookup itself failed.
func (s *RememberDeviceService) IsRemembered(ctx context.Context, memberID household.MemberID, token string, now time.Time) (bool, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != rememberTokenBytes {
		return false, nil
	}
	err = s.repo.MarkUsed(ctx, memberID, hashRememberToken(raw), now)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, authdomain.ErrRememberedDeviceNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("remember device: check: %w", err)
	}
}

// RevokeAll deletes every remembered device of memberID, so no copy of a
// previously issued cookie skips the login MFA prompt again.
func (s *RememberDeviceService) RevokeAll(ctx context.Context, memberID household.MemberID) error {
	if err := s.repo.RevokeAllForMember(ctx, memberID); err != nil {
		return fmt.Errorf("remember device: revoke all: %w", err)
	}
	return nil
}

func hashRememberToken(raw []byte) []byte {
	sum := sha256.Sum256(raw)
	return sum[:]
}

// truncateRunes drops invalid UTF-8 (Postgres text rejects it) and cuts s to
// at most n runes, never splitting a multi-byte rune.
func truncateRunes(s string, n int) string {
	runes := []rune(strings.ToValidUTF8(s, ""))
	if len(runes) > n {
		runes = runes[:n]
	}
	return string(runes)
}
