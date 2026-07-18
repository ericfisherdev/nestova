package main

import (
	"context"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
)

func newTestVerifier(t *testing.T, local *fakeLocalStore, target *fakeTargetStore, photos *fakePhotoRepo, choreProofPhotos *fakeTaskInstancePhotoRepo) *verifier {
	t.Helper()
	target.classOfKeyFn = mediaadapter.ClassOfKey
	v, err := newVerifier(local, target, photos, choreProofPhotos)
	if err != nil {
		t.Fatalf("newVerifier: %v", err)
	}
	return v
}

// TestVerifierCleanReportsNoFindings covers the clean case: every s3-backend
// row has a matching bucket object, every bucket object has a matching row,
// every local-backend row's file exists — HasDataLoss must be false.
func TestVerifierCleanReportsNoFindings(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	data := []byte("clean album photo")
	ref := canonicalRef(t, hh, mediadomain.PhotoClassAlbum, data)
	target.seed(ref, data, "image/jpeg", time.Now())
	photos.seed(&mediadomain.Photo{
		ID: mediadomain.NewPhotoID(), HouseholdID: hh, StorageRef: ref,
		ContentHash: sha256Hex(data), SizeBytes: int64(len(data)), ContentType: "image/jpeg",
		StorageBackend: mediadomain.StorageBackendS3,
	})

	localData := []byte("clean local-backend chore-proof photo")
	localRef := canonicalRef(t, hh, mediadomain.PhotoClassChoreProof, localData)
	local.seed(localRef, localData, "image/jpeg")
	choreProofPhotos.seed(&mediadomain.TaskInstancePhoto{
		ID: mediadomain.NewTaskInstancePhotoID(), HouseholdID: hh, TaskInstanceID: mediadomain.TaskInstanceID(household.NewHouseholdID()),
		Kind: mediadomain.PhotoKindBefore, StorageRef: localRef, ContentHash: sha256Hex(localData),
		SizeBytes: int64(len(localData)), ContentType: "image/jpeg", TakenAt: time.Now().UTC(),
		StorageBackend: mediadomain.StorageBackendLocal,
	})

	v := newTestVerifier(t, local, target, photos, choreProofPhotos)
	result, err := v.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.HasDataLoss() {
		t.Fatalf("HasDataLoss() = true, want false: %+v", result)
	}
	for _, c := range result.S3 {
		if len(c.RowsWithoutObject) != 0 || len(c.ObjectsWithoutRow) != 0 || len(c.CrossPrefixRows) != 0 {
			t.Fatalf("class %s has unexpected findings: %+v", c.Class, c)
		}
	}
	for _, l := range result.Local {
		if len(l.MissingFiles) != 0 {
			t.Fatalf("class %s has unexpected missing local files: %+v", l.Class, l)
		}
	}
}

// TestVerifierFlagsRowWithoutObject covers AC3: an s3-backend row whose
// object was deleted directly from the bucket is a DATA-LOSS finding
// (rows-without-object) and sets HasDataLoss.
func TestVerifierFlagsRowWithoutObject(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	data := []byte("row references this, but the object is gone")
	ref := canonicalRef(t, hh, mediadomain.PhotoClassAlbum, data)
	// Deliberately NOT seeded into target: simulates "delete an object
	// behind a row" (an operator or bug deleted the S3 object directly).
	photos.seed(&mediadomain.Photo{
		ID: mediadomain.NewPhotoID(), HouseholdID: hh, StorageRef: ref,
		ContentHash: sha256Hex(data), SizeBytes: int64(len(data)), ContentType: "image/jpeg",
		StorageBackend: mediadomain.StorageBackendS3,
	})

	v := newTestVerifier(t, local, target, photos, choreProofPhotos)
	result, err := v.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !result.HasDataLoss() {
		t.Fatal("HasDataLoss() = false, want true")
	}
	found := false
	for _, c := range result.S3 {
		if c.Class != mediadomain.PhotoClassAlbum {
			continue
		}
		for _, r := range c.RowsWithoutObject {
			if r == ref {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected ref %s in album RowsWithoutObject", ref)
	}
}

// TestVerifierFlagsObjectWithoutRow covers AC3's informational half: a
// bucket object with no referencing row is a reaper candidate
// (objects-without-row), but does NOT set HasDataLoss (nothing is missing —
// something is merely unreferenced).
func TestVerifierFlagsObjectWithoutRow(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	orphanData := []byte("orphaned object, no row references it")
	orphanRef := canonicalRef(t, hh, mediadomain.PhotoClassAlbum, orphanData)
	target.seed(orphanRef, orphanData, "image/jpeg", time.Now())

	v := newTestVerifier(t, local, target, photos, choreProofPhotos)
	result, err := v.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.HasDataLoss() {
		t.Fatal("HasDataLoss() = true, want false: an unreferenced object is not data loss")
	}
	found := false
	for _, c := range result.S3 {
		if c.Class != mediadomain.PhotoClassAlbum {
			continue
		}
		for _, r := range c.ObjectsWithoutRow {
			if r == orphanRef {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected ref %s in album ObjectsWithoutRow", orphanRef)
	}
}

// TestVerifierFlagsCrossPrefixRow covers AC3's cross-prefix check AND its
// regression guard: an album row referencing an object that genuinely
// EXISTS (just under the wrong, chore-proof prefix) must be reported ONLY
// as a CrossPrefixRows finding — never also as RowsWithoutObject (the
// object is not missing, it's just filed under the other class's own
// listing) and never as ObjectsWithoutRow under the chore-proof class (a
// row DOES reference it, just from the album table) — and HasDataLoss must
// stay false throughout. Checking existence/reference against only the
// OWNING class's own listing (rather than a global view across every
// class) would double-misclassify this exact scenario.
func TestVerifierFlagsCrossPrefixRow(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	data := []byte("album row misfiled under the chore-proof prefix")
	// Deliberately build the key under the WRONG class for an album row.
	misfiledRef, err := mediaadapter.BuildStorageKey(hh, mediadomain.PhotoClassChoreProof, sha256Hex(data), "jpg")
	if err != nil {
		t.Fatalf("BuildStorageKey: %v", err)
	}
	target.seed(mediadomain.StorageRef(misfiledRef), data, "image/jpeg", time.Now())
	photos.seed(&mediadomain.Photo{
		ID: mediadomain.NewPhotoID(), HouseholdID: hh, StorageRef: mediadomain.StorageRef(misfiledRef),
		ContentHash: sha256Hex(data), SizeBytes: int64(len(data)), ContentType: "image/jpeg",
		StorageBackend: mediadomain.StorageBackendS3,
	})

	v := newTestVerifier(t, local, target, photos, choreProofPhotos)
	result, err := v.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.HasDataLoss() {
		t.Fatalf("HasDataLoss() = true, want false: the object exists, it's just filed under the wrong prefix — %+v", result)
	}

	found := false
	for _, c := range result.S3 {
		switch c.Class {
		case mediadomain.PhotoClassAlbum:
			if len(c.RowsWithoutObject) != 0 {
				t.Fatalf("album RowsWithoutObject = %v, want none: the object genuinely exists (just under the wrong prefix)", c.RowsWithoutObject)
			}
			for _, r := range c.CrossPrefixRows {
				if r.String() == misfiledRef {
					found = true
				}
			}
		case mediadomain.PhotoClassChoreProof:
			if len(c.ObjectsWithoutRow) != 0 {
				t.Fatalf("chore-proof ObjectsWithoutRow = %v, want none: an album row DOES reference this object, just from the other table", c.ObjectsWithoutRow)
			}
		}
	}
	if !found {
		t.Fatalf("expected ref %s in album CrossPrefixRows", misfiledRef)
	}
}

// TestVerifierFlagsMissingLocalFile covers the local-side data-loss alarm:
// a local-backend row whose file is missing from the local store.
func TestVerifierFlagsMissingLocalFile(t *testing.T) {
	local := newFakeLocalStore()
	target := newFakeTargetStore()
	photos := newFakePhotoRepo()
	choreProofPhotos := newFakeTaskInstancePhotoRepo()

	hh := household.NewHouseholdID()
	data := []byte("local row, but the file is missing")
	ref := canonicalRef(t, hh, mediadomain.PhotoClassAlbum, data)
	// Deliberately NOT seeded into local: simulates a missing file.
	photos.seed(&mediadomain.Photo{
		ID: mediadomain.NewPhotoID(), HouseholdID: hh, StorageRef: ref,
		ContentHash: sha256Hex(data), SizeBytes: int64(len(data)), ContentType: "image/jpeg",
		StorageBackend: mediadomain.StorageBackendLocal,
	})

	v := newTestVerifier(t, local, target, photos, choreProofPhotos)
	result, err := v.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !result.HasDataLoss() {
		t.Fatal("HasDataLoss() = false, want true")
	}
	found := false
	for _, l := range result.Local {
		if l.Class != mediadomain.PhotoClassAlbum {
			continue
		}
		for _, r := range l.MissingFiles {
			if r == ref {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected ref %s in album MissingFiles", ref)
	}
}
