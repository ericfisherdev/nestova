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
	r, err := app.NewReaperService(lister, store, photos, choreProofPhotos, testGraceWindow, retention)
	if err != nil {
		t.Fatalf("NewReaperService: %v", err)
	}
	return r
}

// TestNewReaperServiceValidatesDependencies covers the nil-dependency and
// non-positive-graceWindow guards.
func TestNewReaperServiceValidatesDependencies(t *testing.T) {
	lister := &fakeObjectLister{}
	store := &fakePhotoStore{}
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	cases := []struct {
		name   string
		lister domain.ObjectLister
		store  domain.PhotoStore
		photos domain.PhotoRepository
		cpp    domain.TaskInstancePhotoRepository
		grace  time.Duration
	}{
		{"nil lister", nil, store, photos, choreProofPhotos, testGraceWindow},
		{"nil store", lister, nil, photos, choreProofPhotos, testGraceWindow},
		{"nil photos repo", lister, store, nil, choreProofPhotos, testGraceWindow},
		{"nil chore-proof repo", lister, store, photos, nil, testGraceWindow},
		{"zero grace window", lister, store, photos, choreProofPhotos, 0},
		{"negative grace window", lister, store, photos, choreProofPhotos, -time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := app.NewReaperService(tc.lister, tc.store, tc.photos, tc.cpp, tc.grace, 0); err == nil {
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
	referenced := &domain.Photo{ID: domain.NewPhotoID(), HouseholdID: hh, StorageRef: domain.StorageRef("households/" + hh.String() + "/photos/aa/referenced.jpg")}
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
	restored := &domain.Photo{ID: domain.NewPhotoID(), HouseholdID: hh, StorageRef: ref}
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
	old := &domain.TaskInstancePhoto{ID: domain.NewTaskInstancePhotoID(), StorageRef: "households/hh/chore-photos/aa/old.jpg", UploadedAt: now.Add(-40 * 24 * time.Hour)}
	fresh := &domain.TaskInstancePhoto{ID: domain.NewTaskInstancePhotoID(), StorageRef: "households/hh/chore-photos/bb/fresh.jpg", UploadedAt: now.Add(-5 * 24 * time.Hour)}
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
	old := &domain.TaskInstancePhoto{ID: domain.NewTaskInstancePhotoID(), StorageRef: "households/hh/chore-photos/aa/old.jpg", UploadedAt: now.Add(-400 * 24 * time.Hour)}
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
