package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// newTestMigrator wires a photoMigrator over fakes, recording every
// migrateProgress event it reports for assertions.
func newTestMigrator(t *testing.T, local *fakeLocalStore, target *fakeTargetStore, photos *fakePhotoRepo, choreProofPhotos *fakeTaskInstancePhotoRepo) (*photoMigrator, *[]migrateProgress) {
	t.Helper()
	target.classOfKeyFn = mediaadapter.ClassOfKey
	events := make([]migrateProgress, 0)
	m, err := newPhotoMigrator(local, target, mediadomain.StorageBackendS3, photos, choreProofPhotos, 10<<20, func(p migrateProgress) {
		events = append(events, p)
	})
	if err != nil {
		t.Fatalf("newPhotoMigrator: %v", err)
	}
	return m, &events
}

// canonicalRef builds the SAME class-namespaced, content-addressed key
// LocalPhotoStore.Put/S3PhotoStore.Put would have produced for hh/class/data
// — the realistic shape virtually every row's StorageRef already has (see
// mediaadapter.BuildStorageKey's doc); seedLocalAlbumPhoto/
// seedLocalChoreProofPhoto use this rather than an arbitrary string so the
// sweep pass (which looks a row's local file up BY ITS OWN current ref —
// see sweepOneLeftoverLocalFile's doc for the legacy-ref caveat that
// deliberately does not apply here) finds it.
func canonicalRef(t *testing.T, hh household.HouseholdID, class mediadomain.PhotoClass, data []byte) mediadomain.StorageRef {
	t.Helper()
	key, err := mediaadapter.BuildStorageKey(hh, class, sha256Hex(data), "jpg")
	if err != nil {
		t.Fatalf("BuildStorageKey: %v", err)
	}
	return mediadomain.StorageRef(key)
}

func seedLocalAlbumPhoto(t *testing.T, local *fakeLocalStore, photos *fakePhotoRepo, hh household.HouseholdID, data []byte, contentHash string) *mediadomain.Photo {
	t.Helper()
	ref := canonicalRef(t, hh, mediadomain.PhotoClassAlbum, data)
	local.seed(ref, data, "image/jpeg")
	p := &mediadomain.Photo{
		ID: mediadomain.NewPhotoID(), HouseholdID: hh, StorageRef: ref,
		ContentHash: contentHash, SizeBytes: int64(len(data)), ContentType: "image/jpeg",
		StorageBackend: mediadomain.StorageBackendLocal,
	}
	photos.seed(p)
	return p
}

func seedLocalChoreProofPhoto(t *testing.T, local *fakeLocalStore, repo *fakeTaskInstancePhotoRepo, hh household.HouseholdID, data []byte, contentHash string) *mediadomain.TaskInstancePhoto {
	t.Helper()
	ref := canonicalRef(t, hh, mediadomain.PhotoClassChoreProof, data)
	local.seed(ref, data, "image/jpeg")
	p := &mediadomain.TaskInstancePhoto{
		ID: mediadomain.NewTaskInstancePhotoID(), HouseholdID: hh, TaskInstanceID: mediadomain.TaskInstanceID(household.NewHouseholdID()),
		Kind: mediadomain.PhotoKindBefore, StorageRef: ref,
		ContentHash: contentHash, SizeBytes: int64(len(data)), ContentType: "image/jpeg",
		TakenAt: time.Now().UTC(), StorageBackend: mediadomain.StorageBackendLocal,
	}
	repo.seed(p)
	return p
}

// TestPhotoMigratorMigratesAlbumPhoto covers the basic happy path: a
// local-backend album row is uploaded to the target backend under its
// canonical key and flipped to s3.
func TestPhotoMigratorMigratesAlbumPhoto(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	data := []byte("album photo bytes")
	hash := sha256Hex(data)
	photo := seedLocalAlbumPhoto(t, local, photos, hh, data, hash)

	m, events := newTestMigrator(t, local, target, photos, choreProofPhotos)
	result, err := m.Migrate(context.Background(), migrateOptions{})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	wantKey, err := mediaadapter.BuildStorageKey(hh, mediadomain.PhotoClassAlbum, hash, "jpg")
	if err != nil {
		t.Fatalf("BuildStorageKey: %v", err)
	}
	if photo.StorageBackend != mediadomain.StorageBackendS3 {
		t.Fatalf("photo.StorageBackend = %q, want s3", photo.StorageBackend)
	}
	if photo.StorageRef.String() != wantKey {
		t.Fatalf("photo.StorageRef = %q, want canonical key %q", photo.StorageRef, wantKey)
	}
	if target.putAtCalls != 1 {
		t.Fatalf("putAtCalls = %d, want 1", target.putAtCalls)
	}
	if _, ok := target.objects[mediadomain.StorageRef(wantKey)]; !ok {
		t.Fatalf("target store does not hold the migrated object at %q", wantKey)
	}

	var classResult *migrateClassResult
	for i := range result.Classes {
		if result.Classes[i].Class == mediadomain.PhotoClassAlbum {
			classResult = &result.Classes[i]
		}
	}
	if classResult == nil || classResult.Migrated != 1 {
		t.Fatalf("album class result = %+v, want Migrated=1", classResult)
	}
	if len(*events) == 0 {
		t.Fatal("expected at least one progress event")
	}
}

// TestPhotoMigratorResumeIsIdempotent covers AC1: interrupting the
// migrator mid-run and re-running it completes without duplicating
// uploads or corrupting state — simulated here by running Migrate twice
// over the SAME repos/stores. The second run must find nothing left to do
// (the first run's rows no longer show up as local-backend) and must not
// call PutAt again.
func TestPhotoMigratorResumeIsIdempotent(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	albumData := []byte("resumable album bytes")
	albumPhoto := seedLocalAlbumPhoto(t, local, photos, hh, albumData, sha256Hex(albumData))
	choreData := []byte("resumable chore-proof bytes")
	chorePhoto := seedLocalChoreProofPhoto(t, local, choreProofPhotos, hh, choreData, sha256Hex(choreData))

	m, _ := newTestMigrator(t, local, target, photos, choreProofPhotos)

	first, err := m.Migrate(context.Background(), migrateOptions{})
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	putAtAfterFirst := target.putAtCalls

	second, err := m.Migrate(context.Background(), migrateOptions{})
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	for _, cr := range first.Classes {
		if cr.Migrated != 1 {
			t.Fatalf("first run class %s: Migrated = %d, want 1", cr.Class, cr.Migrated)
		}
	}
	for _, cr := range second.Classes {
		if cr.Migrated != 0 {
			t.Fatalf("second run class %s: Migrated = %d, want 0 (nothing local left)", cr.Class, cr.Migrated)
		}
	}
	if target.putAtCalls != putAtAfterFirst {
		t.Fatalf("second run uploaded again: putAtCalls = %d, want %d (unchanged)", target.putAtCalls, putAtAfterFirst)
	}
	if albumPhoto.StorageBackend != mediadomain.StorageBackendS3 || chorePhoto.StorageBackend != mediadomain.StorageBackendS3 {
		t.Fatal("both rows must be flipped to s3 after resuming")
	}
}

// TestPhotoMigratorDedupSkipsReupload covers the content-addressed dedup
// case: two chore-proof rows in the same household share byte-identical
// content (and so the same local ref AND the same canonical target key).
// Migrating the second must find the object already present (via
// ObjectExists) and skip re-uploading it, while still flipping its own row.
func TestPhotoMigratorDedupSkipsReupload(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	data := []byte("shared chore-proof bytes")
	hash := sha256Hex(data)
	sharedRef := mediadomain.StorageRef("hh/cc/shared.jpg")
	local.seed(sharedRef, data, "image/jpeg")

	rowA := &mediadomain.TaskInstancePhoto{
		ID: mediadomain.NewTaskInstancePhotoID(), HouseholdID: hh, TaskInstanceID: mediadomain.TaskInstanceID(household.NewHouseholdID()),
		Kind: mediadomain.PhotoKindBefore, StorageRef: sharedRef, ContentHash: hash,
		SizeBytes: int64(len(data)), ContentType: "image/jpeg", TakenAt: time.Now().UTC(),
		StorageBackend: mediadomain.StorageBackendLocal,
	}
	rowB := &mediadomain.TaskInstancePhoto{
		ID: mediadomain.NewTaskInstancePhotoID(), HouseholdID: hh, TaskInstanceID: mediadomain.TaskInstanceID(household.NewHouseholdID()),
		Kind: mediadomain.PhotoKindAfter, StorageRef: sharedRef, ContentHash: hash,
		SizeBytes: int64(len(data)), ContentType: "image/jpeg", TakenAt: time.Now().UTC(),
		StorageBackend: mediadomain.StorageBackendLocal,
	}
	choreProofPhotos.seed(rowA)
	choreProofPhotos.seed(rowB)

	m, _ := newTestMigrator(t, local, target, photos, choreProofPhotos)
	result, err := m.Migrate(context.Background(), migrateOptions{Classes: []mediadomain.PhotoClass{mediadomain.PhotoClassChoreProof}})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if target.putAtCalls != 1 {
		t.Fatalf("putAtCalls = %d, want exactly 1 (dedup must skip the second upload)", target.putAtCalls)
	}
	if rowA.StorageBackend != mediadomain.StorageBackendS3 || rowB.StorageBackend != mediadomain.StorageBackendS3 {
		t.Fatal("both rows must be flipped to s3 even though only one upload happened")
	}
	if rowA.StorageRef != rowB.StorageRef {
		t.Fatalf("dedup rows should resolve to the SAME canonical target ref: %q vs %q", rowA.StorageRef, rowB.StorageRef)
	}
	if len(result.Classes) != 1 || result.Classes[0].Migrated != 2 {
		t.Fatalf("chore-proof class result = %+v, want Migrated=2", result.Classes)
	}
}

// TestPhotoMigratorHashMismatchAbortsFlip covers AC2: a corrupted local
// file's hash no longer matches the row's recorded content_sha256 — the
// flip must be aborted (the row keeps serving from local), the mismatch
// must be counted, and the migrator must continue processing other rows.
func TestPhotoMigratorHashMismatchAbortsFlip(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	goodData := []byte("intact photo bytes")
	good := seedLocalAlbumPhoto(t, local, photos, hh, goodData, sha256Hex(goodData))

	corruptRef := mediadomain.StorageRef("hh/dd/corrupt.jpg")
	original := []byte("original photo bytes")
	local.seed(corruptRef, []byte("CORRUPTED — not the original bytes"), "image/jpeg")
	corrupt := &mediadomain.Photo{
		ID: mediadomain.NewPhotoID(), HouseholdID: hh, StorageRef: corruptRef,
		ContentHash: sha256Hex(original), SizeBytes: int64(len(original)), ContentType: "image/jpeg",
		StorageBackend: mediadomain.StorageBackendLocal,
	}
	photos.seed(corrupt)

	m, events := newTestMigrator(t, local, target, photos, choreProofPhotos)
	result, err := m.Migrate(context.Background(), migrateOptions{Classes: []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum}})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if corrupt.StorageBackend != mediadomain.StorageBackendLocal {
		t.Fatalf("corrupt row's StorageBackend = %q, want local (flip must be aborted)", corrupt.StorageBackend)
	}
	if corrupt.StorageRef != corruptRef {
		t.Fatalf("corrupt row's StorageRef changed to %q, want unchanged %q", corrupt.StorageRef, corruptRef)
	}
	if good.StorageBackend != mediadomain.StorageBackendS3 {
		t.Fatal("the migrator must continue past the mismatch and still migrate the other (good) row")
	}
	if len(result.Classes) != 1 || result.Classes[0].HashMismatches != 1 || result.Classes[0].Migrated != 1 {
		t.Fatalf("album class result = %+v, want HashMismatches=1 Migrated=1", result.Classes[0])
	}

	foundMismatch := false
	for _, e := range *events {
		if e.Outcome == migrateOutcomeHashMismatch && e.Ref == corruptRef {
			foundMismatch = true
		}
	}
	if !foundMismatch {
		t.Fatal("expected a hash-mismatch progress event for the corrupt ref")
	}
}

// TestPhotoMigratorBackfillsLegacyNullHash covers the legacy pre-NES-123
// photo.Photo row case: content_sha256 is blank in the DB, so there is no
// expected hash to verify against; the migrator computes it from the bytes
// and backfills content_sha256 in the same flip.
func TestPhotoMigratorBackfillsLegacyNullHash(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	data := []byte("legacy photo predating content hashing")
	legacy := seedLocalAlbumPhoto(t, local, photos, hh, data, "" /* legacy: no stored hash */)

	m, _ := newTestMigrator(t, local, target, photos, choreProofPhotos)
	if _, err := m.Migrate(context.Background(), migrateOptions{Classes: []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum}}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if legacy.StorageBackend != mediadomain.StorageBackendS3 {
		t.Fatal("legacy row must still migrate despite having no stored hash")
	}
	if legacy.ContentHash != sha256Hex(data) {
		t.Fatalf("legacy row's ContentHash = %q, want backfilled %q", legacy.ContentHash, sha256Hex(data))
	}
}

// TestPhotoMigratorDeleteLocalDeletesUnreferencedSoloFile covers AC4's
// straightforward case: --delete-local removes a row's local file once its
// move to the target backend is verified, when no other row references it.
func TestPhotoMigratorDeleteLocalDeletesUnreferencedSoloFile(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	soloData := []byte("solo album photo, no other reference")
	solo := seedLocalAlbumPhoto(t, local, photos, hh, soloData, sha256Hex(soloData))
	soloOriginalRef := solo.StorageRef

	m, _ := newTestMigrator(t, local, target, photos, choreProofPhotos)
	result, err := m.Migrate(context.Background(), migrateOptions{
		Classes:     []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum},
		DeleteLocal: true,
	})
	if err != nil {
		t.Fatalf("Migrate album: %v", err)
	}
	if result.Classes[0].DeletedLocal != 1 {
		t.Fatalf("album DeletedLocal = %d, want 1 (the solo photo's local file)", result.Classes[0].DeletedLocal)
	}
	if _, ok := local.objects[soloOriginalRef]; ok {
		t.Fatal("solo photo's local file should have been deleted (verified, unreferenced)")
	}
}

// TestPhotoMigratorDeleteLocalSkipsWhenAnotherLocalRowStillReferencesIt
// covers AC4's safety half directly: deleteLocalIfUnreferenced must NOT
// delete a local file another still-local-backend row references
// (content-addressed dedup — two chore-proof rows can share one local
// object), and must delete it once that sibling reference is gone. Testing
// deleteLocalIfUnreferenced directly (rather than through Migrate's
// row-processing order, which is not something callers should rely on)
// keeps this deterministic.
func TestPhotoMigratorDeleteLocalSkipsWhenAnotherLocalRowStillReferencesIt(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	target.classOfKeyFn = mediaadapter.ClassOfKey
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	sharedData := []byte("chore-proof bytes shared by two rows")
	sharedRef := canonicalRef(t, hh, mediadomain.PhotoClassChoreProof, sharedData)
	local.seed(sharedRef, sharedData, "image/jpeg")

	rowA := &mediadomain.TaskInstancePhoto{
		ID: mediadomain.NewTaskInstancePhotoID(), HouseholdID: hh, TaskInstanceID: mediadomain.TaskInstanceID(household.NewHouseholdID()),
		Kind: mediadomain.PhotoKindBefore, StorageRef: sharedRef, ContentHash: sha256Hex(sharedData),
		SizeBytes: int64(len(sharedData)), ContentType: "image/jpeg", TakenAt: time.Now().UTC(),
		StorageBackend: mediadomain.StorageBackendS3, // already migrated
	}
	rowB := &mediadomain.TaskInstancePhoto{
		ID: mediadomain.NewTaskInstancePhotoID(), HouseholdID: hh, TaskInstanceID: mediadomain.TaskInstanceID(household.NewHouseholdID()),
		Kind: mediadomain.PhotoKindAfter, StorageRef: sharedRef, ContentHash: sha256Hex(sharedData),
		SizeBytes: int64(len(sharedData)), ContentType: "image/jpeg", TakenAt: time.Now().UTC(),
		StorageBackend: mediadomain.StorageBackendLocal, // NOT migrated yet
	}
	choreProofPhotos.seed(rowA)
	choreProofPhotos.seed(rowB)

	m, err := newPhotoMigrator(local, target, mediadomain.StorageBackendS3, photos, choreProofPhotos, 10<<20, nil)
	if err != nil {
		t.Fatalf("newPhotoMigrator: %v", err)
	}

	deleted, err := m.deleteLocalIfUnreferenced(context.Background(), sharedRef)
	if err != nil {
		t.Fatalf("deleteLocalIfUnreferenced (rowB still local): %v", err)
	}
	if deleted {
		t.Fatal("must not delete: rowB is still local-backend and references the same file")
	}
	if _, ok := local.objects[sharedRef]; !ok {
		t.Fatal("shared local file must survive while rowB still references it")
	}

	// rowB itself is migrated (mirroring what Migrate would do to it before
	// its own delete-local check): the file now has no remaining local
	// reference.
	rowB.StorageBackend = mediadomain.StorageBackendS3

	deleted, err = m.deleteLocalIfUnreferenced(context.Background(), sharedRef)
	if err != nil {
		t.Fatalf("deleteLocalIfUnreferenced (nothing left local): %v", err)
	}
	if !deleted {
		t.Fatal("must delete now: no local-backend row references the file anymore")
	}
	if _, ok := local.objects[sharedRef]; ok {
		t.Fatal("shared local file should now be deleted")
	}
}

// TestPhotoMigratorSweepReclaimsLeftoverLocalFilesFromPriorRun covers the
// "migrate now, --delete-local much later" runbook: a row migrated WITHOUT
// --delete-local leaves its local file behind; re-running Migrate with
// --delete-local later must find and reclaim it via the sweep pass, even
// though the row is no longer local-backend at all.
func TestPhotoMigratorSweepReclaimsLeftoverLocalFilesFromPriorRun(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	data := []byte("migrated without delete-local first")
	photo := seedLocalAlbumPhoto(t, local, photos, hh, data, sha256Hex(data))
	originalLocalRef := photo.StorageRef

	m, _ := newTestMigrator(t, local, target, photos, choreProofPhotos)
	if _, err := m.Migrate(context.Background(), migrateOptions{Classes: []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum}}); err != nil {
		t.Fatalf("first Migrate (no delete-local): %v", err)
	}
	if photo.StorageBackend != mediadomain.StorageBackendS3 {
		t.Fatal("row should already be s3-backed after the first run")
	}
	if _, ok := local.objects[originalLocalRef]; !ok {
		t.Fatal("local file must still exist: --delete-local was not requested on the first run")
	}

	m2, _ := newTestMigrator(t, local, target, photos, choreProofPhotos)
	result, err := m2.Migrate(context.Background(), migrateOptions{
		Classes:     []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum},
		DeleteLocal: true,
	})
	if err != nil {
		t.Fatalf("second Migrate (delete-local): %v", err)
	}
	if result.Classes[0].Migrated != 0 {
		t.Fatalf("second run Migrated = %d, want 0 (row is already s3-backed)", result.Classes[0].Migrated)
	}
	if result.Classes[0].DeletedLocal != 1 {
		t.Fatalf("second run DeletedLocal = %d, want 1 (the sweep must find and delete the leftover local file)", result.Classes[0].DeletedLocal)
	}
	if _, ok := local.objects[originalLocalRef]; ok {
		t.Fatal("leftover local file should now be deleted by the sweep pass")
	}
}

// TestPhotoMigratorPropagatesPutAtError covers a hard per-row error (an
// upload failure): it must be counted and reported, and must not silently
// flip the row.
func TestPhotoMigratorPropagatesPutAtError(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	target.putAtErr = errors.New("simulated network failure")
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	data := []byte("upload will fail")
	photo := seedLocalAlbumPhoto(t, local, photos, hh, data, sha256Hex(data))

	m, events := newTestMigrator(t, local, target, photos, choreProofPhotos)
	result, err := m.Migrate(context.Background(), migrateOptions{Classes: []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum}})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if photo.StorageBackend != mediadomain.StorageBackendLocal {
		t.Fatal("row must not be flipped when the upload failed")
	}
	if result.Classes[0].Errors != 1 {
		t.Fatalf("Errors = %d, want 1", result.Classes[0].Errors)
	}
	if !result.HasFindings() {
		t.Fatal("HasFindings() should be true when a hard error occurred")
	}
	foundError := false
	for _, e := range *events {
		if e.Outcome == migrateOutcomeError {
			foundError = true
		}
	}
	if !foundError {
		t.Fatal("expected an error progress event")
	}
}
