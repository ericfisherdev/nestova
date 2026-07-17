package app_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/app"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// --- fakes ---

// jpegLikeBytes starts with the real JPEG SOI marker so
// ChoreProofPhotoService.captureAndScrub treats it as a JPEG candidate and
// calls the ChoreProofExif fake — the fake doesn't need genuinely decodable
// image data since fakePhotoStore.Put (reused from service_test.go) only
// hashes bytes, it never decodes them.
func jpegLikeBytes(payload string) []byte {
	return append([]byte{0xFF, 0xD8}, []byte(payload)...)
}

// scrubbedMarker is prepended by fakeChoreProofExif.Scrub so a test can
// assert PhotoStore.Put received the SCRUBBED bytes, not the raw upload.
const scrubbedMarker = "scrubbed:"

type fakeChoreProofExif struct {
	taken       *time.Time
	orientation int
	scrubErr    error
	scrubCalls  int
	lastOrient  int
}

func (f *fakeChoreProofExif) TakenAtAndOrientation([]byte) (*time.Time, int) {
	return f.taken, f.orientation
}

func (f *fakeChoreProofExif) Scrub(data []byte, orientation int) ([]byte, error) {
	f.scrubCalls++
	f.lastOrient = orientation
	if f.scrubErr != nil {
		return nil, f.scrubErr
	}
	return append([]byte(scrubbedMarker), data...), nil
}

// fakeTaskInstancePhotoRepo fakes domain.TaskInstancePhotoRepository.
// instanceExists defaults to true via newFakeTaskInstancePhotoRepo (below) —
// most tests are not exercising the InstanceExists preflight and need
// Upload to sail past it.
type fakeTaskInstancePhotoRepo struct {
	created             []*domain.TaskInstancePhoto
	createErr           error
	latestTakenAt       time.Time
	latestOK            bool
	latestErr           error
	latestCalls         int
	instanceExists      bool
	instanceExistsErr   error
	instanceExistsCalls int
	// simulateOrderingCheck, when true, makes Create itself apply the
	// before/after ordering rule (domain.AfterPrecedesBefore) against
	// latestTakenAt/latestOK — mirroring what
	// TaskInstancePhotoRepository.Create actually does atomically in
	// Postgres (see its doc) — so app-layer tests can exercise
	// ChoreProofPhotoService.Upload's error propagation for
	// ErrAfterPrecedesBefore without a real database. The service itself no
	// longer performs this check (moved into Create for atomicity, NES-119
	// review); a fake that didn't simulate it at all would make that
	// propagation path untestable at this layer.
	simulateOrderingCheck bool
}

func newFakeTaskInstancePhotoRepo() *fakeTaskInstancePhotoRepo {
	return &fakeTaskInstancePhotoRepo{instanceExists: true}
}

func (f *fakeTaskInstancePhotoRepo) Create(_ context.Context, p *domain.TaskInstancePhoto) error {
	if f.simulateOrderingCheck && p.Kind == domain.PhotoKindAfter && f.latestOK && domain.AfterPrecedesBefore(p.TakenAt, f.latestTakenAt) {
		return domain.ErrAfterPrecedesBefore
	}
	if f.createErr != nil {
		return f.createErr
	}
	p.UploadedAt = time.Now().UTC()
	f.created = append(f.created, p)
	return nil
}

func (f *fakeTaskInstancePhotoRepo) InstanceExists(context.Context, household.HouseholdID, domain.TaskInstanceID) (bool, error) {
	f.instanceExistsCalls++
	if f.instanceExistsErr != nil {
		return false, f.instanceExistsErr
	}
	return f.instanceExists, nil
}

func (f *fakeTaskInstancePhotoRepo) LatestTakenAt(context.Context, household.HouseholdID, domain.TaskInstanceID, domain.PhotoKind) (time.Time, bool, error) {
	f.latestCalls++
	return f.latestTakenAt, f.latestOK, f.latestErr
}

func (f *fakeTaskInstancePhotoRepo) ListByInstance(context.Context, household.HouseholdID, domain.TaskInstanceID) ([]*domain.TaskInstancePhoto, error) {
	return nil, nil
}

// --- helpers ---

const testFreshnessWindow = time.Hour

func newChoreProofService(t *testing.T, store *fakePhotoStore, exif *fakeChoreProofExif, repo *fakeTaskInstancePhotoRepo) *app.ChoreProofPhotoService {
	t.Helper()
	svc, err := app.NewChoreProofPhotoService(store, exif, repo, 10<<20, testFreshnessWindow)
	if err != nil {
		t.Fatalf("NewChoreProofPhotoService: %v", err)
	}
	return svc
}

// --- tests ---

// TestChoreProofUploadSucceedsWithFreshExif covers AC1: a camera photo with
// fresh EXIF succeeds, records taken_at from EXIF, and — critically — the
// bytes PhotoStore.Put receives are the SCRUBBED bytes (Scrub was actually
// invoked before storage), not the raw upload.
func TestChoreProofUploadSucceedsWithFreshExif(t *testing.T) {
	store := &fakePhotoStore{}
	taken := time.Now().UTC().Add(-5 * time.Minute)
	exif := &fakeChoreProofExif{taken: &taken, orientation: 1}
	repo := newFakeTaskInstancePhotoRepo()
	svc := newChoreProofService(t, store, exif, repo)

	hh := household.NewHouseholdID()
	uploader := household.NewMemberID()
	instance := freshInstanceID(t)
	raw := jpegLikeBytes("camera-photo-bytes")

	photo, err := svc.Upload(context.Background(), hh, uploader, instance, domain.PhotoKindBefore, bytes.NewReader(raw), time.Now().UTC())
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if photo.TakenAt != taken {
		t.Fatalf("TakenAt = %s, want %s", photo.TakenAt, taken)
	}
	if photo.Kind != domain.PhotoKindBefore || photo.HouseholdID != hh || photo.TaskInstanceID != instance {
		t.Fatalf("photo attribution wrong: %+v", photo)
	}
	if photo.UploadedBy == nil || *photo.UploadedBy != uploader {
		t.Fatalf("uploader wrong: %+v", photo)
	}
	if repo.instanceExistsCalls != 1 {
		t.Fatalf("InstanceExists called %d times, want 1 (the preflight)", repo.instanceExistsCalls)
	}
	if exif.scrubCalls != 1 {
		t.Fatalf("Scrub called %d times, want 1", exif.scrubCalls)
	}
	if !bytes.HasPrefix(store.lastPutBytes, []byte(scrubbedMarker)) {
		t.Fatalf("PhotoStore.Put received %q, want the SCRUBBED bytes (prefixed %q) — EXIF must be stripped before storage", store.lastPutBytes, scrubbedMarker)
	}
	if store.lastPutClass != domain.PhotoClassChoreProof {
		t.Fatalf("Put called with class %v, want PhotoClassChoreProof", store.lastPutClass)
	}
	if len(repo.created) != 1 {
		t.Fatalf("created %d photo rows, want 1", len(repo.created))
	}
}

// TestChoreProofUploadPreflightRejectsUnknownInstance covers the
// InstanceExists preflight (NES-119 review, design resolution A): an
// unknown or cross-household task instance is rejected BEFORE anything is
// buffered, scrubbed, or stored — the fake cannot distinguish "unknown" from
// "belongs to another household" (both collapse to InstanceExists
// returning false), but the real repository's household-scoped SQL does
// (see chore_photo_postgres_test.go for the household-scoped gated
// coverage).
func TestChoreProofUploadPreflightRejectsUnknownInstance(t *testing.T) {
	store := &fakePhotoStore{}
	exif := &fakeChoreProofExif{}
	repo := newFakeTaskInstancePhotoRepo()
	repo.instanceExists = false
	svc := newChoreProofService(t, store, exif, repo)

	_, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindBefore, bytes.NewReader(jpegLikeBytes("x")), time.Now())
	if !errors.Is(err, domain.ErrTaskInstanceNotFound) {
		t.Fatalf("Upload error = %v, want ErrTaskInstanceNotFound", err)
	}
	if store.puts != 0 {
		t.Fatal("an unknown-instance upload must not reach PhotoStore.Put")
	}
	if exif.scrubCalls != 0 {
		t.Fatal("an unknown-instance upload must not even attempt EXIF extraction/scrubbing")
	}
	if len(repo.created) != 0 {
		t.Fatal("an unknown-instance upload must not persist a photo")
	}
}

func TestChoreProofUploadPreflightErrorPropagates(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakeTaskInstancePhotoRepo()
	repo.instanceExistsErr = errors.New("db down")
	svc := newChoreProofService(t, store, &fakeChoreProofExif{}, repo)

	_, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindBefore, bytes.NewReader(jpegLikeBytes("x")), time.Now())
	if err == nil || errors.Is(err, domain.ErrTaskInstanceNotFound) {
		t.Fatalf("Upload error = %v, want a wrapped preflight error, not ErrTaskInstanceNotFound", err)
	}
	if store.puts != 0 {
		t.Fatal("a preflight error must not reach PhotoStore.Put")
	}
}

// TestChoreProofUploadRejectsMissingTimestamp covers AC2: an upload with no
// usable EXIF timestamp (e.g. a screenshot, or any EXIF-stripped image) is
// rejected with ErrPhotoMissingTimestamp, and nothing is stored or persisted.
func TestChoreProofUploadRejectsMissingTimestamp(t *testing.T) {
	store := &fakePhotoStore{}
	exif := &fakeChoreProofExif{taken: nil}
	repo := newFakeTaskInstancePhotoRepo()
	svc := newChoreProofService(t, store, exif, repo)

	_, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindBefore, bytes.NewReader(jpegLikeBytes("no-exif")), time.Now())
	if !errors.Is(err, domain.ErrPhotoMissingTimestamp) {
		t.Fatalf("Upload error = %v, want ErrPhotoMissingTimestamp", err)
	}
	if store.puts != 0 || len(repo.created) != 0 {
		t.Fatal("a missing-timestamp upload must not store or persist anything")
	}
}

// TestChoreProofUploadNonJPEGNeverExtractsTimestamp covers the same AC2
// family for a non-JPEG upload: EXIF extraction (and scrubbing) is never
// even attempted — see captureAndScrub's doc — so it fails the same
// ErrPhotoMissingTimestamp check without ever calling the ChoreProofExif
// port.
func TestChoreProofUploadNonJPEGNeverExtractsTimestamp(t *testing.T) {
	store := &fakePhotoStore{}
	taken := time.Now().UTC() // would succeed if (wrongly) consulted
	exif := &fakeChoreProofExif{taken: &taken}
	repo := newFakeTaskInstancePhotoRepo()
	svc := newChoreProofService(t, store, exif, repo)

	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 'r', 'e', 's', 't'}
	_, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindBefore, bytes.NewReader(pngBytes), time.Now())
	if !errors.Is(err, domain.ErrPhotoMissingTimestamp) {
		t.Fatalf("Upload error = %v, want ErrPhotoMissingTimestamp", err)
	}
	if exif.scrubCalls != 0 {
		t.Fatal("Scrub must never be called for a non-JPEG upload")
	}
	if store.puts != 0 {
		t.Fatal("a non-JPEG chore-proof upload must not reach PhotoStore.Put")
	}
}

// TestChoreProofUploadRejectsStalePhoto covers AC3: a photo whose EXIF
// capture time falls outside the freshness window, in EITHER direction
// (too old, or a badly-clocked camera dating it into the future), is
// rejected.
func TestChoreProofUploadRejectsStalePhoto(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		taken  time.Time
		wantOK bool
	}{
		{"just inside the window (past)", now.Add(-59 * time.Minute), true},
		{"just inside the window (future)", now.Add(59 * time.Minute), true},
		{"too old", now.Add(-90 * time.Minute), false},
		{"too far in the future", now.Add(90 * time.Minute), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakePhotoStore{}
			taken := tt.taken
			exif := &fakeChoreProofExif{taken: &taken, orientation: 1}
			repo := newFakeTaskInstancePhotoRepo()
			svc := newChoreProofService(t, store, exif, repo)

			_, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindBefore, bytes.NewReader(jpegLikeBytes("x")), now)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("Upload = %v, want success", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrPhotoStale) {
				t.Fatalf("Upload error = %v, want ErrPhotoStale", err)
			}
			if store.puts != 0 {
				t.Fatal("a stale upload must not reach PhotoStore.Put")
			}
		})
	}
}

// TestChoreProofUploadPropagatesOrderingCheckFromCreate covers AC3's second
// half after the NES-119 atomicity review: the before/after ordering rule
// (domain.ErrAfterPrecedesBefore) is now enforced by
// TaskInstancePhotoRepository.Create itself, atomically, NOT by
// ChoreProofPhotoService — this test proves the service (a) never performs
// its own read-then-decide check (LatestTakenAt is never called from
// Upload) and (b) correctly propagates whatever Create decides, using the
// fake's simulateOrderingCheck to stand in for Create's real atomic
// behavior (covered end-to-end, including the concurrency argument, by
// chore_photo_postgres_test.go's gated tests).
func TestChoreProofUploadPropagatesOrderingCheckFromCreate(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	beforeTaken := now.Add(-30 * time.Minute)

	t.Run("Create rejecting an after propagates ErrAfterPrecedesBefore", func(t *testing.T) {
		store := &fakePhotoStore{}
		afterTaken := beforeTaken.Add(-1 * time.Minute) // earlier than "before"
		exif := &fakeChoreProofExif{taken: &afterTaken, orientation: 1}
		repo := newFakeTaskInstancePhotoRepo()
		repo.latestTakenAt, repo.latestOK, repo.simulateOrderingCheck = beforeTaken, true, true
		svc := newChoreProofService(t, store, exif, repo)

		_, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindAfter, bytes.NewReader(jpegLikeBytes("x")), now)
		if !errors.Is(err, domain.ErrAfterPrecedesBefore) {
			t.Fatalf("Upload error = %v, want ErrAfterPrecedesBefore", err)
		}
	})

	t.Run("Create accepting a valid after succeeds", func(t *testing.T) {
		store := &fakePhotoStore{}
		afterTaken := beforeTaken.Add(1 * time.Minute)
		exif := &fakeChoreProofExif{taken: &afterTaken, orientation: 1}
		repo := newFakeTaskInstancePhotoRepo()
		repo.latestTakenAt, repo.latestOK, repo.simulateOrderingCheck = beforeTaken, true, true
		svc := newChoreProofService(t, store, exif, repo)

		if _, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindAfter, bytes.NewReader(jpegLikeBytes("x")), now); err != nil {
			t.Fatalf("Upload: %v", err)
		}
	})

	t.Run("Upload never calls LatestTakenAt itself, for either kind", func(t *testing.T) {
		for _, kind := range []domain.PhotoKind{domain.PhotoKindBefore, domain.PhotoKindAfter} {
			store := &fakePhotoStore{}
			taken := now.Add(-5 * time.Minute)
			exif := &fakeChoreProofExif{taken: &taken, orientation: 1}
			repo := newFakeTaskInstancePhotoRepo()
			svc := newChoreProofService(t, store, exif, repo)

			if _, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), kind, bytes.NewReader(jpegLikeBytes("x")), now); err != nil {
				t.Fatalf("Upload(%v): %v", kind, err)
			}
			if repo.latestCalls != 0 {
				t.Fatalf("Upload(%v) called LatestTakenAt %d times, want 0 — that check now lives entirely in Create", kind, repo.latestCalls)
			}
		}
	})
}

func TestChoreProofUploadRejectsInvalidKind(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakeTaskInstancePhotoRepo()
	svc := newChoreProofService(t, store, &fakeChoreProofExif{}, repo)

	_, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindUnspecified, bytes.NewReader(jpegLikeBytes("x")), time.Now())
	if !errors.Is(err, domain.ErrInvalidTaskInstancePhoto) {
		t.Fatalf("Upload error = %v, want ErrInvalidTaskInstancePhoto", err)
	}
	if store.puts != 0 {
		t.Fatal("an invalid-kind upload must not reach PhotoStore.Put")
	}
}

func TestChoreProofUploadRejectsOversizeUpload(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakeTaskInstancePhotoRepo()
	svc, err := app.NewChoreProofPhotoService(store, &fakeChoreProofExif{}, repo, 8, testFreshnessWindow)
	if err != nil {
		t.Fatalf("NewChoreProofPhotoService: %v", err)
	}

	oversized := bytes.Repeat([]byte("x"), 9)
	_, err = svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindBefore, bytes.NewReader(oversized), time.Now())
	if !errors.Is(err, domain.ErrPhotoTooLarge) {
		t.Fatalf("Upload error = %v, want ErrPhotoTooLarge", err)
	}
	if store.puts != 0 {
		t.Fatal("an oversize upload must not reach PhotoStore.Put")
	}
}

func TestChoreProofUploadStoreErrorPropagates(t *testing.T) {
	store := &fakePhotoStore{putErr: domain.ErrUnsupportedMediaType}
	taken := time.Now().UTC()
	exif := &fakeChoreProofExif{taken: &taken, orientation: 1}
	repo := newFakeTaskInstancePhotoRepo()
	svc := newChoreProofService(t, store, exif, repo)

	_, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindBefore, bytes.NewReader(jpegLikeBytes("x")), time.Now())
	if !errors.Is(err, domain.ErrUnsupportedMediaType) {
		t.Fatalf("Upload error = %v, want ErrUnsupportedMediaType", err)
	}
	if len(repo.created) != 0 {
		t.Fatal("a store error must not persist a photo")
	}
}

func TestChoreProofUploadScrubErrorPropagates(t *testing.T) {
	store := &fakePhotoStore{}
	taken := time.Now().UTC()
	scrubErr := errors.New("scrub boom")
	exif := &fakeChoreProofExif{taken: &taken, orientation: 6, scrubErr: scrubErr}
	repo := newFakeTaskInstancePhotoRepo()
	svc := newChoreProofService(t, store, exif, repo)

	_, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindBefore, bytes.NewReader(jpegLikeBytes("x")), time.Now())
	if !errors.Is(err, scrubErr) {
		t.Fatalf("Upload error = %v, want it to wrap the scrub error", err)
	}
	if store.puts != 0 {
		t.Fatal("a scrub error must not reach PhotoStore.Put")
	}
}

func TestNewChoreProofPhotoServiceValidatesDependencies(t *testing.T) {
	store := &fakePhotoStore{}
	exif := &fakeChoreProofExif{}
	repo := newFakeTaskInstancePhotoRepo()

	if _, err := app.NewChoreProofPhotoService(nil, exif, repo, 10, testFreshnessWindow); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := app.NewChoreProofPhotoService(store, nil, repo, 10, testFreshnessWindow); err == nil {
		t.Fatal("nil exif accepted")
	}
	if _, err := app.NewChoreProofPhotoService(store, exif, nil, 10, testFreshnessWindow); err == nil {
		t.Fatal("nil repo accepted")
	}
	if _, err := app.NewChoreProofPhotoService(store, exif, repo, 0, testFreshnessWindow); err == nil {
		t.Fatal("non-positive maxUploadBytes accepted")
	}
	if _, err := app.NewChoreProofPhotoService(store, exif, repo, 10, 0); err == nil {
		t.Fatal("non-positive freshnessWindow accepted")
	}
}

// TestNewChoreProofPhotoServiceRejectsOverflowingMaxUploadBytes covers the
// NES-119 review's overflow guard: readBounded computes maxBytes+1, which
// wraps negative only when maxBytes == math.MaxInt64 (the one value whose
// +1 does not fit in an int64) — rejected at construction time instead of
// corrupting the size check later; math.MaxInt64-1 is the largest value
// that still leaves exactly enough room and must be accepted.
func TestNewChoreProofPhotoServiceRejectsOverflowingMaxUploadBytes(t *testing.T) {
	store := &fakePhotoStore{}
	exif := &fakeChoreProofExif{}
	repo := newFakeTaskInstancePhotoRepo()

	if _, err := app.NewChoreProofPhotoService(store, exif, repo, math.MaxInt64, testFreshnessWindow); err == nil {
		t.Fatal("maxUploadBytes = math.MaxInt64 accepted, want rejected (would overflow maxBytes+1)")
	}
	if _, err := app.NewChoreProofPhotoService(store, exif, repo, math.MaxInt64-1, testFreshnessWindow); err != nil {
		t.Fatalf("maxUploadBytes = math.MaxInt64-1 rejected: %v, want accepted (leaves exactly enough room for +1)", err)
	}
}

// freshInstanceID returns an arbitrary, syntactically valid TaskInstanceID —
// the fakes never look up an instance by row, so any value works.
func freshInstanceID(t *testing.T) domain.TaskInstanceID {
	t.Helper()
	id, err := domain.ParseTaskInstanceID("11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("ParseTaskInstanceID: %v", err)
	}
	return id
}
