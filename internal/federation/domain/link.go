// Package domain models the federation bounded context's instance link
// (NSTR-106): the binding between a Nestova household and the single
// Nestorage instance it is attached to, which every later push and
// reconciliation call is scoped by.
package domain

import (
	"errors"
	"net/url"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// InstanceLink binds one Nestova household to exactly one Nestorage
// instance (NSTR-106): the credential every later push and reconciliation
// call (NSTR-107/108) is scoped by. AttachedBy is nullable — the member who
// performed the attach may later leave the household (migration 00038's
// ON DELETE SET NULL) — while the link, and the fact that it was created by
// a real owner at AttachedAt, survives.
type InstanceLink struct {
	ID          InstanceLinkID
	HouseholdID household.HouseholdID
	BaseURL     string
	APIKeyEnc   []byte
	AttachedBy  *household.MemberID
	AttachedAt  time.Time
}

// Validate reports whether the link's own invariants hold, independent of
// any storage-layer uniqueness check (those are InstanceLinkRepository.
// Create's concern: ErrHouseholdAlreadyBound / ErrInstanceAlreadyBound).
func (l *InstanceLink) Validate() error {
	if _, err := NormalizeBaseURL(l.BaseURL); err != nil {
		return err
	}
	if len(l.APIKeyEnc) == 0 {
		return errors.New("federation: encrypted api key must not be empty")
	}
	return nil
}

// NormalizeBaseURL validates rawURL as an absolute http(s) url and returns
// its canonical form: lowercase scheme and host, no trailing slash, no
// query or fragment. Canonicalizing here — rather than only at the storage
// layer's unique index on lower(base_url) — is what makes
// LinkService.Attach's own "is this url already bound to a different
// household" pre-check agree with that index bit-for-bit.
func NormalizeBaseURL(rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", ErrInvalidBaseURL
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", ErrInvalidBaseURL
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", ErrInvalidBaseURL
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}
