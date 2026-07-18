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
// ChoreProofPhotoService.captureMetadata/scrubIfJPEG treat it as a JPEG
// candidate and call the ChoreProofExif fake — the fake doesn't need
// genuinely decodable image data since fakePhotoStore.Put (reused from
// service_test.go) only hashes bytes, it never decodes them.
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
	// createRejects, when true, makes Create unconditionally return
	// domain.ErrAfterPrecedesBefore for an "after" photo, INDEPENDENT of
	// latestTakenAt/latestOK/simulateOrderingCheck — used to simulate
	// Create's atomic check catching a violation that
	// Upload.preflightOrderingCheck's necessarily non-atomic read did NOT
	// see (e.g. a concurrent "before" that committed in the gap between the
	// preflight and Create's own transaction). The real version of that gap
	// being closed is proven by the gated concurrent Postgres test
	// (chore_photo_postgres_test.go); this flag exists only to prove Upload
	// correctly PROPAGATES whatever Create decides rather than trusting its
	// own preflight as authoritative.
	createRejects bool
	// getPhoto/getErr (NES-120) configure Get's result for OpenBytes tests.
	// The zero value (both nil) reports ErrTaskInstancePhotoNotFound,
	// matching a genuinely unknown id.
	getPhoto *domain.TaskInstancePhoto
	getErr   error
	// existsOverride mirrors fakePhotoRepo's identical field (see its doc):
	// lets a test make ExistsByStorageRef diverge from a prior
	// ListAllStorageRefs snapshot, simulating a row committed after the
	// reaper's bulk snapshot but before its per-object recheck.
	existsOverride map[domain.StorageRef]bool
	existsCalls    []domain.StorageRef

	// backend is the StorageBackend Create stamps onto every row it writes
	// (mirroring the real TaskInstancePhotoRepository.Create — see its
	// doc), defaulting to domain.StorageBackendLocal via
	// newFakeTaskInstancePhotoRepo.
	backend domain.StorageBackend
}

func newFakeTaskInstancePhotoRepo() *fakeTaskInstancePhotoRepo {
	return &fakeTaskInstancePhotoRepo{instanceExists: true, backend: domain.StorageBackendLocal}
}

func (f *fakeTaskInstancePhotoRepo) Create(_ context.Context, p *domain.TaskInstancePhoto) error {
	if f.createRejects && p.Kind == domain.PhotoKindAfter {
		return domain.ErrAfterPrecedesBefore
	}
	if f.simulateOrderingCheck && p.Kind == domain.PhotoKindAfter && f.latestOK && domain.AfterPrecedesBefore(p.TakenAt, f.latestTakenAt) {
		return domain.ErrAfterPrecedesBefore
	}
	if f.createErr != nil {
		return f.createErr
	}
	p.UploadedAt = time.Now().UTC()
	p.StorageBackend = f.backend
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

// ListByInstances is unused by this file's Upload/OpenBytes-focused tests;
// implemented only to satisfy the interface (NES-120 added it for the
// /tasks list builder's batched photo lookup).
func (f *fakeTaskInstancePhotoRepo) ListByInstances(context.Context, household.HouseholdID, []domain.TaskInstanceID) ([]*domain.TaskInstancePhoto, error) {
	return nil, nil
}

// Get is deliberately ID-only (NES-120), mirroring PhotoRepository.Get:
// ownership is enforced by the caller (ChoreProofPhotoService.OpenBytes's
// ownedPhoto), not by this fake or the real repository.
func (f *fakeTaskInstancePhotoRepo) Get(context.Context, domain.TaskInstancePhotoID) (*domain.TaskInstancePhoto, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getPhoto != nil {
		return f.getPhoto, nil
	}
	return nil, domain.ErrTaskInstancePhotoNotFound
}

// ListAllStorageRefs reflects every photo Create has recorded, filtered to
// backend (mirroring the real TaskInstancePhotoRepository's storage_backend
// filter, NES-132) — unused by this file's Upload/OpenBytes-focused tests,
// but genuinely functional (rather than a stub) so reaper_service_test.go
// can reuse this same fake.
func (f *fakeTaskInstancePhotoRepo) ListAllStorageRefs(_ context.Context, backend domain.StorageBackend) ([]domain.StorageRef, error) {
	refs := make([]domain.StorageRef, 0, len(f.created))
	for _, p := range f.created {
		if p.StorageBackend == backend {
			refs = append(refs, p.StorageRef)
		}
	}
	return refs, nil
}

// ExistsByStorageRef mirrors fakePhotoRepo's identical method (see its
// doc): existsOverride first, otherwise a live lookup against f.created
// filtered to backend.
func (f *fakeTaskInstancePhotoRepo) ExistsByStorageRef(_ context.Context, ref domain.StorageRef, backend domain.StorageBackend) (bool, error) {
	f.existsCalls = append(f.existsCalls, ref)
	if v, ok := f.existsOverride[ref]; ok {
		return v, nil
	}
	for _, p := range f.created {
		if p.StorageRef == ref && p.StorageBackend == backend {
			return true, nil
		}
	}
	return false, nil
}

// DeleteUploadedBefore removes every created row whose UploadedAt precedes
// cutoff and reports how many were removed — genuinely functional (not a
// stub) so reaper_service_test.go can reuse this same fake to exercise the
// retention pass.
func (f *fakeTaskInstancePhotoRepo) DeleteUploadedBefore(_ context.Context, cutoff time.Time) (int64, error) {
	kept := f.created[:0:0]
	var n int64
	for _, p := range f.created {
		if p.UploadedAt.Before(cutoff) {
			n++
			continue
		}
		kept = append(kept, p)
	}
	f.created = kept
	return n, nil
}

// --- helpers ---

const testFreshnessWindow = time.Hour

func newChoreProofService(t *testing.T, store *fakePhotoStore, exif *fakeChoreProofExif, repo *fakeTaskInstancePhotoRepo) *app.ChoreProofPhotoService {
	t.Helper()
	resolver := newFakeStoreResolver(domain.StorageBackendLocal, store)
	svc, err := app.NewChoreProofPhotoService(resolver, domain.StorageBackendLocal, exif, repo, 10<<20, testFreshnessWindow)
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
// rejected with ErrPhotoMissingTimestamp, and nothing is stored or
// persisted. It also covers the NES-119 review's reordering fix: Scrub must
// never be called for an upload that was always going to be rejected on
// timestamp grounds alone — captureMetadata's cheap tag read is enough to
// decide that, so scrubIfJPEG's potentially expensive decode/re-encode must
// never run.
func TestChoreProofUploadRejectsMissingTimestamp(t *testing.T) {
	store := &fakePhotoStore{}
	// orientation != 1/0 would trigger the expensive re-encode path in
	// Scrub if it were ever (wrongly) called — makes the zero-scrub-calls
	// assertion below meaningful rather than vacuous.
	exif := &fakeChoreProofExif{taken: nil, orientation: 6}
	repo := newFakeTaskInstancePhotoRepo()
	svc := newChoreProofService(t, store, exif, repo)

	_, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindBefore, bytes.NewReader(jpegLikeBytes("no-exif")), time.Now())
	if !errors.Is(err, domain.ErrPhotoMissingTimestamp) {
		t.Fatalf("Upload error = %v, want ErrPhotoMissingTimestamp", err)
	}
	if store.puts != 0 || len(repo.created) != 0 {
		t.Fatal("a missing-timestamp upload must not store or persist anything")
	}
	if exif.scrubCalls != 0 {
		t.Fatal("a missing-timestamp upload must never reach Scrub — cheap metadata alone is enough to reject it")
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
// rejected. Also covers the NES-119 review's reordering fix (see
// TestChoreProofUploadRejectsMissingTimestamp's doc): a rejected upload
// must never reach Scrub.
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
			// orientation != 1/0 makes a would-be Scrub call expensive in
			// spirit, same reasoning as the missing-timestamp test.
			exif := &fakeChoreProofExif{taken: &taken, orientation: 6}
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
			if exif.scrubCalls != 0 {
				t.Fatal("a stale upload must never reach Scrub — the freshness check alone is enough to reject it")
			}
		})
	}
}

// TestChoreProofUploadPropagatesOrderingCheckFromCreate covers AC3's second
// half after the NES-119 atomicity review: the before/after ordering rule
// (domain.ErrAfterPrecedesBefore) is AUTHORITATIVELY enforced by
// TaskInstancePhotoRepository.Create itself, atomically — this test proves
// Upload correctly propagates whatever Create decides, using the fake's
// simulateOrderingCheck to stand in for Create's real atomic behavior
// (covered end-to-end, including the concurrency argument, by
// chore_photo_postgres_test.go's gated tests). See
// TestChoreProofUploadOrderingPreflight for the separate, best-effort
// preflight Upload itself now also runs (a later review round added it to
// avoid paying for Scrub on an obviously-doomed "after" upload).
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

	t.Run("Create still enforces the rule even when the preflight found nothing to object to", func(t *testing.T) {
		// repo.latestOK stays false, so preflightOrderingCheck's own read
		// sees "no before yet" and passes — but createRejects makes Create
		// itself still reject, simulating a concurrent write the preflight's
		// necessarily non-atomic read could not have seen. Upload must
		// propagate Create's decision regardless of what its own preflight
		// concluded, and — since the preflight passed — Put IS reached
		// (proving this really is Create's rejection, not the preflight's).
		store := &fakePhotoStore{}
		afterTaken := now.Add(-5 * time.Minute)
		exif := &fakeChoreProofExif{taken: &afterTaken, orientation: 1}
		repo := newFakeTaskInstancePhotoRepo()
		repo.createRejects = true
		svc := newChoreProofService(t, store, exif, repo)

		_, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindAfter, bytes.NewReader(jpegLikeBytes("x")), now)
		if !errors.Is(err, domain.ErrAfterPrecedesBefore) {
			t.Fatalf("Upload error = %v, want ErrAfterPrecedesBefore (from Create, not the preflight)", err)
		}
		if store.puts != 1 {
			t.Fatalf("PhotoStore.Put called %d times, want 1 — the preflight passed, so Put must have been reached before Create's own rejection", store.puts)
		}
	})

	t.Run("before uploads never consult LatestTakenAt", func(t *testing.T) {
		store := &fakePhotoStore{}
		taken := now.Add(-5 * time.Minute)
		exif := &fakeChoreProofExif{taken: &taken, orientation: 1}
		repo := newFakeTaskInstancePhotoRepo()
		svc := newChoreProofService(t, store, exif, repo)

		if _, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindBefore, bytes.NewReader(jpegLikeBytes("x")), now); err != nil {
			t.Fatalf("Upload: %v", err)
		}
		if repo.latestCalls != 0 {
			t.Fatal("a \"before\" upload must never check LatestTakenAt — only \"after\" uploads run the ordering preflight")
		}
	})
}

// TestChoreProofUploadOrderingPreflight covers the NES-119 review's
// reordering fix: an "after" upload that is ALREADY provably invalid based
// on a plain (unlocked) LatestTakenAt read is rejected by Upload's own
// preflight BEFORE Scrub or PhotoStore.Put ever run — a real performance
// win when the upload needs the expensive re-encode path (orientation != 1)
// — while a valid "after" upload still calls LatestTakenAt exactly once
// (the preflight) and proceeds normally.
func TestChoreProofUploadOrderingPreflight(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	beforeTaken := now.Add(-30 * time.Minute)

	t.Run("preflight rejects before Scrub or Put run", func(t *testing.T) {
		store := &fakePhotoStore{}
		afterTaken := beforeTaken.Add(-1 * time.Minute) // earlier than "before"
		// orientation != 1/0: if the preflight did NOT short-circuit, this
		// would trigger the (simulated) expensive re-encode path.
		exif := &fakeChoreProofExif{taken: &afterTaken, orientation: 6}
		repo := newFakeTaskInstancePhotoRepo()
		repo.latestTakenAt, repo.latestOK = beforeTaken, true
		svc := newChoreProofService(t, store, exif, repo)

		_, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindAfter, bytes.NewReader(jpegLikeBytes("x")), now)
		if !errors.Is(err, domain.ErrAfterPrecedesBefore) {
			t.Fatalf("Upload error = %v, want ErrAfterPrecedesBefore", err)
		}
		if repo.latestCalls != 1 {
			t.Fatalf("LatestTakenAt called %d times, want 1 (the preflight)", repo.latestCalls)
		}
		if exif.scrubCalls != 0 {
			t.Fatal("a preflight-rejected after must never reach Scrub")
		}
		if store.puts != 0 {
			t.Fatal("a preflight-rejected after must never reach PhotoStore.Put")
		}
		if len(repo.created) != 0 {
			t.Fatal("a preflight-rejected after must never be persisted")
		}
	})

	t.Run("a valid after still calls the preflight once and succeeds", func(t *testing.T) {
		store := &fakePhotoStore{}
		afterTaken := beforeTaken.Add(1 * time.Minute)
		exif := &fakeChoreProofExif{taken: &afterTaken, orientation: 1}
		repo := newFakeTaskInstancePhotoRepo()
		repo.latestTakenAt, repo.latestOK = beforeTaken, true
		svc := newChoreProofService(t, store, exif, repo)

		if _, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindAfter, bytes.NewReader(jpegLikeBytes("x")), now); err != nil {
			t.Fatalf("Upload: %v", err)
		}
		if repo.latestCalls != 1 {
			t.Fatalf("LatestTakenAt called %d times, want 1", repo.latestCalls)
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
	svc, err := app.NewChoreProofPhotoService(newFakeStoreResolver(domain.StorageBackendLocal, store), domain.StorageBackendLocal, &fakeChoreProofExif{}, repo, 8, testFreshnessWindow)
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

// ---------------------------------------------------------------------------
// OpenBytes (NES-120)
// ---------------------------------------------------------------------------

// TestChoreProofPhotoService_OpenBytes_Success verifies the happy path:
// OpenBytes streams the stored bytes and returns the photo's own recorded
// ContentType.
func TestChoreProofPhotoService_OpenBytes_Success(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakeTaskInstancePhotoRepo()
	hh := household.NewHouseholdID()
	photo := &domain.TaskInstancePhoto{
		ID:             domain.NewTaskInstancePhotoID(),
		HouseholdID:    hh,
		StorageRef:     domain.StorageRef("hh/aa/abc.jpg"),
		ContentType:    domain.ContentTypeJPEG,
		StorageBackend: domain.StorageBackendLocal,
	}
	repo.getPhoto = photo
	svc := newChoreProofService(t, store, &fakeChoreProofExif{}, repo)

	rc, contentType, err := svc.OpenBytes(context.Background(), hh, photo.ID)
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if contentType != domain.ContentTypeJPEG {
		t.Errorf("contentType = %q, want %q", contentType, domain.ContentTypeJPEG)
	}
}

// TestChoreProofPhotoService_OpenBytes_CrossHouseholdRejected verifies
// NES-120's moved ownership check: a photo belonging to a DIFFERENT
// household than the caller's is reported as not found — the repository's
// Get is ID-only (mirrors PhotoRepository.Get), so this is the service's
// own enforcement (ownedPhoto), not the repository's.
func TestChoreProofPhotoService_OpenBytes_CrossHouseholdRejected(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakeTaskInstancePhotoRepo()
	photo := &domain.TaskInstancePhoto{
		ID:          domain.NewTaskInstancePhotoID(),
		HouseholdID: household.NewHouseholdID(),
		StorageRef:  domain.StorageRef("hh/aa/abc.jpg"),
		ContentType: domain.ContentTypeJPEG,
	}
	repo.getPhoto = photo
	svc := newChoreProofService(t, store, &fakeChoreProofExif{}, repo)

	// A DIFFERENT household than photo.HouseholdID requests the same id.
	_, _, err := svc.OpenBytes(context.Background(), household.NewHouseholdID(), photo.ID)
	if !errors.Is(err, domain.ErrTaskInstancePhotoNotFound) {
		t.Errorf("OpenBytes(cross-household) = %v, want ErrTaskInstancePhotoNotFound", err)
	}
	// OpenBytes' only PhotoStore call is Open (never Put — this is a read
	// path); ownedPhoto must reject the mismatch BEFORE store.Open is ever
	// reached, so store.puts alone (always 0 on a read path regardless of
	// whether ownership was checked) would not actually prove the
	// short-circuit — openCalls is the call that matters here.
	if store.openCalls != 0 {
		t.Errorf("openCalls = %d, want 0 (cross-household rejection must never reach PhotoStore.Open)", store.openCalls)
	}
	if store.puts != 0 {
		t.Error("cross-household rejection must never reach PhotoStore")
	}
}

// TestChoreProofPhotoService_OpenBytes_UnknownIDPropagates verifies that an
// unknown id (the repository's own zero-value fallback) surfaces
// ErrTaskInstancePhotoNotFound unchanged.
func TestChoreProofPhotoService_OpenBytes_UnknownIDPropagates(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakeTaskInstancePhotoRepo() // getPhoto unset
	svc := newChoreProofService(t, store, &fakeChoreProofExif{}, repo)

	_, _, err := svc.OpenBytes(context.Background(), household.NewHouseholdID(), domain.NewTaskInstancePhotoID())
	if !errors.Is(err, domain.ErrTaskInstancePhotoNotFound) {
		t.Errorf("OpenBytes(unknown id) = %v, want ErrTaskInstancePhotoNotFound", err)
	}
}

// TestChoreProofPhotoService_RawServe_StreamsWhenBackendLacksDirectURL
// mirrors TestPhotoServiceRawServeStreamsWhenBackendLacksDirectURL, one
// table over (NES-132): a local-like backend (SupportsDirectURL false)
// yields a Body to stream, never a RedirectURL.
func TestChoreProofPhotoService_RawServe_StreamsWhenBackendLacksDirectURL(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakeTaskInstancePhotoRepo()
	hh := household.NewHouseholdID()
	photo := &domain.TaskInstancePhoto{
		ID: domain.NewTaskInstancePhotoID(), HouseholdID: hh,
		StorageRef: domain.StorageRef("hh/aa/abc.jpg"), ContentType: domain.ContentTypeJPEG,
		StorageBackend: domain.StorageBackendLocal,
	}
	repo.getPhoto = photo
	svc := newChoreProofService(t, store, &fakeChoreProofExif{}, repo)

	result, err := svc.RawServe(context.Background(), hh, photo.ID)
	if err != nil {
		t.Fatalf("RawServe: %v", err)
	}
	if result.RedirectURL != "" {
		t.Fatalf("RedirectURL = %q, want empty for a local-like backend", result.RedirectURL)
	}
	if result.Body == nil {
		t.Fatal("Body is nil, want a stream for a local-like backend")
	}
	_ = result.Body.Close()
	if result.ContentType != domain.ContentTypeJPEG {
		t.Errorf("ContentType = %q, want %q", result.ContentType, domain.ContentTypeJPEG)
	}
}

// TestChoreProofPhotoService_RawServe_RedirectsWhenBackendSupportsDirectURL
// mirrors TestPhotoServiceRawServeRedirectsWhenBackendSupportsDirectURL: an
// S3-like backend yields a RedirectURL, never opening/streaming a body.
func TestChoreProofPhotoService_RawServe_RedirectsWhenBackendSupportsDirectURL(t *testing.T) {
	store := &fakePhotoStore{directURL: true}
	repo := newFakeTaskInstancePhotoRepo()
	hh := household.NewHouseholdID()
	photo := &domain.TaskInstancePhoto{
		ID: domain.NewTaskInstancePhotoID(), HouseholdID: hh,
		StorageRef: domain.StorageRef("households/hh/chore-photos/aa/abc.jpg"), ContentType: domain.ContentTypeJPEG,
		StorageBackend: domain.StorageBackendLocal,
	}
	repo.getPhoto = photo
	svc := newChoreProofService(t, store, &fakeChoreProofExif{}, repo)

	result, err := svc.RawServe(context.Background(), hh, photo.ID)
	if err != nil {
		t.Fatalf("RawServe: %v", err)
	}
	if result.RedirectURL != "households/hh/chore-photos/aa/abc.jpg" {
		t.Fatalf("RedirectURL = %q, want the fake store's URL() result", result.RedirectURL)
	}
	if result.Body != nil {
		t.Fatal("Body is non-nil, want none for an S3-like backend redirect")
	}
	if store.openCalls != 0 {
		t.Fatalf("Open was called %d times, want 0 (redirect must never open/stream)", store.openCalls)
	}
}

// TestChoreProofPhotoService_MixedStateReadsResolveByRowBackend mirrors
// TestPhotoServiceMixedStateReadsResolveByRowBackend (service_test.go), one
// table over: with BOTH a local and an s3 store registered, a row stamped
// 'local' resolves to the local store and a row stamped 's3' resolves to
// the s3 store, in the SAME service instance.
func TestChoreProofPhotoService_MixedStateReadsResolveByRowBackend(t *testing.T) {
	localStore := &fakePhotoStore{}
	s3Store := &fakePhotoStore{directURL: true}
	resolver := newFakeStoreResolver(domain.StorageBackendLocal, localStore).withStore(domain.StorageBackendS3, s3Store)
	repo := newFakeTaskInstancePhotoRepo()
	hh := household.NewHouseholdID()

	localPhoto := &domain.TaskInstancePhoto{
		ID: domain.NewTaskInstancePhotoID(), HouseholdID: hh,
		StorageRef: "households/hh/chore-photos/aa/local.jpg", ContentType: domain.ContentTypeJPEG,
		StorageBackend: domain.StorageBackendLocal,
	}
	s3Photo := &domain.TaskInstancePhoto{
		ID: domain.NewTaskInstancePhotoID(), HouseholdID: hh,
		StorageRef: "households/hh/chore-photos/bb/s3.jpg", ContentType: domain.ContentTypeJPEG,
		StorageBackend: domain.StorageBackendS3,
	}

	svc, err := app.NewChoreProofPhotoService(resolver, domain.StorageBackendS3, &fakeChoreProofExif{}, repo, 10<<20, testFreshnessWindow)
	if err != nil {
		t.Fatalf("NewChoreProofPhotoService: %v", err)
	}

	repo.getPhoto = localPhoto
	localResult, err := svc.RawServe(context.Background(), hh, localPhoto.ID)
	if err != nil {
		t.Fatalf("RawServe(local row): %v", err)
	}
	if localResult.Body == nil || localResult.RedirectURL != "" {
		t.Fatalf("RawServe(local row) = %+v, want a streamed Body", localResult)
	}

	repo.getPhoto = s3Photo
	s3Result, err := svc.RawServe(context.Background(), hh, s3Photo.ID)
	if err != nil {
		t.Fatalf("RawServe(s3 row): %v", err)
	}
	if s3Result.RedirectURL != "households/hh/chore-photos/bb/s3.jpg" || s3Result.Body != nil {
		t.Fatalf("RawServe(s3 row) = %+v, want a RedirectURL", s3Result)
	}
	if localStore.openCalls != 1 || s3Store.urlCalls != 1 {
		t.Fatalf("expected exactly one local Open and one s3 URL call, got local.openCalls=%d s3.urlCalls=%d", localStore.openCalls, s3Store.urlCalls)
	}
}

// TestChoreProofPhotoService_RawServeReturnsErrStoreNotConfiguredForMissingBackend
// mirrors TestPhotoServiceRawServeReturnsErrStoreNotConfiguredForMissingBackend:
// a row stamped with a backend this deployment never constructed a store
// for must fail with a wrapped domain.ErrStoreNotConfigured.
func TestChoreProofPhotoService_RawServeReturnsErrStoreNotConfiguredForMissingBackend(t *testing.T) {
	localStore := &fakePhotoStore{}
	resolver := newFakeStoreResolver(domain.StorageBackendLocal, localStore)
	repo := newFakeTaskInstancePhotoRepo()
	hh := household.NewHouseholdID()

	photo := &domain.TaskInstancePhoto{
		ID: domain.NewTaskInstancePhotoID(), HouseholdID: hh,
		StorageRef: "households/hh/chore-photos/aa/s3-only.jpg", ContentType: domain.ContentTypeJPEG,
		StorageBackend: domain.StorageBackendS3,
	}
	repo.getPhoto = photo
	svc, err := app.NewChoreProofPhotoService(resolver, domain.StorageBackendLocal, &fakeChoreProofExif{}, repo, 10<<20, testFreshnessWindow)
	if err != nil {
		t.Fatalf("NewChoreProofPhotoService: %v", err)
	}

	if _, err := svc.RawServe(context.Background(), hh, photo.ID); !errors.Is(err, domain.ErrStoreNotConfigured) {
		t.Fatalf("RawServe(s3-stamped row, no s3 store configured) = %v, want ErrStoreNotConfigured", err)
	}
}

// TestChoreProofPhotoService_UploadAlwaysWritesToConfiguredBackend mirrors
// TestPhotoServiceUploadAlwaysWritesToConfiguredBackend: with both stores
// registered, Upload must write only to writeBackend, regardless of what
// other backends the resolver knows about.
func TestChoreProofPhotoService_UploadAlwaysWritesToConfiguredBackend(t *testing.T) {
	localStore := &fakePhotoStore{}
	s3Store := &fakePhotoStore{}
	resolver := newFakeStoreResolver(domain.StorageBackendLocal, localStore).withStore(domain.StorageBackendS3, s3Store)
	repo := newFakeTaskInstancePhotoRepo()
	repo.backend = domain.StorageBackendS3 // mirrors the real repo also being configured for s3
	taken := time.Now().UTC().Add(-5 * time.Minute)
	exif := &fakeChoreProofExif{taken: &taken, orientation: 1}

	svc, err := app.NewChoreProofPhotoService(resolver, domain.StorageBackendS3, exif, repo, 10<<20, testFreshnessWindow)
	if err != nil {
		t.Fatalf("NewChoreProofPhotoService: %v", err)
	}

	photo, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), freshInstanceID(t), domain.PhotoKindBefore, bytes.NewReader(jpegLikeBytes("x")), time.Now().UTC())
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if s3Store.puts != 1 {
		t.Fatalf("s3 store Put calls = %d, want 1", s3Store.puts)
	}
	if localStore.puts != 0 {
		t.Fatal("Upload must never write to a backend other than writeBackend")
	}
	if photo.StorageBackend != domain.StorageBackendS3 {
		t.Fatalf("created photo StorageBackend = %q, want %q", photo.StorageBackend, domain.StorageBackendS3)
	}
}

// TestChoreProofPhotoService_RawServe_CrossHouseholdRejected mirrors
// TestChoreProofPhotoService_OpenBytes_CrossHouseholdRejected: ownership is
// enforced BEFORE the store is ever consulted, regardless of backend.
func TestChoreProofPhotoService_RawServe_CrossHouseholdRejected(t *testing.T) {
	store := &fakePhotoStore{directURL: true}
	repo := newFakeTaskInstancePhotoRepo()
	photo := &domain.TaskInstancePhoto{
		ID: domain.NewTaskInstancePhotoID(), HouseholdID: household.NewHouseholdID(),
		StorageRef: domain.StorageRef("hh/aa/abc.jpg"), ContentType: domain.ContentTypeJPEG,
	}
	repo.getPhoto = photo
	svc := newChoreProofService(t, store, &fakeChoreProofExif{}, repo)

	_, err := svc.RawServe(context.Background(), household.NewHouseholdID(), photo.ID)
	if !errors.Is(err, domain.ErrTaskInstancePhotoNotFound) {
		t.Errorf("cross-household RawServe = %v, want ErrTaskInstancePhotoNotFound", err)
	}
	if store.openCalls != 0 {
		t.Error("cross-household RawServe must never touch the store")
	}
	// directURL is true on this fake specifically so a bug that checked
	// ownership AFTER branching on SupportsDirectURL would still be caught:
	// URL must never be called either.
	if store.urlCalls != 0 {
		t.Error("cross-household RawServe must never call URL")
	}
}

func TestNewChoreProofPhotoServiceValidatesDependencies(t *testing.T) {
	store := &fakePhotoStore{}
	resolver := newFakeStoreResolver(domain.StorageBackendLocal, store)
	exif := &fakeChoreProofExif{}
	repo := newFakeTaskInstancePhotoRepo()

	if _, err := app.NewChoreProofPhotoService(nil, domain.StorageBackendLocal, exif, repo, 10, testFreshnessWindow); err == nil {
		t.Fatal("nil resolver accepted")
	}
	if _, err := app.NewChoreProofPhotoService(resolver, domain.StorageBackend("azure-blob"), exif, repo, 10, testFreshnessWindow); err == nil {
		t.Fatal("invalid writeBackend accepted")
	}
	if _, err := app.NewChoreProofPhotoService(resolver, domain.StorageBackendLocal, nil, repo, 10, testFreshnessWindow); err == nil {
		t.Fatal("nil exif accepted")
	}
	if _, err := app.NewChoreProofPhotoService(resolver, domain.StorageBackendLocal, exif, nil, 10, testFreshnessWindow); err == nil {
		t.Fatal("nil repo accepted")
	}
	if _, err := app.NewChoreProofPhotoService(resolver, domain.StorageBackendLocal, exif, repo, 0, testFreshnessWindow); err == nil {
		t.Fatal("non-positive maxUploadBytes accepted")
	}
	if _, err := app.NewChoreProofPhotoService(resolver, domain.StorageBackendLocal, exif, repo, 10, 0); err == nil {
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
	resolver := newFakeStoreResolver(domain.StorageBackendLocal, store)
	exif := &fakeChoreProofExif{}
	repo := newFakeTaskInstancePhotoRepo()

	if _, err := app.NewChoreProofPhotoService(resolver, domain.StorageBackendLocal, exif, repo, math.MaxInt64, testFreshnessWindow); err == nil {
		t.Fatal("maxUploadBytes = math.MaxInt64 accepted, want rejected (would overflow maxBytes+1)")
	}
	if _, err := app.NewChoreProofPhotoService(resolver, domain.StorageBackendLocal, exif, repo, math.MaxInt64-1, testFreshnessWindow); err != nil {
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
