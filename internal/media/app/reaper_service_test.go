package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/app"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// fakeObjectLister fakes domain.ObjectLister: a fixed set of objects per
// class, so a test can shape exactly what "the bucket contains" without a
// real object store.
type fakeObjectLister struct {
	objects map[domain.PhotoClass][]domain.ObjectInfo
	listErr error
}

func (f *fakeObjectLister) ListObjects(_ context.Context, class domain.PhotoClass) ([]domain.ObjectInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.objects[class], nil
}

const testGraceWindow = 24 * time.Hour

func newTestReaper(t *testing.T, lister *fakeObjectLister, store *fakePhotoStore, photos *fakePhotoRepo, choreProofPhotos *fakeTaskInstancePhotoRepo, retention time.Duration) *app.ReaperService {
	t.Helper()
	// domain.StorageBackendLocal matches newFakePhotoRepo/newFakeTaskInstancePhotoRepo's
	// own default backend, so every test's directly-constructed rows (and
	// every row Create stamps) are visible to this reaper's backend-scoped
	// referencedRefs/existsByStorageRef queries.
	r, err := app.NewReaperService(lister, store, domain.StorageBackendLocal, photos, choreProofPhotos, testGraceWindow, retention)
	if err != nil {
		t.Fatalf("NewReaperService: %v", err)
	}
	return r
}

// TestNewReaperServiceValidatesDependencies covers the nil-dependency,
// invalid-backend, and non-positive-graceWindow guards.
func TestNewReaperServiceValidatesDependencies(t *testing.T) {
	lister := &fakeObjectLister{}
	store := &fakePhotoStore{}
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	cases := []struct {
		name    string
		lister  domain.ObjectLister
		store   domain.PhotoStore
		backend domain.StorageBackend
		photos  domain.PhotoRepository
		cpp     domain.TaskInstancePhotoRepository
		grace   time.Duration
	}{
		{"nil lister", nil, store, domain.StorageBackendLocal, photos, choreProofPhotos, testGraceWindow},
		{"nil store", lister, nil, domain.StorageBackendLocal, photos, choreProofPhotos, testGraceWindow},
		{"invalid backend", lister, store, domain.StorageBackend("azure-blob"), photos, choreProofPhotos, testGraceWindow},
		{"nil photos repo", lister, store, domain.StorageBackendLocal, nil, choreProofPhotos, testGraceWindow},
		{"nil chore-proof repo", lister, store, domain.StorageBackendLocal, photos, nil, testGraceWindow},
		{"zero grace window", lister, store, domain.StorageBackendLocal, photos, choreProofPhotos, 0},
		{"negative grace window", lister, store, domain.StorageBackendLocal, photos, choreProofPhotos, -time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := app.NewReaperService(tc.lister, tc.store, tc.backend, tc.photos, tc.cpp, tc.grace, 0); err == nil {
				t.Fatal("NewReaperService should have failed")
			}
		})
	}
}

// TestReaperDeletesUnreferencedObjectsPastGraceWindow covers AC3: an object
// with no referencing row, older than the grace window, is deleted; a
// referenced object never is, regardless of age.
func TestReaperDeletesUnreferencedObjectsPastGraceWindow(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * testGraceWindow)

	hh := household.NewHouseholdID()
	referenced := &domain.Photo{ID: domain.NewPhotoID(), HouseholdID: hh, StorageRef: domain.StorageRef("households/" + hh.String() + "/photos/aa/referenced.jpg"), StorageBackend: domain.StorageBackendLocal}
	photos := newFakePhotoRepo()
	photos.store[referenced.ID] = referenced

	lister := &fakeObjectLister{objects: map[domain.PhotoClass][]domain.ObjectInfo{
		domain.PhotoClassAlbum: {
			{Key: referenced.StorageRef, LastModified: old},
			{Key: domain.StorageRef("households/" + hh.String() + "/photos/bb/orphan.jpg"), LastModified: old},
		},
	}}
	store := &fakePhotoStore{}
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	r := newTestReaper(t, lister, store, photos, choreProofPhotos, 0)
	result, err := r.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OrphansDeleted[domain.PhotoClassAlbum] != 1 {
		t.Fatalf("OrphansDeleted[album] = %d, want 1", result.OrphansDeleted[domain.PhotoClassAlbum])
	}
	if len(store.deleted) != 1 || store.deleted[0] != domain.StorageRef("households/"+hh.String()+"/photos/bb/orphan.jpg") {
		t.Fatalf("store.deleted = %v, want exactly the orphan ref", store.deleted)
	}
}

// TestReaperSkipsObjectsWithinGraceWindow covers the other half of AC3:
// deleting a photo hides it immediately (a row-only delete, out of this
// service's scope) but the object survives the grace window — an
// unreferenced object younger than the grace window must not be deleted
// yet, since it might be a concurrent, not-yet-committed upload.
func TestReaperSkipsObjectsWithinGraceWindow(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-testGraceWindow / 2)

	photos := newFakePhotoRepo()
	lister := &fakeObjectLister{objects: map[domain.PhotoClass][]domain.ObjectInfo{
		domain.PhotoClassAlbum: {
			{Key: domain.StorageRef("households/hh/photos/bb/fresh-orphan.jpg"), LastModified: recent},
		},
	}}
	store := &fakePhotoStore{}
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	r := newTestReaper(t, lister, store, photos, choreProofPhotos, 0)
	result, err := r.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OrphansDeleted[domain.PhotoClassAlbum] != 0 {
		t.Fatalf("OrphansDeleted[album] = %d, want 0 (object is within the grace window)", result.OrphansDeleted[domain.PhotoClassAlbum])
	}
	if len(store.deleted) != 0 {
		t.Fatalf("store.deleted = %v, want none", store.deleted)
	}
}

// TestReaperRestoreSafety covers AC5: restoring a week-old DB dump (rows
// re-inserted with the same StorageRef they always had) must leave every
// photo's object un-reaped, even one the reaper would otherwise have
// deleted — a restored row reappearing in ListAllStorageRefs protects its
// object on the very next Run.
func TestReaperRestoreSafety(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * testGraceWindow)
	ref := domain.StorageRef("households/hh/photos/aa/restored.jpg")

	hh := household.NewHouseholdID()
	photos := newFakePhotoRepo()
	restored := &domain.Photo{ID: domain.NewPhotoID(), HouseholdID: hh, StorageRef: ref, StorageBackend: domain.StorageBackendLocal}
	photos.store[restored.ID] = restored // simulates the row re-inserted after a restore

	lister := &fakeObjectLister{objects: map[domain.PhotoClass][]domain.ObjectInfo{
		domain.PhotoClassAlbum: {{Key: ref, LastModified: old}},
	}}
	store := &fakePhotoStore{}
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	r := newTestReaper(t, lister, store, photos, choreProofPhotos, 0)
	result, err := r.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OrphansDeleted[domain.PhotoClassAlbum] != 0 {
		t.Fatalf("OrphansDeleted[album] = %d, want 0 (row was restored, object is referenced again)", result.OrphansDeleted[domain.PhotoClassAlbum])
	}
	if len(store.deleted) != 0 {
		t.Fatalf("a restored, still-referenced object must never be deleted, got %v", store.deleted)
	}
}

// TestReaperRecheckCatchesRowCommittedAfterSnapshot covers the TOCTOU
// narrowing fix directly: a row that commits AFTER the bulk
// ListAllStorageRefs snapshot was taken but BEFORE the per-object delete
// runs (e.g. a restore landing mid-Run) must still protect its object,
// because sweepClass's targeted ExistsByStorageRef recheck — not the stale
// snapshot — is what gates the delete. existsOverride simulates exactly
// this: the snapshot (photos.store) is EMPTY (the object looks orphaned as
// of the snapshot), but the fresh per-ref recheck reports it referenced.
func TestReaperRecheckCatchesRowCommittedAfterSnapshot(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * testGraceWindow)
	ref := domain.StorageRef("households/hh/photos/aa/mid-run-restore.jpg")

	photos := newFakePhotoRepo() // empty: the snapshot sees ref as unreferenced
	photos.existsOverride = map[domain.StorageRef]bool{ref: true}
	lister := &fakeObjectLister{objects: map[domain.PhotoClass][]domain.ObjectInfo{
		domain.PhotoClassAlbum: {{Key: ref, LastModified: old}},
	}}
	store := &fakePhotoStore{}
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	r := newTestReaper(t, lister, store, photos, choreProofPhotos, 0)
	result, err := r.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OrphansDeleted[domain.PhotoClassAlbum] != 0 {
		t.Fatalf("OrphansDeleted[album] = %d, want 0 — the recheck must catch the mid-run commit even though the snapshot missed it", result.OrphansDeleted[domain.PhotoClassAlbum])
	}
	if len(store.deleted) != 0 {
		t.Fatalf("store.deleted = %v, want none: the recheck must have run and found the ref referenced", store.deleted)
	}
	if len(photos.existsCalls) == 0 || photos.existsCalls[0] != ref {
		t.Fatalf("ExistsByStorageRef was not called for the candidate ref: existsCalls = %v", photos.existsCalls)
	}
}

// TestReaperRecheckIsPerCandidateNotBatched is a regression test for a
// review finding on the DryRun refactor: candidate selection used to
// recheck EVERY candidate up front, in one pass, and ONLY THEN did a
// separate loop delete them — which reopened exactly the TOCTOU window
// TestReaperRecheckCatchesRowCommittedAfterSnapshot exists to close, for
// any commit that lands WHILE an EARLIER candidate in the same Run is
// being processed (not just before Run starts at all). Two candidates in
// the same class: deleting the FIRST one (via fakePhotoStore.deleteHook)
// injects a fresh reference for the SECOND — simulating a row committing
// mid-Run, between the two candidates' turns. The second candidate must
// survive, which only happens if its own recheck runs immediately before
// ITS OWN delete (i.e. AFTER the first candidate has already been fully
// processed), not in an earlier batched recheck pass completed before any
// deletes began.
func TestReaperRecheckIsPerCandidateNotBatched(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * testGraceWindow)
	refA := domain.StorageRef("households/hh/photos/aa/first.jpg")
	refB := domain.StorageRef("households/hh/photos/bb/second.jpg")

	photos := newFakePhotoRepo() // both refs unreferenced in the bulk snapshot
	lister := &fakeObjectLister{objects: map[domain.PhotoClass][]domain.ObjectInfo{
		domain.PhotoClassAlbum: {
			{Key: refA, LastModified: old},
			{Key: refB, LastModified: old},
		},
	}}
	store := &fakePhotoStore{}
	// Fires exactly when refA is deleted — i.e. WHILE refA is being
	// processed, before refB's own turn in the loop — simulating a row
	// committing a reference to refB at that instant.
	store.deleteHook = func(deleted domain.StorageRef) {
		if deleted == refA {
			photos.existsOverride = map[domain.StorageRef]bool{refB: true}
		}
	}
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	r := newTestReaper(t, lister, store, photos, choreProofPhotos, 0)
	result, err := r.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OrphansDeleted[domain.PhotoClassAlbum] != 1 {
		t.Fatalf("OrphansDeleted[album] = %d, want 1 (only refA — refB's mid-run commit must protect it)", result.OrphansDeleted[domain.PhotoClassAlbum])
	}
	if len(store.deleted) != 1 || store.deleted[0] != refA {
		t.Fatalf("store.deleted = %v, want exactly [refA]", store.deleted)
	}
}

// TestReaperWalksBothClasses covers the ticket's "walking BOTH photo
// classes under their prefixes": an unreferenced object in EACH class is
// independently detected and deleted in one Run.
func TestReaperWalksBothClasses(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * testGraceWindow)

	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()
	albumOrphan := domain.StorageRef("households/hh/photos/aa/orphan.jpg")
	choreOrphan := domain.StorageRef("households/hh/chore-photos/bb/orphan.jpg")
	lister := &fakeObjectLister{objects: map[domain.PhotoClass][]domain.ObjectInfo{
		domain.PhotoClassAlbum:      {{Key: albumOrphan, LastModified: old}},
		domain.PhotoClassChoreProof: {{Key: choreOrphan, LastModified: old}},
	}}
	store := &fakePhotoStore{}

	r := newTestReaper(t, lister, store, photos, choreProofPhotos, 0)
	result, err := r.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OrphansDeleted[domain.PhotoClassAlbum] != 1 || result.OrphansDeleted[domain.PhotoClassChoreProof] != 1 {
		t.Fatalf("OrphansDeleted = %v, want 1 for each class", result.OrphansDeleted)
	}
	if len(store.deleted) != 2 {
		t.Fatalf("store.deleted = %v, want both orphans deleted", store.deleted)
	}
}

// TestReaperAppliesChoreProofRetention covers the optional per-class
// retention pass: a chore-proof row older than the configured retention is
// deleted (row-only — the object is left for the ordinary orphan sweep,
// bounded by its own grace window) when ChoreProofRetention is positive.
func TestReaperAppliesChoreProofRetention(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	retention := 30 * 24 * time.Hour

	choreProofPhotos := newFakeTaskInstancePhotoRepo()
	old := &domain.TaskInstancePhoto{ID: domain.NewTaskInstancePhotoID(), StorageRef: "households/hh/chore-photos/aa/old.jpg", UploadedAt: now.Add(-40 * 24 * time.Hour), StorageBackend: domain.StorageBackendLocal}
	fresh := &domain.TaskInstancePhoto{ID: domain.NewTaskInstancePhotoID(), StorageRef: "households/hh/chore-photos/bb/fresh.jpg", UploadedAt: now.Add(-5 * 24 * time.Hour), StorageBackend: domain.StorageBackendLocal}
	choreProofPhotos.created = append(choreProofPhotos.created, old, fresh)

	photos := newFakePhotoRepo()
	lister := &fakeObjectLister{}
	store := &fakePhotoStore{}

	r := newTestReaper(t, lister, store, photos, choreProofPhotos, retention)
	result, err := r.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RetentionRowsDeleted != 1 {
		t.Fatalf("RetentionRowsDeleted = %d, want 1", result.RetentionRowsDeleted)
	}
	if len(choreProofPhotos.created) != 1 || choreProofPhotos.created[0] != fresh {
		t.Fatalf("retention pass did not leave exactly the fresh row: %v", choreProofPhotos.created)
	}
}

// TestReaperSkipsRetentionWhenDisabled covers the "0 = keep forever"
// default: no retention pass runs, and every chore-proof row survives.
func TestReaperSkipsRetentionWhenDisabled(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	choreProofPhotos := newFakeTaskInstancePhotoRepo()
	old := &domain.TaskInstancePhoto{ID: domain.NewTaskInstancePhotoID(), StorageRef: "households/hh/chore-photos/aa/old.jpg", UploadedAt: now.Add(-400 * 24 * time.Hour), StorageBackend: domain.StorageBackendLocal}
	choreProofPhotos.created = append(choreProofPhotos.created, old)

	photos := newFakePhotoRepo()
	lister := &fakeObjectLister{}
	store := &fakePhotoStore{}

	r := newTestReaper(t, lister, store, photos, choreProofPhotos, 0)
	result, err := r.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RetentionRowsDeleted != 0 {
		t.Fatalf("RetentionRowsDeleted = %d, want 0 when retention is disabled", result.RetentionRowsDeleted)
	}
	if len(choreProofPhotos.created) != 1 {
		t.Fatalf("retention pass ran despite being disabled: %v", choreProofPhotos.created)
	}
}

// TestReaperDryRunPreviewsWithoutDeleting covers NES-133's `storage reap
// --dry-run`: DryRun reports the exact same candidates Run would delete —
// both the retention row count and each class's orphaned object refs —
// WITHOUT deleting or removing anything.
func TestReaperDryRunPreviewsWithoutDeleting(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * testGraceWindow)
	retention := 30 * 24 * time.Hour

	hh := household.NewHouseholdID()
	referenced := &domain.Photo{ID: domain.NewPhotoID(), HouseholdID: hh, StorageRef: domain.StorageRef("households/" + hh.String() + "/photos/aa/referenced.jpg"), StorageBackend: domain.StorageBackendLocal}
	photos := newFakePhotoRepo()
	photos.store[referenced.ID] = referenced

	orphanRef := domain.StorageRef("households/" + hh.String() + "/photos/bb/orphan.jpg")
	lister := &fakeObjectLister{objects: map[domain.PhotoClass][]domain.ObjectInfo{
		domain.PhotoClassAlbum: {
			{Key: referenced.StorageRef, LastModified: old},
			{Key: orphanRef, LastModified: old},
		},
	}}
	store := &fakePhotoStore{}

	choreProofPhotos := newFakeTaskInstancePhotoRepo()
	oldChoreProof := &domain.TaskInstancePhoto{ID: domain.NewTaskInstancePhotoID(), StorageRef: "households/hh/chore-photos/aa/old.jpg", UploadedAt: now.Add(-40 * 24 * time.Hour), StorageBackend: domain.StorageBackendLocal}
	choreProofPhotos.created = append(choreProofPhotos.created, oldChoreProof)

	r, err := app.NewReaperService(lister, store, domain.StorageBackendLocal, photos, choreProofPhotos, testGraceWindow, retention)
	if err != nil {
		t.Fatalf("NewReaperService: %v", err)
	}

	preview, err := r.DryRun(context.Background(), now)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if preview.RetentionRowsWouldDelete != 1 {
		t.Fatalf("RetentionRowsWouldDelete = %d, want 1", preview.RetentionRowsWouldDelete)
	}
	if got := preview.OrphansWouldDelete[domain.PhotoClassAlbum]; len(got) != 1 || got[0] != orphanRef {
		t.Fatalf("OrphansWouldDelete[album] = %v, want exactly [%s]", got, orphanRef)
	}

	// Nothing was actually touched: the row survives, the object survives,
	// and a subsequent real Run still finds exactly the same work to do.
	if len(store.deleted) != 0 {
		t.Fatalf("DryRun deleted objects: %v, want none", store.deleted)
	}
	if len(choreProofPhotos.created) != 1 {
		t.Fatalf("DryRun removed rows: %v, want the row still present", choreProofPhotos.created)
	}

	result, err := r.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run after DryRun: %v", err)
	}
	if result.RetentionRowsDeleted != 1 {
		t.Fatalf("Run RetentionRowsDeleted = %d, want 1 (matching the preview)", result.RetentionRowsDeleted)
	}
	if result.OrphansDeleted[domain.PhotoClassAlbum] != 1 {
		t.Fatalf("Run OrphansDeleted[album] = %d, want 1 (matching the preview)", result.OrphansDeleted[domain.PhotoClassAlbum])
	}
}

// TestReaperProtectsCrossPrefixReference is a CRITICAL regression test: an
// object physically filed under the CHORE-PROOF prefix, but referenced
// only by an ALBUM row (a cross-prefix row — see cmd/storage's verifier.go
// classifyS3 doc for the identical scenario on the verify side), must
// survive the orphan sweep. Before this fix, referencedRefs/
// existsByStorageRef only consulted the repository matching the CLASS the
// object was listed under (chore-proof here, via lister.ListObjects), so
// the album row's genuine reference was never seen — the sweep would
// treat a live reference as an orphan and delete it.
func TestReaperProtectsCrossPrefixReference(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * testGraceWindow)

	hh := household.NewHouseholdID()
	// An object physically listed under the CHORE-PROOF prefix...
	crossPrefixRef := domain.StorageRef("households/" + hh.String() + "/chore-photos/aa/cross-prefix.jpg")
	lister := &fakeObjectLister{objects: map[domain.PhotoClass][]domain.ObjectInfo{
		domain.PhotoClassChoreProof: {{Key: crossPrefixRef, LastModified: old}},
	}}
	store := &fakePhotoStore{}

	// ...but referenced ONLY by an ALBUM row (the row's own ref embeds the
	// WRONG class's key prefix for the table it's actually persisted in —
	// exactly the cross-prefix scenario `storage verify` also flags).
	photos := newFakePhotoRepo()
	albumRow := &domain.Photo{ID: domain.NewPhotoID(), HouseholdID: hh, StorageRef: crossPrefixRef, StorageBackend: domain.StorageBackendLocal}
	photos.store[albumRow.ID] = albumRow
	choreProofPhotos := newFakeTaskInstancePhotoRepo() // no chore-proof row references it

	r := newTestReaper(t, lister, store, photos, choreProofPhotos, 0)
	result, err := r.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OrphansDeleted[domain.PhotoClassChoreProof] != 0 {
		t.Fatalf("OrphansDeleted[chore-proof] = %d, want 0 — the album row's cross-prefix reference must protect the object", result.OrphansDeleted[domain.PhotoClassChoreProof])
	}
	if len(store.deleted) != 0 {
		t.Fatalf("store.deleted = %v, want none", store.deleted)
	}
}

// TestReaperRetentionIsScopedToBackend covers the NES-133/149 fix directly:
// a reaper instance bound to ONE backend (StorageBackendLocal, via
// newTestReaper) must only apply retention to chore-proof rows STAMPED
// WITH that backend — an equally-old row stamped with a DIFFERENT backend
// must survive, since deleting it here would strand its object with no
// same-backend reaper instance able to ever reclaim it.
func TestReaperRetentionIsScopedToBackend(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	retention := 30 * 24 * time.Hour

	choreProofPhotos := newFakeTaskInstancePhotoRepo()
	oldLocal := &domain.TaskInstancePhoto{ID: domain.NewTaskInstancePhotoID(), StorageRef: "households/hh/chore-photos/aa/old-local.jpg", UploadedAt: now.Add(-40 * 24 * time.Hour), StorageBackend: domain.StorageBackendLocal}
	oldS3 := &domain.TaskInstancePhoto{ID: domain.NewTaskInstancePhotoID(), StorageRef: "households/hh/chore-photos/bb/old-s3.jpg", UploadedAt: now.Add(-40 * 24 * time.Hour), StorageBackend: domain.StorageBackendS3}
	choreProofPhotos.created = append(choreProofPhotos.created, oldLocal, oldS3)

	photos := newFakePhotoRepo()
	lister := &fakeObjectLister{}
	store := &fakePhotoStore{}

	// newTestReaper binds the reaper to StorageBackendLocal.
	r := newTestReaper(t, lister, store, photos, choreProofPhotos, retention)
	result, err := r.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RetentionRowsDeleted != 1 {
		t.Fatalf("RetentionRowsDeleted = %d, want 1 (only the local-backend row)", result.RetentionRowsDeleted)
	}
	if len(choreProofPhotos.created) != 1 || choreProofPhotos.created[0] != oldS3 {
		t.Fatalf("retention pass did not leave exactly the untouched s3-backend row: %v", choreProofPhotos.created)
	}
}

// TestReaperDryRunModelsRetentionCascadeIntoOrphanPreview covers the
// NES-133/149 DryRun fidelity fix: a chore-proof row old enough for
// retention to remove, whose object is ALSO past the grace window, must
// appear in the SAME preview's orphan-would-delete list — a naive preview
// built from the CURRENT (pre-retention) referenced set would still see
// the row "referencing" its object and never report it, even though the
// very next real Run (retention, then the orphan sweep observing the row
// genuinely gone) would reap it in that one call.
func TestReaperDryRunModelsRetentionCascadeIntoOrphanPreview(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * testGraceWindow)
	retention := 30 * 24 * time.Hour

	hh := household.NewHouseholdID()
	doomedRef := domain.StorageRef("households/" + hh.String() + "/chore-photos/aa/doomed.jpg")
	doomedRow := &domain.TaskInstancePhoto{
		ID: domain.NewTaskInstancePhotoID(), StorageRef: doomedRef,
		UploadedAt: now.Add(-40 * 24 * time.Hour), StorageBackend: domain.StorageBackendLocal,
	}
	choreProofPhotos := newFakeTaskInstancePhotoRepo()
	choreProofPhotos.created = append(choreProofPhotos.created, doomedRow)

	lister := &fakeObjectLister{objects: map[domain.PhotoClass][]domain.ObjectInfo{
		domain.PhotoClassChoreProof: {{Key: doomedRef, LastModified: old}},
	}}
	store := &fakePhotoStore{}
	photos := newFakePhotoRepo()

	r := newTestReaper(t, lister, store, photos, choreProofPhotos, retention)
	preview, err := r.DryRun(context.Background(), now)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if preview.RetentionRowsWouldDelete != 1 {
		t.Fatalf("RetentionRowsWouldDelete = %d, want 1", preview.RetentionRowsWouldDelete)
	}
	got := preview.OrphansWouldDelete[domain.PhotoClassChoreProof]
	if len(got) != 1 || got[0] != doomedRef {
		t.Fatalf("OrphansWouldDelete[chore-proof] = %v, want exactly [%s] — the preview must model retention's cascading effect on the SAME Run call's orphan sweep", got, doomedRef)
	}

	// A subsequent real Run finds exactly the same work the preview
	// promised: retention deletes the row, then the orphan sweep — now
	// seeing the row genuinely gone — reaps the object.
	result, err := r.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RetentionRowsDeleted != 1 {
		t.Fatalf("Run RetentionRowsDeleted = %d, want 1 (matching the preview)", result.RetentionRowsDeleted)
	}
	if result.OrphansDeleted[domain.PhotoClassChoreProof] != 1 {
		t.Fatalf("Run OrphansDeleted[chore-proof] = %d, want 1 (matching the preview)", result.OrphansDeleted[domain.PhotoClassChoreProof])
	}
}

// TestReaperDryRunRetentionDoesNotOrphanSharedRefWithSurvivingRow covers
// dryRunReferencedRefs' dedup-safety nuance: two chore-proof rows can
// share one StorageRef (content-addressed dedup — see domain.PhotoClass's
// dedup note). When only ONE of the pair is old enough for retention to
// remove while the OTHER survives, the shared ref must still count as
// referenced in the preview — a plain set-subtraction (rather than the
// occurrence-counted subtraction this fix implements) would wrongly report
// the object as an orphan candidate even though the surviving row still
// needs it.
func TestReaperDryRunRetentionDoesNotOrphanSharedRefWithSurvivingRow(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * testGraceWindow)
	retention := 30 * 24 * time.Hour

	hh := household.NewHouseholdID()
	sharedRef := domain.StorageRef("households/" + hh.String() + "/chore-photos/aa/shared.jpg")
	doomed := &domain.TaskInstancePhoto{ID: domain.NewTaskInstancePhotoID(), StorageRef: sharedRef, UploadedAt: now.Add(-40 * 24 * time.Hour), StorageBackend: domain.StorageBackendLocal}
	surviving := &domain.TaskInstancePhoto{ID: domain.NewTaskInstancePhotoID(), StorageRef: sharedRef, UploadedAt: now.Add(-5 * 24 * time.Hour), StorageBackend: domain.StorageBackendLocal}
	choreProofPhotos := newFakeTaskInstancePhotoRepo()
	choreProofPhotos.created = append(choreProofPhotos.created, doomed, surviving)

	lister := &fakeObjectLister{objects: map[domain.PhotoClass][]domain.ObjectInfo{
		domain.PhotoClassChoreProof: {{Key: sharedRef, LastModified: old}},
	}}
	store := &fakePhotoStore{}
	photos := newFakePhotoRepo()

	r := newTestReaper(t, lister, store, photos, choreProofPhotos, retention)
	preview, err := r.DryRun(context.Background(), now)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if preview.RetentionRowsWouldDelete != 1 {
		t.Fatalf("RetentionRowsWouldDelete = %d, want 1 (only the doomed row)", preview.RetentionRowsWouldDelete)
	}
	if got := preview.OrphansWouldDelete[domain.PhotoClassChoreProof]; len(got) != 0 {
		t.Fatalf("OrphansWouldDelete[chore-proof] = %v, want none: the surviving row still references the shared object", got)
	}
}

// TestReaperPropagatesListerError ensures a lister failure aborts the Run
// with a wrapped error rather than silently treating it as "no objects."
func TestReaperPropagatesListerError(t *testing.T) {
	lister := &fakeObjectLister{listErr: errors.New("bucket unreachable")}
	store := &fakePhotoStore{}
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	r := newTestReaper(t, lister, store, photos, choreProofPhotos, 0)
	if _, err := r.Run(context.Background(), time.Now()); err == nil {
		t.Fatal("Run should have failed when the lister errors")
	}
}
