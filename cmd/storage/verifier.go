package main

import (
	"context"
	"errors"
	"fmt"

	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
)

// verifiedClasses is every domain.PhotoClass `storage verify` checks —
// mirrors migratableClasses/media/app's reapedClasses scope exactly (album
// and chore-proof).
var verifiedClasses = []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum, mediadomain.PhotoClassChoreProof}

// classVerifyResult is one class's S3-side findings: DATA-LOSS rows whose
// s3-backend StorageRef has no matching bucket object, informational
// reaper-candidate objects the bucket holds with no referencing row, and
// cross-prefix rows whose OWN storage_ref embeds a DIFFERENT class prefix
// than the table it lives in (a data-integrity bug, not data loss by
// itself, but a strong signal something upstream — most likely a previous
// migrate run — wrote the wrong key).
type classVerifyResult struct {
	Class             mediadomain.PhotoClass
	RowsWithoutObject []mediadomain.StorageRef
	ObjectsWithoutRow []mediadomain.StorageRef
	CrossPrefixRows   []mediadomain.StorageRef
}

// localVerifyResult is one class's local-side finding: LOCAL-backend rows
// whose file is missing from MEDIA_ROOT — the same data-loss alarm as an
// s3-backend row missing its object, just for the other backend.
type localVerifyResult struct {
	Class        mediadomain.PhotoClass
	MissingFiles []mediadomain.StorageRef
}

// verifyResult is the full report from one VerifierService.Verify call.
type verifyResult struct {
	S3    []classVerifyResult
	Local []localVerifyResult
}

// HasDataLoss reports whether result contains any DATA-LOSS finding — a
// row's bytes were not found under EITHER backend — which is `storage
// verify`'s exit-code-1 trigger (see cmd/storage's verify command doc for
// the full 0/1/2 exit code contract). Cross-prefix findings and
// objects-without-row are reported but do NOT set this: the former is a
// data-integrity anomaly (still readable, just misfiled), the latter is
// purely a reaper candidate (nothing is missing, something is merely
// unreferenced).
func (r verifyResult) HasDataLoss() bool {
	for _, c := range r.S3 {
		if len(c.RowsWithoutObject) > 0 {
			return true
		}
	}
	for _, l := range r.Local {
		if len(l.MissingFiles) > 0 {
			return true
		}
	}
	return false
}

// verifier cross-checks the photo/task_instance_photo tables against the
// LOCAL filesystem and the S3 bucket's actual contents (NES-133's `storage
// verify` command). It lives in cmd/storage for the identical reason
// photoMigrator does (see that type's doc): ClassOfKey, the cross-prefix
// check's key parser, lives in internal/media/adapter, which
// internal/media/app is forbidden from importing.
type verifier struct {
	localStore       mediadomain.PhotoStore
	lister           mediadomain.ObjectLister
	photos           mediadomain.PhotoRepository
	choreProofPhotos mediadomain.TaskInstancePhotoRepository
}

// newVerifier constructs a verifier, requiring s3Store to additionally
// implement domain.ObjectLister (S3PhotoStore is the only adapter that does
// today).
func newVerifier(localStore, s3Store mediadomain.PhotoStore, photos mediadomain.PhotoRepository, choreProofPhotos mediadomain.TaskInstancePhotoRepository) (*verifier, error) {
	switch {
	case localStore == nil:
		return nil, fmt.Errorf("storage: verifier requires a non-nil local PhotoStore")
	case s3Store == nil:
		return nil, fmt.Errorf("storage: verifier requires a non-nil S3 PhotoStore")
	case photos == nil:
		return nil, fmt.Errorf("storage: verifier requires a non-nil PhotoRepository")
	case choreProofPhotos == nil:
		return nil, fmt.Errorf("storage: verifier requires a non-nil TaskInstancePhotoRepository")
	}
	lister, ok := s3Store.(mediadomain.ObjectLister)
	if !ok {
		return nil, fmt.Errorf("storage: S3 PhotoStore does not support ObjectLister")
	}
	return &verifier{localStore: localStore, lister: lister, photos: photos, choreProofPhotos: choreProofPhotos}, nil
}

// Verify runs every class's S3 cross-check plus the local file-existence
// check, and returns the combined report.
//
// The S3 cross-check is deliberately NOT "one class at a time, checked only
// against that same class's own listing": a cross-prefix row (see
// classVerifyResult's doc) references an object filed under a DIFFERENT
// class's prefix than the table the row lives in, by definition — checking
// existence/reference within just one class's own listing would therefore
// double-misclassify it: the row would be reported as RowsWithoutObject in
// its OWN class (the object is invisible to that class's own ListObjects,
// since it lives under the OTHER prefix) AND the object it actually points
// at would be reported as ObjectsWithoutRow in THAT other class (no row of
// that class's OWN table references it). Verify avoids this by collecting
// every class's row refs and bucket objects into GLOBAL sets first, then
// classifying each class's own rows/objects against those global sets —
// existence and reference are genuinely global facts about the bucket and
// the database, only the CrossPrefixRows classification itself is anchored
// to a row's OWNING class (the table it is actually persisted in).
func (v *verifier) Verify(ctx context.Context) (verifyResult, error) {
	objectsByClass := make(map[mediadomain.PhotoClass][]mediadomain.ObjectInfo, len(verifiedClasses))
	rowRefsByClass := make(map[mediadomain.PhotoClass][]mediadomain.StorageRef, len(verifiedClasses))
	globalObjectSet := make(map[mediadomain.StorageRef]struct{})
	globalRowSet := make(map[mediadomain.StorageRef]struct{})

	for _, class := range verifiedClasses {
		objects, err := v.lister.ListObjects(ctx, class)
		if err != nil {
			return verifyResult{}, fmt.Errorf("list bucket objects for class %s: %w", class, err)
		}
		objectsByClass[class] = objects
		for _, obj := range objects {
			globalObjectSet[obj.Key] = struct{}{}
		}

		rowRefs, err := v.rowRefs(ctx, class, mediadomain.StorageBackendS3)
		if err != nil {
			return verifyResult{}, fmt.Errorf("list s3-backend refs for class %s: %w", class, err)
		}
		rowRefsByClass[class] = rowRefs
		for _, ref := range rowRefs {
			globalRowSet[ref] = struct{}{}
		}
	}

	var result verifyResult
	for _, class := range verifiedClasses {
		result.S3 = append(result.S3, classifyS3(class, rowRefsByClass[class], objectsByClass[class], globalObjectSet, globalRowSet))

		lr, err := v.verifyLocalClass(ctx, class)
		if err != nil {
			return verifyResult{}, err
		}
		result.Local = append(result.Local, lr)
	}
	return result, nil
}

// classifyS3 builds class's classVerifyResult from ITS OWN s3-backend rows
// (rowRefs) and ITS OWN bucket objects (objects, i.e. ListObjects(class)'s
// result — everything filed under class's own prefix), but checks
// existence/reference against the GLOBAL sets Verify built across every
// class — see Verify's doc for why that global scope is required to avoid
// double-misclassifying a cross-prefix row. CrossPrefixRows is still keyed
// by class alone: it is precisely "a row belonging to this class whose OWN
// ref embeds a DIFFERENT class's prefix."
func classifyS3(class mediadomain.PhotoClass, rowRefs []mediadomain.StorageRef, objects []mediadomain.ObjectInfo, globalObjectSet, globalRowSet map[mediadomain.StorageRef]struct{}) classVerifyResult {
	result := classVerifyResult{Class: class}
	for _, ref := range rowRefs {
		if _, ok := globalObjectSet[ref]; !ok {
			result.RowsWithoutObject = append(result.RowsWithoutObject, ref)
		}
		if refClass, ok := mediaadapter.ClassOfKey(ref.String()); ok && refClass != class {
			result.CrossPrefixRows = append(result.CrossPrefixRows, ref)
		}
	}
	for _, obj := range objects {
		if _, ok := globalRowSet[obj.Key]; !ok {
			result.ObjectsWithoutRow = append(result.ObjectsWithoutRow, obj.Key)
		}
	}
	return result
}

// verifyLocalClass checks every LOCAL-backend row of class for a missing
// file on disk — the same data-loss alarm as an s3-backend row's missing
// object, checked via the ordinary domain.PhotoStore.Open contract
// (ErrPhotoNotFound) rather than a dedicated existence port: LocalPhotoStore
// never buffers anything into memory just to open a file handle, so this is
// cheap even though it is not the same "list once" shape verifyClass uses
// against S3.
func (v *verifier) verifyLocalClass(ctx context.Context, class mediadomain.PhotoClass) (localVerifyResult, error) {
	result := localVerifyResult{Class: class}
	refs, err := v.rowRefs(ctx, class, mediadomain.StorageBackendLocal)
	if err != nil {
		return localVerifyResult{}, fmt.Errorf("list local-backend refs for class %s: %w", class, err)
	}
	for _, ref := range refs {
		reader, err := v.localStore.Open(ctx, ref)
		if err != nil {
			if errors.Is(err, mediadomain.ErrPhotoNotFound) {
				result.MissingFiles = append(result.MissingFiles, ref)
				continue
			}
			return localVerifyResult{}, fmt.Errorf("open local file %s: %w", ref, err)
		}
		_ = reader.Close()
	}
	return result, nil
}

// rowRefs dispatches to the repository that owns class, mirroring
// media/app.ReaperService.referencedRefs' identical class switch.
func (v *verifier) rowRefs(ctx context.Context, class mediadomain.PhotoClass, backend mediadomain.StorageBackend) ([]mediadomain.StorageRef, error) {
	switch class {
	case mediadomain.PhotoClassAlbum:
		return v.photos.ListAllStorageRefs(ctx, backend)
	case mediadomain.PhotoClassChoreProof:
		return v.choreProofPhotos.ListAllStorageRefs(ctx, backend)
	default:
		return nil, fmt.Errorf("storage: verify does not support class %s", class)
	}
}
