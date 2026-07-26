package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/federation/app"
	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

// fakeLinkRepo is an in-memory domain.InstanceLinkRepository keyed by
// household id, with a secondary index by base url — mirroring the two
// unique constraints migration 00038 enforces at the storage layer, so
// LinkService's own pre-checks (refuseIfAlreadyBound) can be exercised
// without a database.
type fakeLinkRepo struct {
	byHousehold map[household.HouseholdID]*domain.InstanceLink
	byBaseURL   map[string]*domain.InstanceLink
	createCalls int
}

func newFakeLinkRepo() *fakeLinkRepo {
	return &fakeLinkRepo{byHousehold: map[household.HouseholdID]*domain.InstanceLink{}, byBaseURL: map[string]*domain.InstanceLink{}}
}

func (f *fakeLinkRepo) Create(_ context.Context, link *domain.InstanceLink) error {
	f.createCalls++
	if _, ok := f.byHousehold[link.HouseholdID]; ok {
		return domain.ErrHouseholdAlreadyBound
	}
	if _, ok := f.byBaseURL[link.BaseURL]; ok {
		return domain.ErrInstanceAlreadyBound
	}
	f.byHousehold[link.HouseholdID] = link
	f.byBaseURL[link.BaseURL] = link
	return nil
}

func (f *fakeLinkRepo) GetByHousehold(_ context.Context, id household.HouseholdID) (*domain.InstanceLink, error) {
	if link, ok := f.byHousehold[id]; ok {
		return link, nil
	}
	return nil, domain.ErrLinkNotFound
}

func (f *fakeLinkRepo) GetByBaseURL(_ context.Context, baseURL string) (*domain.InstanceLink, error) {
	if link, ok := f.byBaseURL[baseURL]; ok {
		return link, nil
	}
	return nil, domain.ErrLinkNotFound
}

func (f *fakeLinkRepo) DeleteByHousehold(_ context.Context, id household.HouseholdID) error {
	if link, ok := f.byHousehold[id]; ok {
		delete(f.byHousehold, id)
		delete(f.byBaseURL, link.BaseURL)
	}
	return nil
}

// fakeVerifier is a scripted instanceVerifier: it records every call's
// arguments and returns err, so tests can assert both what Attach passed
// through to verification and that a failure never reaches it at all.
type fakeVerifier struct {
	err     error
	calls   int
	seenKey string
	seenHH  household.HouseholdID
	seenURL string
}

func (f *fakeVerifier) Verify(_ context.Context, baseURL, apiKey string, householdID household.HouseholdID) error {
	f.calls++
	f.seenURL = baseURL
	f.seenKey = apiKey
	f.seenHH = householdID
	return f.err
}

func mustService(t *testing.T, repo domain.InstanceLinkRepository, verifier *fakeVerifier, cipher *crypto.Cipher) *app.LinkService {
	t.Helper()
	svc, err := app.NewLinkService(repo, cipher, verifier, discardLogger())
	if err != nil {
		t.Fatalf("NewLinkService: %v", err)
	}
	return svc
}

func TestNewLinkServiceRequiresDependencies(t *testing.T) {
	repo := newFakeLinkRepo()
	cipher := testCipher(t)
	verifier := &fakeVerifier{}
	logger := discardLogger()

	if _, err := app.NewLinkService(nil, cipher, verifier, logger); err == nil {
		t.Error("NewLinkService(nil repo) error = nil, want error")
	}
	if _, err := app.NewLinkService(repo, nil, verifier, logger); err == nil {
		t.Error("NewLinkService(nil cipher) error = nil, want error")
	}
	if _, err := app.NewLinkService(repo, cipher, nil, logger); err == nil {
		t.Error("NewLinkService(nil verifier) error = nil, want error")
	}
	if _, err := app.NewLinkService(repo, cipher, verifier, nil); err == nil {
		t.Error("NewLinkService(nil logger) error = nil, want error")
	}
}

func TestLinkServiceAttachSuccess(t *testing.T) {
	repo := newFakeLinkRepo()
	verifier := &fakeVerifier{}
	svc := mustService(t, repo, verifier, testCipher(t))
	hh := household.NewHouseholdID()
	member := household.NewMemberID()

	link, err := svc.Attach(context.Background(), hh, member, "https://Nestorage.Example.ts.net/", "  secret-key  ")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if link.HouseholdID != hh {
		t.Errorf("link.HouseholdID = %v, want %v", link.HouseholdID, hh)
	}
	if link.BaseURL != "https://nestorage.example.ts.net" {
		t.Errorf("link.BaseURL = %q, want normalized", link.BaseURL)
	}
	if link.AttachedBy == nil || *link.AttachedBy != member {
		t.Errorf("link.AttachedBy = %v, want %v", link.AttachedBy, member)
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier.calls = %d, want 1", verifier.calls)
	}
	if verifier.seenKey != "secret-key" {
		t.Errorf("verifier saw key %q, want trimmed \"secret-key\"", verifier.seenKey)
	}
	if verifier.seenHH != hh {
		t.Errorf("verifier saw household %v, want %v", verifier.seenHH, hh)
	}
	if verifier.seenURL != link.BaseURL {
		t.Errorf("verifier saw url %q, want %q", verifier.seenURL, link.BaseURL)
	}

	// The stored key must be encrypted, never the raw value (AC: "encrypted
	// at rest").
	if strings.Contains(string(link.APIKeyEnc), "secret-key") {
		t.Error("stored APIKeyEnc contains the raw api key")
	}
}

func TestLinkServiceAttachRefusesWhenVerifyFails(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr error
	}{
		{name: "invalid key", err: domain.ErrInvalidAPIKey, wantErr: domain.ErrInvalidAPIKey},
		{name: "unreachable", err: domain.ErrInstanceUnreachable, wantErr: domain.ErrInstanceUnreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeLinkRepo()
			verifier := &fakeVerifier{err: tt.err}
			svc := mustService(t, repo, verifier, testCipher(t))

			_, err := svc.Attach(context.Background(), household.NewHouseholdID(), household.NewMemberID(), "https://nestorage.example.ts.net", "key")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Attach() error = %v, want %v", err, tt.wantErr)
			}
			if repo.createCalls != 0 {
				t.Errorf("repo.createCalls = %d, want 0 — a failed verify must store nothing", repo.createCalls)
			}
		})
	}
}

func TestLinkServiceAttachRefusesInvalidURL(t *testing.T) {
	repo := newFakeLinkRepo()
	verifier := &fakeVerifier{}
	svc := mustService(t, repo, verifier, testCipher(t))

	_, err := svc.Attach(context.Background(), household.NewHouseholdID(), household.NewMemberID(), "not-a-url", "key")
	if !errors.Is(err, domain.ErrInvalidBaseURL) {
		t.Fatalf("Attach() error = %v, want ErrInvalidBaseURL", err)
	}
	if verifier.calls != 0 {
		t.Errorf("verifier.calls = %d, want 0 — an invalid url must never reach Verify", verifier.calls)
	}
	if repo.createCalls != 0 {
		t.Errorf("repo.createCalls = %d, want 0", repo.createCalls)
	}
}

func TestLinkServiceAttachRefusesEmptyAPIKey(t *testing.T) {
	repo := newFakeLinkRepo()
	verifier := &fakeVerifier{}
	svc := mustService(t, repo, verifier, testCipher(t))

	_, err := svc.Attach(context.Background(), household.NewHouseholdID(), household.NewMemberID(), "https://nestorage.example.ts.net", "   ")
	if err == nil {
		t.Fatal("Attach() error = nil, want error for a blank api key")
	}
	if verifier.calls != 0 {
		t.Errorf("verifier.calls = %d, want 0", verifier.calls)
	}
}

func TestLinkServiceAttachRefusesSecondBindingForHousehold(t *testing.T) {
	repo := newFakeLinkRepo()
	verifier := &fakeVerifier{}
	svc := mustService(t, repo, verifier, testCipher(t))
	hh := household.NewHouseholdID()
	member := household.NewMemberID()

	if _, err := svc.Attach(context.Background(), hh, member, "https://one.example.ts.net", "key"); err != nil {
		t.Fatalf("first Attach: %v", err)
	}
	verifier.calls = 0

	_, err := svc.Attach(context.Background(), hh, member, "https://two.example.ts.net", "key")
	if !errors.Is(err, domain.ErrHouseholdAlreadyBound) {
		t.Fatalf("second Attach() error = %v, want ErrHouseholdAlreadyBound", err)
	}
	if verifier.calls != 0 {
		t.Errorf("verifier.calls = %d, want 0 — a household already bound must never reach Verify", verifier.calls)
	}
}

func TestLinkServiceAttachRefusesURLBoundToAnotherHousehold(t *testing.T) {
	repo := newFakeLinkRepo()
	verifier := &fakeVerifier{}
	svc := mustService(t, repo, verifier, testCipher(t))
	first := household.NewHouseholdID()
	second := household.NewHouseholdID()
	member := household.NewMemberID()

	if _, err := svc.Attach(context.Background(), first, member, "https://shared.example.ts.net", "key"); err != nil {
		t.Fatalf("first Attach: %v", err)
	}
	verifier.calls = 0

	_, err := svc.Attach(context.Background(), second, member, "https://shared.example.ts.net", "key")
	if !errors.Is(err, domain.ErrInstanceAlreadyBound) {
		t.Fatalf("second Attach() error = %v, want ErrInstanceAlreadyBound", err)
	}
	if verifier.calls != 0 {
		t.Errorf("verifier.calls = %d, want 0", verifier.calls)
	}
}

func TestLinkServiceAttachNeverLogsAPIKey(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	repo := newFakeLinkRepo()
	verifier := &fakeVerifier{}
	svc, err := app.NewLinkService(repo, testCipher(t), verifier, logger)
	if err != nil {
		t.Fatalf("NewLinkService: %v", err)
	}

	const rawKey = "super-secret-nestorage-key"
	if _, err := svc.Attach(context.Background(), household.NewHouseholdID(), household.NewMemberID(), "https://nestorage.example.ts.net", rawKey); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if strings.Contains(buf.String(), rawKey) {
		t.Errorf("log output contains the raw api key: %s", buf.String())
	}
}

func TestLinkServiceDetachIsIdempotent(t *testing.T) {
	repo := newFakeLinkRepo()
	svc := mustService(t, repo, &fakeVerifier{}, testCipher(t))
	hh := household.NewHouseholdID()

	if err := svc.Detach(context.Background(), hh); err != nil {
		t.Fatalf("Detach on never-attached household: %v", err)
	}

	member := household.NewMemberID()
	if _, err := svc.Attach(context.Background(), hh, member, "https://nestorage.example.ts.net", "key"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := svc.Detach(context.Background(), hh); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if _, err := svc.Status(context.Background(), hh); !errors.Is(err, domain.ErrLinkNotFound) {
		t.Errorf("Status() after Detach error = %v, want ErrLinkNotFound", err)
	}
}

func TestLinkServiceStatusNotFound(t *testing.T) {
	repo := newFakeLinkRepo()
	svc := mustService(t, repo, &fakeVerifier{}, testCipher(t))

	if _, err := svc.Status(context.Background(), household.NewHouseholdID()); !errors.Is(err, domain.ErrLinkNotFound) {
		t.Errorf("Status() error = %v, want ErrLinkNotFound", err)
	}
}

func TestLinkServiceCredentialRoundTrips(t *testing.T) {
	repo := newFakeLinkRepo()
	svc := mustService(t, repo, &fakeVerifier{}, testCipher(t))
	hh := household.NewHouseholdID()
	member := household.NewMemberID()

	if _, err := svc.Attach(context.Background(), hh, member, "https://nestorage.example.ts.net", "the-real-key"); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	baseURL, apiKey, err := svc.Credential(context.Background(), hh)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if baseURL != "https://nestorage.example.ts.net" {
		t.Errorf("Credential baseURL = %q, want normalized", baseURL)
	}
	if apiKey != "the-real-key" {
		t.Errorf("Credential apiKey = %q, want %q", apiKey, "the-real-key")
	}
}

func TestLinkServiceCredentialNotFound(t *testing.T) {
	repo := newFakeLinkRepo()
	svc := mustService(t, repo, &fakeVerifier{}, testCipher(t))

	if _, _, err := svc.Credential(context.Background(), household.NewHouseholdID()); !errors.Is(err, domain.ErrLinkNotFound) {
		t.Errorf("Credential() error = %v, want ErrLinkNotFound", err)
	}
}
