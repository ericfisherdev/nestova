// Package app holds the federation bounded context's application service:
// LinkService, which attaches, detaches, and reads a household's Nestorage
// instance link (NSTR-106).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// secretCipher is the slice of the crypto cipher the service depends on,
// satisfied by *crypto.Cipher — the identical two-method interface
// calendar/app.AccountService declares (ISP: LinkService needs neither more
// nor less of the cipher's surface).
type secretCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// instanceVerifier is the slice of the Nestorage HTTP client the service
// depends on, satisfied by adapter.NestorageClient and faked in tests. It
// returns domain.ErrInvalidAPIKey or domain.ErrInstanceUnreachable on
// failure — never a bare error — so Attach can propagate a specific reason
// straight through to the settings UI.
type instanceVerifier interface {
	Verify(ctx context.Context, baseURL, apiKey string, householdID household.HouseholdID) error
}

// LinkService attaches, detaches, and reads the household's single
// federation instance link (NSTR-106). The api key is encrypted at rest
// with the injected cipher and is never logged in any branch — only the
// instance host and ids are.
type LinkService struct {
	repo     domain.InstanceLinkRepository
	cipher   secretCipher
	verifier instanceVerifier
	logger   *slog.Logger
}

// NewLinkService constructs the service with injected dependencies,
// returning an error on any nil dependency (mirrors calendar/app.
// NewAccountService's constructor-injection convention).
func NewLinkService(repo domain.InstanceLinkRepository, cipher secretCipher, verifier instanceVerifier, logger *slog.Logger) (*LinkService, error) {
	if repo == nil {
		return nil, errors.New("federation: NewLinkService requires a non-nil repository")
	}
	if cipher == nil {
		return nil, errors.New("federation: NewLinkService requires a non-nil cipher")
	}
	if verifier == nil {
		return nil, errors.New("federation: NewLinkService requires a non-nil verifier")
	}
	if logger == nil {
		return nil, errors.New("federation: NewLinkService requires a non-nil logger")
	}
	return &LinkService{repo: repo, cipher: cipher, verifier: verifier, logger: logger}, nil
}

// Attach binds householdID to the Nestorage instance at rawBaseURL,
// verifying rawKey against the LIVE instance before anything is stored.
// Refuses when householdID already has a link, or when the normalized url
// is already linked to a DIFFERENT household — both checked here as a
// friendly pre-check and enforced race-safe by the repository's own unique
// constraints. Returns domain.ErrInvalidAPIKey or
// domain.ErrInstanceUnreachable when verification fails, with nothing
// stored either way.
func (s *LinkService) Attach(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID, rawBaseURL, rawKey string) (*domain.InstanceLink, error) {
	baseURL, err := domain.NormalizeBaseURL(rawBaseURL)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(rawKey)
	if key == "" {
		return nil, errors.New("federation: api key must not be empty")
	}

	if err := s.refuseIfAlreadyBound(ctx, householdID, baseURL); err != nil {
		return nil, err
	}

	if err := s.verifier.Verify(ctx, baseURL, key, householdID); err != nil {
		return nil, err
	}

	keyEnc, err := s.cipher.Encrypt([]byte(key))
	if err != nil {
		return nil, fmt.Errorf("encrypt nestorage api key: %w", err)
	}

	link := &domain.InstanceLink{
		ID:          domain.NewInstanceLinkID(),
		HouseholdID: householdID,
		BaseURL:     baseURL,
		APIKeyEnc:   keyEnc,
		AttachedBy:  &memberID,
		AttachedAt:  time.Now(),
	}
	if err := link.Validate(); err != nil {
		return nil, err
	}
	if err := s.repo.Create(ctx, link); err != nil {
		return nil, fmt.Errorf("create instance link: %w", err)
	}

	s.logger.InfoContext(ctx, "federation instance attached",
		"household_id", householdID.String(), "instance_host", instanceHost(baseURL))
	return link, nil
}

// refuseIfAlreadyBound returns domain.ErrHouseholdAlreadyBound when
// householdID already has a link, or domain.ErrInstanceAlreadyBound when
// baseURL is already linked to a different household. Both are pre-checks:
// the repository's own unique constraints are the race-safe backstop
// Create relies on regardless of what this method observes.
func (s *LinkService) refuseIfAlreadyBound(ctx context.Context, householdID household.HouseholdID, baseURL string) error {
	switch _, err := s.repo.GetByHousehold(ctx, householdID); {
	case err == nil:
		return domain.ErrHouseholdAlreadyBound
	case !errors.Is(err, domain.ErrLinkNotFound):
		return fmt.Errorf("look up existing household link: %w", err)
	}

	switch existing, err := s.repo.GetByBaseURL(ctx, baseURL); {
	case err == nil && existing.HouseholdID != householdID:
		return domain.ErrInstanceAlreadyBound
	case err != nil && !errors.Is(err, domain.ErrLinkNotFound):
		return fmt.Errorf("look up existing instance link: %w", err)
	}
	return nil
}

// Detach removes householdID's federation instance link, if any — this is
// what clears the stored credential. Idempotent: detaching an already-
// unbound household is not an error.
func (s *LinkService) Detach(ctx context.Context, householdID household.HouseholdID) error {
	if err := s.repo.DeleteByHousehold(ctx, householdID); err != nil {
		return fmt.Errorf("delete instance link: %w", err)
	}
	s.logger.InfoContext(ctx, "federation instance detached", "household_id", householdID.String())
	return nil
}

// Status returns householdID's current link, or domain.ErrLinkNotFound —
// for the settings section's connection-state display.
func (s *LinkService) Status(ctx context.Context, householdID household.HouseholdID) (*domain.InstanceLink, error) {
	link, err := s.repo.GetByHousehold(ctx, householdID)
	if err != nil {
		return nil, err
	}
	return link, nil
}

// Credential decrypts and returns householdID's stored base url and api
// key, for the reconciliation and push tickets (NSTR-107/108) — decryption
// stays inside this service so the cipher is never handed to another
// bounded context. Returns domain.ErrLinkNotFound when unbound.
func (s *LinkService) Credential(ctx context.Context, householdID household.HouseholdID) (baseURL, apiKey string, err error) {
	link, err := s.repo.GetByHousehold(ctx, householdID)
	if err != nil {
		return "", "", err
	}
	decrypted, err := s.cipher.Decrypt(link.APIKeyEnc)
	if err != nil {
		return "", "", fmt.Errorf("decrypt nestorage api key: %w", err)
	}
	return link.BaseURL, string(decrypted), nil
}

// instanceHost extracts the host from an already-normalized base url for
// logging, falling back to the full value on the (unreachable, since
// baseURL is always NormalizeBaseURL's own output) chance it fails to
// parse — never the api key, in any case.
func instanceHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	return u.Host
}
