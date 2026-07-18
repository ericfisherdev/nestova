package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
)

// migrateBatchSize bounds how many local-backend rows photoMigrator loads
// per ListByBackend page — the resumability requirement (NES-133 AC1) in
// bounded memory: a large photo library is walked in fixed-size batches
// rather than loaded all at once, and interrupting the process mid-batch
// loses at most the in-flight photo's own work (each photo is committed to
// s3-backend, one row at a time — see migrateAlbumClass's doc).
const migrateBatchSize = 100

// migratableClasses is every domain.PhotoClass `storage migrate` moves —
// mirrors media/app's reapedClasses scope exactly: album and chore-proof.
// PhotoClassRewardImage is excluded; it only reserves a key namespace (see
// that constant's doc) and has no upload path, so it never has local rows
// to migrate.
var migratableClasses = []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum, mediadomain.PhotoClassChoreProof}

// errHashMismatch is returned by photoMigrator.migrateBytes when a row's
// local bytes no longer match its recorded content hash — NES-133 AC2: the
// row's flip is aborted (it keeps serving from local) and the mismatch is
// counted/reported, but the migrator continues with the next photo rather
// than aborting the whole run.
var errHashMismatch = errors.New("storage: local bytes do not match the row's recorded content hash")

// errTargetIntegrityFailed is returned by migrateBytes' post-upload
// verification (and sweepOneLeftoverLocalFile's pre-delete check) when the
// TARGET object cannot be read back at all, or reads back with a hash that
// does not match the bytes verified locally. This is a DIFFERENT failure
// from errHashMismatch (which is about the LOCAL file no longer matching
// the row's recorded hash): errTargetIntegrityFailed covers the target
// side — a pre-existing object (content-addressed dedup: a different
// row's earlier upload) that turns out corrupt, an upload that silently
// truncated in transit, or an object deleted/altered directly in the
// bucket between the existence check and this verification.
// ObjectExister.ObjectExists only proves an object is PRESENT at a key; it
// says nothing about whether its bytes are the bytes this migrator is
// about to trust — this sentinel is what closes that gap. Like
// errHashMismatch, a row hitting this keeps serving from local and the
// migrator continues with the next photo.
var errTargetIntegrityFailed = errors.New("storage: target object is missing or its bytes do not match the verified local hash")

// migrateOutcome classifies what happened to one row during a migrate pass —
// reported to photoMigrator's optional progress callback (see
// migrateProgress) so `storage migrate` can print per-photo/per-batch
// status without the migrator itself taking a logging dependency (mirroring
// how internal/media/app's services stay logger-free — logging is a
// composition-root/CLI concern in this codebase).
type migrateOutcome int

const (
	migrateOutcomeMigrated migrateOutcome = iota
	migrateOutcomeAlreadyMigrated
	migrateOutcomeHashMismatch
	migrateOutcomeTargetIntegrityFailed
	migrateOutcomeError
)

// String returns a human-readable label for o, used in progress output.
func (o migrateOutcome) String() string {
	switch o {
	case migrateOutcomeMigrated:
		return "migrated"
	case migrateOutcomeAlreadyMigrated:
		return "already migrated"
	case migrateOutcomeHashMismatch:
		return "hash mismatch"
	case migrateOutcomeTargetIntegrityFailed:
		return "target integrity failed"
	case migrateOutcomeError:
		return "error"
	default:
		return "unknown"
	}
}

// migrateProgress is reported once per row photoMigrator processes.
type migrateProgress struct {
	Class        mediadomain.PhotoClass
	Done         int
	Total        int
	Outcome      migrateOutcome
	Ref          mediadomain.StorageRef
	DeletedLocal bool
	Err          error
}

// migrateOptions configures one photoMigrator.Migrate call.
type migrateOptions struct {
	// Classes restricts the migration to the given classes; empty means
	// every class in migratableClasses (the --class flag's "unset" default).
	Classes []mediadomain.PhotoClass
	// DeleteLocal opts into deleting a photo's local file once its flip to
	// the target backend is verified, PLUS a second sweep pass over rows
	// ALREADY on the target backend (from a prior run that had DeleteLocal
	// off) whose local file was never cleaned up — see
	// photoMigrator.sweepLeftoverLocalFiles's doc for why this second pass
	// exists: it is what makes "migrate now, delete-local much later" (the
	// documented runbook) actually reclaim disk space on a later run.
	DeleteLocal bool
}

// migrateClassResult summarizes one class's migrate pass.
type migrateClassResult struct {
	Class          mediadomain.PhotoClass
	Migrated       int
	AlreadyDone    int
	HashMismatches int
	// TargetIntegrityFailures counts rows (from either the main migrate
	// pass or the --delete-local leftover sweep) whose TARGET object was
	// missing or failed the post-upload/pre-delete hash verification — see
	// errTargetIntegrityFailed's doc. Tracked separately from
	// HashMismatches: a hash mismatch means the LOCAL file is suspect: a
	// target integrity failure means the TARGET copy is suspect, a
	// meaningfully different problem for an operator to investigate.
	TargetIntegrityFailures int
	Errors                  int
	DeletedLocal            int
}

// migrateResult summarizes an entire Migrate call, one result per processed
// class.
type migrateResult struct {
	Classes []migrateClassResult
}

// HasFindings reports whether any class hit a hash mismatch, a target
// integrity failure, or a hard error — `storage migrate`'s nonzero-exit
// trigger.
func (r migrateResult) HasFindings() bool {
	for _, c := range r.Classes {
		if c.HashMismatches > 0 || c.TargetIntegrityFailures > 0 || c.Errors > 0 {
			return true
		}
	}
	return false
}

// photoMigrator moves LOCAL-backend photo bytes to an object-store backend
// and flips each row's storage_backend once the move is verified (NES-133).
// It lives in cmd/storage (the composition root), not internal/media/app:
// its core pipeline (migrateBytes) must derive a row's canonical
// content-addressed key via the SAME formula S3PhotoStore.Put uses
// (mediaadapter.BuildStorageKey/ExtensionForContentType), and
// internal/media/app is architecturally forbidden from importing
// internal/media/adapter (app depends on domain ports only — see
// internal/media/adapter/hexagonal_boundary_test.go for the enforced half
// of that boundary, and PhotoService/ReaperService's own logger-free,
// adapter-free shape for the established precedent). cmd/server's
// provisioning.go/media_storage.go already established that
// composition-root-adjacent orchestration logic like this belongs directly
// in the binary's own package, not a manufactured bounded-context service.
//
// Every dependency is still injected as a domain PORT (PhotoRepository,
// TaskInstancePhotoRepository, PhotoStore, ObjectExister, RawObjectWriter),
// so unit tests exercise this exactly like an app-layer service would —
// via fakes, never a real database or object store — mirroring
// cmd/server's own handler-test precedent (e.g. fakeMediaPhotoRepo).
type photoMigrator struct {
	localStore       mediadomain.PhotoStore
	targetStore      mediadomain.PhotoStore
	targetBackend    mediadomain.StorageBackend
	targetExister    mediadomain.ObjectExister
	targetWriter     mediadomain.RawObjectWriter
	photos           mediadomain.PhotoRepository
	choreProofPhotos mediadomain.TaskInstancePhotoRepository
	maxUploadBytes   int64
	// progress is called once per row processed; nil is a valid no-op (a
	// caller that only wants the final migrateResult, e.g. a unit test,
	// need not supply one).
	progress func(migrateProgress)
}

// newPhotoMigrator constructs a photoMigrator, requiring targetStore to
// additionally implement domain.ObjectExister and domain.RawObjectWriter
// (S3PhotoStore is the only adapter that does today — see those ports'
// docs) since the migrate pipeline's idempotent upload depends on both.
func newPhotoMigrator(
	localStore, targetStore mediadomain.PhotoStore,
	targetBackend mediadomain.StorageBackend,
	photos mediadomain.PhotoRepository,
	choreProofPhotos mediadomain.TaskInstancePhotoRepository,
	maxUploadBytes int64,
	progress func(migrateProgress),
) (*photoMigrator, error) {
	switch {
	case localStore == nil:
		return nil, errors.New("storage: photo migrator requires a non-nil local PhotoStore")
	case targetStore == nil:
		return nil, errors.New("storage: photo migrator requires a non-nil target PhotoStore")
	case !targetBackend.Valid():
		return nil, fmt.Errorf("storage: photo migrator requires a valid target StorageBackend, got %q", targetBackend)
	case photos == nil:
		return nil, errors.New("storage: photo migrator requires a non-nil PhotoRepository")
	case choreProofPhotos == nil:
		return nil, errors.New("storage: photo migrator requires a non-nil TaskInstancePhotoRepository")
	case maxUploadBytes <= 0:
		return nil, fmt.Errorf("storage: photo migrator requires a positive maxUploadBytes, got %d", maxUploadBytes)
	}
	exister, ok := targetStore.(mediadomain.ObjectExister)
	if !ok {
		return nil, fmt.Errorf("storage: target PhotoStore for backend %q does not support ObjectExister", targetBackend)
	}
	writer, ok := targetStore.(mediadomain.RawObjectWriter)
	if !ok {
		return nil, fmt.Errorf("storage: target PhotoStore for backend %q does not support RawObjectWriter", targetBackend)
	}
	return &photoMigrator{
		localStore: localStore, targetStore: targetStore, targetBackend: targetBackend,
		targetExister: exister, targetWriter: writer,
		photos: photos, choreProofPhotos: choreProofPhotos,
		maxUploadBytes: maxUploadBytes, progress: progress,
	}, nil
}

// Migrate runs one migrate pass over opts.Classes (every migratableClasses
// entry when empty), returning a per-class summary.
func (m *photoMigrator) Migrate(ctx context.Context, opts migrateOptions) (migrateResult, error) {
	classes := opts.Classes
	if len(classes) == 0 {
		classes = migratableClasses
	}
	var result migrateResult
	for _, class := range classes {
		var (
			cr  migrateClassResult
			err error
		)
		switch class {
		case mediadomain.PhotoClassAlbum:
			cr, err = m.migrateAlbumClass(ctx, opts.DeleteLocal)
		case mediadomain.PhotoClassChoreProof:
			cr, err = m.migrateChoreProofClass(ctx, opts.DeleteLocal)
		default:
			return migrateResult{}, fmt.Errorf("storage: migrate does not support class %s", class)
		}
		if err != nil {
			return migrateResult{}, err
		}
		result.Classes = append(result.Classes, cr)
	}
	return result, nil
}

// migrateAlbumClass walks every LOCAL-backend photo row in keyset-paginated
// batches (migrateBatchSize), migrating each one via migrateBytes and, on
// success, flipping the row with PhotoRepository.MigrateStorageBackend —
// re-listing ListByBackend(local, ...) from the SAME cursor after each
// batch is what makes an interrupted run resumable (NES-133 AC1): a row
// already flipped by a prior run no longer appears in this listing at all,
// so re-running Migrate simply picks up wherever it left off, without
// re-uploading or double-processing anything.
func (m *photoMigrator) migrateAlbumClass(ctx context.Context, deleteLocal bool) (migrateClassResult, error) {
	result := migrateClassResult{Class: mediadomain.PhotoClassAlbum}
	total, err := m.countLocal(ctx, m.photos)
	if err != nil {
		return migrateClassResult{}, fmt.Errorf("count local album photos: %w", err)
	}

	var (
		afterID   mediadomain.PhotoID
		processed int
	)
	for {
		rows, err := m.photos.ListByBackend(ctx, mediadomain.StorageBackendLocal, afterID, migrateBatchSize)
		if err != nil {
			return migrateClassResult{}, fmt.Errorf("list local album photos: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, photo := range rows {
			afterID = photo.ID
			processed++
			m.migrateOneAlbumPhoto(ctx, photo, deleteLocal, processed, total, &result)
		}
	}

	if deleteLocal {
		n, integrityFailures, err := m.sweepLeftoverLocalAlbumFiles(ctx)
		if err != nil {
			return migrateClassResult{}, fmt.Errorf("sweep leftover local album files: %w", err)
		}
		result.DeletedLocal += n
		result.TargetIntegrityFailures += integrityFailures
	}
	return result, nil
}

// migrateOneAlbumPhoto runs the per-photo pipeline for one album row and
// records its outcome onto result, reporting progress throughout.
func (m *photoMigrator) migrateOneAlbumPhoto(ctx context.Context, photo *mediadomain.Photo, deleteLocal bool, done, total int, result *migrateClassResult) {
	// originalRef is captured up front, before MigrateStorageBackend runs:
	// the delete-local check below MUST operate on the row's ORIGINAL local
	// ref (the actual local file's path), never whatever photo.StorageRef
	// holds after the flip — reading photo.StorageRef again after the flip
	// would rely on the injected PhotoRepository never mutating the struct
	// this call was given only an id for, which is an assumption this code
	// should not need to make.
	originalRef := photo.StorageRef

	migrated, err := m.migrateBytes(ctx, photo.HouseholdID, mediadomain.PhotoClassAlbum, originalRef, photo.ContentType, photo.ContentHash)
	if err != nil {
		if errors.Is(err, errHashMismatch) {
			result.HashMismatches++
			m.report(migrateProgress{Class: mediadomain.PhotoClassAlbum, Done: done, Total: total, Outcome: migrateOutcomeHashMismatch, Ref: originalRef})
			return
		}
		if errors.Is(err, errTargetIntegrityFailed) {
			result.TargetIntegrityFailures++
			m.report(migrateProgress{Class: mediadomain.PhotoClassAlbum, Done: done, Total: total, Outcome: migrateOutcomeTargetIntegrityFailed, Ref: originalRef, Err: err})
			return
		}
		result.Errors++
		m.report(migrateProgress{Class: mediadomain.PhotoClassAlbum, Done: done, Total: total, Outcome: migrateOutcomeError, Ref: originalRef, Err: err})
		return
	}

	// contentHash is passed unconditionally: PhotoRepository.MigrateStorageBackend
	// only backfills it when the row's OWN content_sha256 is currently NULL
	// (COALESCE), so passing it even for a row that already had a hash is a
	// harmless no-op (the two values are, by construction, already equal —
	// migrateBytes would have returned errHashMismatch above otherwise).
	flipped, err := m.photos.MigrateStorageBackend(ctx, photo.ID, migrated.ref, m.targetBackend, migrated.hash)
	if err != nil {
		result.Errors++
		m.report(migrateProgress{Class: mediadomain.PhotoClassAlbum, Done: done, Total: total, Outcome: migrateOutcomeError, Ref: originalRef, Err: err})
		return
	}
	if !flipped {
		result.AlreadyDone++
		m.report(migrateProgress{Class: mediadomain.PhotoClassAlbum, Done: done, Total: total, Outcome: migrateOutcomeAlreadyMigrated, Ref: originalRef})
		return
	}

	result.Migrated++
	deletedLocal := false
	if deleteLocal {
		ok, err := m.deleteLocalIfUnreferenced(ctx, originalRef)
		if err != nil {
			result.Errors++
			m.report(migrateProgress{Class: mediadomain.PhotoClassAlbum, Done: done, Total: total, Outcome: migrateOutcomeError, Ref: originalRef, Err: fmt.Errorf("delete local file: %w", err)})
		} else if ok {
			deletedLocal = true
			result.DeletedLocal++
		}
	}
	m.report(migrateProgress{Class: mediadomain.PhotoClassAlbum, Done: done, Total: total, Outcome: migrateOutcomeMigrated, Ref: migrated.ref, DeletedLocal: deletedLocal})
}

// migrateChoreProofClass is migrateAlbumClass's chore-proof counterpart —
// identical pipeline and resumability contract, sourced from
// TaskInstancePhotoRepository instead of PhotoRepository (see that method's
// doc for why the two tables are separate, structurally-incompatible Go
// types with no shared interface to loop over generically).
func (m *photoMigrator) migrateChoreProofClass(ctx context.Context, deleteLocal bool) (migrateClassResult, error) {
	result := migrateClassResult{Class: mediadomain.PhotoClassChoreProof}
	total, err := m.countLocal(ctx, m.choreProofPhotos)
	if err != nil {
		return migrateClassResult{}, fmt.Errorf("count local chore-proof photos: %w", err)
	}

	var (
		afterID   mediadomain.TaskInstancePhotoID
		processed int
	)
	for {
		rows, err := m.choreProofPhotos.ListByBackend(ctx, mediadomain.StorageBackendLocal, afterID, migrateBatchSize)
		if err != nil {
			return migrateClassResult{}, fmt.Errorf("list local chore-proof photos: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, photo := range rows {
			afterID = photo.ID
			processed++
			m.migrateOneChoreProofPhoto(ctx, photo, deleteLocal, processed, total, &result)
		}
	}

	if deleteLocal {
		n, integrityFailures, err := m.sweepLeftoverLocalChoreProofFiles(ctx)
		if err != nil {
			return migrateClassResult{}, fmt.Errorf("sweep leftover local chore-proof files: %w", err)
		}
		result.DeletedLocal += n
		result.TargetIntegrityFailures += integrityFailures
	}
	return result, nil
}

func (m *photoMigrator) migrateOneChoreProofPhoto(ctx context.Context, photo *mediadomain.TaskInstancePhoto, deleteLocal bool, done, total int, result *migrateClassResult) {
	// originalRef: see migrateOneAlbumPhoto's identical comment for why this
	// is captured up front rather than re-read from photo.StorageRef after
	// the flip.
	originalRef := photo.StorageRef

	migrated, err := m.migrateBytes(ctx, photo.HouseholdID, mediadomain.PhotoClassChoreProof, originalRef, photo.ContentType, photo.ContentHash)
	if err != nil {
		if errors.Is(err, errHashMismatch) {
			result.HashMismatches++
			m.report(migrateProgress{Class: mediadomain.PhotoClassChoreProof, Done: done, Total: total, Outcome: migrateOutcomeHashMismatch, Ref: originalRef})
			return
		}
		if errors.Is(err, errTargetIntegrityFailed) {
			result.TargetIntegrityFailures++
			m.report(migrateProgress{Class: mediadomain.PhotoClassChoreProof, Done: done, Total: total, Outcome: migrateOutcomeTargetIntegrityFailed, Ref: originalRef, Err: err})
			return
		}
		result.Errors++
		m.report(migrateProgress{Class: mediadomain.PhotoClassChoreProof, Done: done, Total: total, Outcome: migrateOutcomeError, Ref: originalRef, Err: err})
		return
	}

	flipped, err := m.choreProofPhotos.MigrateStorageBackend(ctx, photo.ID, migrated.ref, m.targetBackend)
	if err != nil {
		result.Errors++
		m.report(migrateProgress{Class: mediadomain.PhotoClassChoreProof, Done: done, Total: total, Outcome: migrateOutcomeError, Ref: originalRef, Err: err})
		return
	}
	if !flipped {
		result.AlreadyDone++
		m.report(migrateProgress{Class: mediadomain.PhotoClassChoreProof, Done: done, Total: total, Outcome: migrateOutcomeAlreadyMigrated, Ref: originalRef})
		return
	}

	result.Migrated++
	deletedLocal := false
	if deleteLocal {
		ok, err := m.deleteLocalIfUnreferenced(ctx, originalRef)
		if err != nil {
			result.Errors++
			m.report(migrateProgress{Class: mediadomain.PhotoClassChoreProof, Done: done, Total: total, Outcome: migrateOutcomeError, Ref: originalRef, Err: fmt.Errorf("delete local file: %w", err)})
		} else if ok {
			deletedLocal = true
			result.DeletedLocal++
		}
	}
	m.report(migrateProgress{Class: mediadomain.PhotoClassChoreProof, Done: done, Total: total, Outcome: migrateOutcomeMigrated, Ref: migrated.ref, DeletedLocal: deletedLocal})
}

// localCounter is the read surface migrateAlbumClass/migrateChoreProofClass
// share for countLocal's up-front progress total — both PhotoRepository and
// TaskInstancePhotoRepository already expose ListAllStorageRefs, so no new
// method is needed purely to count.
type localCounter interface {
	ListAllStorageRefs(ctx context.Context, backend mediadomain.StorageBackend) ([]mediadomain.StorageRef, error)
}

// countLocal returns how many local-backend rows repo currently holds, for
// migrateProgress's Total field ("batch progress logging... count
// done/total" — NES-133's ticket). A plain count via the existing
// ListAllStorageRefs, not a new dedicated repository method: this app's
// scale (a family's photo library) makes the extra listing cheap, and it
// avoids growing PhotoRepository/TaskInstancePhotoRepository's interface
// surface purely for a progress-bar denominator.
func (m *photoMigrator) countLocal(ctx context.Context, repo localCounter) (int, error) {
	refs, err := repo.ListAllStorageRefs(ctx, mediadomain.StorageBackendLocal)
	if err != nil {
		return 0, err
	}
	return len(refs), nil
}

// report invokes m.progress if the caller supplied one.
func (m *photoMigrator) report(p migrateProgress) {
	if m.progress != nil {
		m.progress(p)
	}
}

// migratedRef is migrateBytes' result: the canonical, class-namespaced
// target-backend key the bytes now live at, and the sha256 actually found
// in them.
type migratedRef struct {
	ref  mediadomain.StorageRef
	hash string
}

// migrateBytes runs NES-133's per-photo local-to-target pipeline: opens ref
// from the local store, streams it into a bounded in-memory buffer while
// hashing (mirroring internal/media/adapter's validateAndStage shape, but
// WITHOUT re-decoding the image — the bytes were already validated once, at
// original upload, and the ticket's AC explicitly calls for a hash
// re-verify only, not a second full validation pass), computes the
// canonical class-namespaced key via mediaadapter.BuildStorageKey — the
// SAME formula S3PhotoStore.Put uses, so a migrated object is
// indistinguishable from one Put would have written directly — and, unless
// an object already exists at that key (idempotent skip: content-addressed
// dedup means a DIFFERENT row referencing the same household+class+hash
// bytes may already have migrated it — see PhotoClass's dedup note),
// uploads the buffered bytes verbatim via RawObjectWriter.PutAt.
//
// When expectedHash is non-blank and does not match the bytes actually
// read, migrateBytes returns errHashMismatch (NES-133 AC2) instead of ever
// uploading — a corrupt/altered local file must never overwrite a
// previously-good object, or worse, silently replace it with bad bytes. A
// blank expectedHash (only possible for a legacy pre-NES-123 photo.Photo
// row — TaskInstancePhoto.ContentHash is NOT NULL from its first migration)
// skips the mismatch check entirely; the caller backfills content_sha256
// with the returned migratedRef.hash.
//
// Whether the object was JUST uploaded or found via the dedup skip above,
// migrateBytes downloads it back and re-hashes it before returning success
// (verifyTargetObject) — ObjectExister.ObjectExists only proves an object
// is PRESENT at the key, never that its bytes are trustworthy. Skipping
// this check would let a corrupt pre-existing object (e.g. a prior failed
// upload, or one damaged directly in the bucket) be silently accepted as
// "already migrated" — the row would flip to the target backend, and a
// later --delete-local could then remove the last INTACT copy, since
// nothing else would ever notice the target was bad. A missing or
// mismatched target returns errTargetIntegrityFailed instead.
func (m *photoMigrator) migrateBytes(ctx context.Context, householdID household.HouseholdID, class mediadomain.PhotoClass, ref mediadomain.StorageRef, contentType, expectedHash string) (migratedRef, error) {
	reader, err := m.localStore.Open(ctx, ref)
	if err != nil {
		return migratedRef{}, fmt.Errorf("open local photo %s: %w", ref, err)
	}
	defer func() { _ = reader.Close() }()

	ext, ok := mediaadapter.ExtensionForContentType(contentType)
	if !ok {
		return migratedRef{}, fmt.Errorf("storage: content type %q on %s is not a recognized image type", contentType, ref)
	}

	var buf bytes.Buffer
	hasher := sha256.New()
	// +1: enough to detect an oversize file (should never happen — Put
	// already enforced maxUploadBytes at original upload) without ever
	// buffering more than that in flight, mirroring validateAndStage's
	// identical cap.
	limited := io.LimitReader(reader, m.maxUploadBytes+1)
	written, err := io.Copy(io.MultiWriter(&buf, hasher), limited)
	if err != nil {
		return migratedRef{}, fmt.Errorf("read local photo %s: %w", ref, err)
	}
	if written > m.maxUploadBytes {
		return migratedRef{}, fmt.Errorf("storage: local photo %s is over %d bytes, exceeds the configured limit", ref, m.maxUploadBytes)
	}
	hash := hex.EncodeToString(hasher.Sum(nil))
	if expectedHash != "" && hash != expectedHash {
		return migratedRef{}, fmt.Errorf("%w: %s", errHashMismatch, ref)
	}

	key, err := mediaadapter.BuildStorageKey(householdID, class, hash, ext)
	if err != nil {
		return migratedRef{}, fmt.Errorf("build canonical storage key for %s: %w", ref, err)
	}
	canonicalRef := mediadomain.StorageRef(key)

	exists, err := m.targetExister.ObjectExists(ctx, canonicalRef)
	if err != nil {
		return migratedRef{}, fmt.Errorf("check object exists at %s: %w", canonicalRef, err)
	}
	if !exists {
		if err := m.targetWriter.PutAt(ctx, canonicalRef, contentType, bytes.NewReader(buf.Bytes())); err != nil {
			return migratedRef{}, fmt.Errorf("upload %s to %s: %w", ref, canonicalRef, err)
		}
	}
	if err := m.verifyTargetObject(ctx, canonicalRef, hash); err != nil {
		return migratedRef{}, err
	}
	return migratedRef{ref: canonicalRef, hash: hash}, nil
}

// verifyTargetObject downloads ref from the TARGET backend and confirms its
// bytes hash to expectedHash — see migrateBytes' doc for why this runs
// unconditionally after every upload-or-dedup-skip, and
// sweepOneLeftoverLocalFile's doc for the second call site (immediately
// before that pass would otherwise delete a local file). Wraps
// errTargetIntegrityFailed on ANY failure — the object is missing/
// unreadable (Open error) or its bytes don't match (hash mismatch) — since
// both mean the same thing to a caller: the target copy cannot be trusted
// yet, keep the local copy. Bounded by maxUploadBytes, mirroring
// migrateBytes' own local-read cap, so a corrupt/oversized object cannot
// cause unbounded memory use here either.
func (m *photoMigrator) verifyTargetObject(ctx context.Context, ref mediadomain.StorageRef, expectedHash string) error {
	reader, err := m.targetStore.Open(ctx, ref)
	if err != nil {
		return fmt.Errorf("%w: open target object %s: %v", errTargetIntegrityFailed, ref, err)
	}
	defer func() { _ = reader.Close() }()

	hasher := sha256.New()
	written, err := io.Copy(hasher, io.LimitReader(reader, m.maxUploadBytes+1))
	if err != nil {
		return fmt.Errorf("%w: read target object %s: %v", errTargetIntegrityFailed, ref, err)
	}
	if written > m.maxUploadBytes {
		return fmt.Errorf("%w: target object %s exceeds the configured size limit", errTargetIntegrityFailed, ref)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != expectedHash {
		return fmt.Errorf("%w: target object %s hash does not match the verified local hash", errTargetIntegrityFailed, ref)
	}
	return nil
}

// deleteLocalIfUnreferenced deletes ref's local file, but ONLY when no OTHER
// row — in either table, on the local backend — still references it
// (content-addressed dedup, see PhotoClass's doc: multiple rows in the same
// household can share one local object). It is the migrator's --delete-local
// safety check (NES-133 AC4): checking BOTH repositories, not just the
// caller's own table, is deliberate even though a cross-table collision on
// the SAME ref is structurally impossible (buildStorageKey namespaces by
// class) — the check is cheap and correct regardless, so there is no reason
// to rely on that invariant holding forever.
func (m *photoMigrator) deleteLocalIfUnreferenced(ctx context.Context, ref mediadomain.StorageRef) (bool, error) {
	referenced, err := m.photos.ExistsByStorageRef(ctx, ref, mediadomain.StorageBackendLocal)
	if err != nil {
		return false, fmt.Errorf("check remaining local album references to %s: %w", ref, err)
	}
	if !referenced {
		referenced, err = m.choreProofPhotos.ExistsByStorageRef(ctx, ref, mediadomain.StorageBackendLocal)
		if err != nil {
			return false, fmt.Errorf("check remaining local chore-proof references to %s: %w", ref, err)
		}
	}
	if referenced {
		return false, nil
	}
	if err := m.localStore.Delete(ctx, ref); err != nil {
		return false, fmt.Errorf("delete local file %s: %w", ref, err)
	}
	return true, nil
}

// sweepLeftoverLocalAlbumFiles is the --delete-local "much later" cleanup
// pass (see migrateOptions.DeleteLocal's doc): it walks every row ALREADY
// on the target backend (not local — those were handled, or will be, by
// the main migrateAlbumClass loop) and deletes its local file if one is
// still sitting on disk from a PRIOR run that migrated the row without
// --delete-local. Each candidate is re-hashed from the local file and
// checked against the row's OWN content_sha256 before deletion — NES-133
// AC4's "ONLY after per-file hash verification" requirement applies to this
// pass exactly as much as the main loop's inline delete.
//
// KNOWN GAP: this pass looks a row's local file up BY THE ROW'S CURRENT
// storage_ref — which, for the overwhelming majority of rows, is the exact
// same canonical key the local file always lived at (buildStorageKey is,
// and always has been, LocalPhotoStore.Put's own key formula). For the rare
// legacy row whose ORIGINAL local ref predates the class segment (see
// LocalPhotoStore's doc) and was therefore NORMALIZED to a different
// canonical key at flip time (migrateBytes/BuildStorageKey), this pass
// cannot find that row's original-path local file: the mapping from the
// new canonical ref back to the old on-disk path is not retained anywhere
// once MigrateStorageBackend overwrites storage_ref. Such a file is not a
// safety issue (it is simply never deleted, an over-retention, not a
// data-loss one) — `storage verify`'s local check would not flag it either,
// since it is an orphan, not a row pointing at a missing file. Left
// unaddressed because it only affects historical, pre-NES-131 refs; a
// future ticket could close it by recording the original ref before the
// flip, or by having `storage verify`/`storage reap` walk MEDIA_ROOT
// directly.
func (m *photoMigrator) sweepLeftoverLocalAlbumFiles(ctx context.Context) (deleted, targetIntegrityFailures int, err error) {
	var afterID mediadomain.PhotoID
	for {
		rows, err := m.photos.ListByBackend(ctx, m.targetBackend, afterID, migrateBatchSize)
		if err != nil {
			return deleted, targetIntegrityFailures, fmt.Errorf("list target-backend album photos: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, photo := range rows {
			afterID = photo.ID
			ok, integrityFailed, err := m.sweepOneLeftoverLocalFile(ctx, photo.StorageRef, photo.ContentHash)
			if err != nil {
				return deleted, targetIntegrityFailures, err
			}
			if ok {
				deleted++
			}
			if integrityFailed {
				targetIntegrityFailures++
			}
		}
	}
	return deleted, targetIntegrityFailures, nil
}

// sweepLeftoverLocalChoreProofFiles is sweepLeftoverLocalAlbumFiles' chore-
// proof counterpart.
func (m *photoMigrator) sweepLeftoverLocalChoreProofFiles(ctx context.Context) (deleted, targetIntegrityFailures int, err error) {
	var afterID mediadomain.TaskInstancePhotoID
	for {
		rows, err := m.choreProofPhotos.ListByBackend(ctx, m.targetBackend, afterID, migrateBatchSize)
		if err != nil {
			return deleted, targetIntegrityFailures, fmt.Errorf("list target-backend chore-proof photos: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, photo := range rows {
			afterID = photo.ID
			ok, integrityFailed, err := m.sweepOneLeftoverLocalFile(ctx, photo.StorageRef, photo.ContentHash)
			if err != nil {
				return deleted, targetIntegrityFailures, err
			}
			if ok {
				deleted++
			}
			if integrityFailed {
				targetIntegrityFailures++
			}
		}
	}
	return deleted, targetIntegrityFailures, nil
}

// sweepOneLeftoverLocalFile is the per-row body shared by
// sweepLeftoverLocalAlbumFiles/sweepLeftoverLocalChoreProofFiles: opens
// ref's local file (a missing file is simply "nothing to sweep here," not
// an error — most target-backend rows will never have had a local file to
// begin with, or already had one cleaned up), re-hashes it, and — ONLY when
// that hash matches the row's own recorded content hash — verifies the
// TARGET object ALSO exists and hashes correctly (verifyTargetObject)
// before ever deleting the local copy. Skipping this target check would let
// the sweep remove the last INTACT copy of a photo whose S3 object had
// gone missing or corrupt out-of-band (deleted directly from the bucket,
// object-store bit rot, etc.) — the local file is this sweep's ONLY
// evidence anything is wrong, so it must never be destroyed before that
// evidence is examined. Returns deleted=false, targetIntegrityFailed=true
// (not an error) on a missing/mismatched target, so the migrator continues
// sweeping other rows and simply counts the finding for the operator.
func (m *photoMigrator) sweepOneLeftoverLocalFile(ctx context.Context, ref mediadomain.StorageRef, expectedHash string) (deleted, targetIntegrityFailed bool, err error) {
	reader, err := m.localStore.Open(ctx, ref)
	if err != nil {
		if errors.Is(err, mediadomain.ErrPhotoNotFound) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("open local file %s for sweep: %w", ref, err)
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, reader)
	closeErr := reader.Close()
	if copyErr != nil {
		return false, false, fmt.Errorf("hash local file %s for sweep: %w", ref, copyErr)
	}
	if closeErr != nil {
		return false, false, fmt.Errorf("close local file %s after sweep hash: %w", ref, closeErr)
	}
	hash := hex.EncodeToString(hasher.Sum(nil))
	if expectedHash == "" || hash != expectedHash {
		// Never delete on an unverifiable or mismatched LOCAL hash — a
		// legacy photo.Photo row that was never backfilled (should not
		// happen post-migration, since MigrateStorageBackend always
		// backfills, but this is a defensive floor) or genuine local
		// corruption is left in place for an operator to investigate,
		// exactly like the main loop's inline hash-mismatch handling.
		return false, false, nil
	}

	// The local copy is verified; now confirm the TARGET copy is ALSO
	// intact before deleting the only other one — see this function's own
	// doc for why ObjectExists alone (which the earlier migrate pass relied
	// on before this fix) is not enough here either.
	if err := m.verifyTargetObject(ctx, ref, hash); err != nil {
		if errors.Is(err, errTargetIntegrityFailed) {
			return false, true, nil
		}
		return false, false, err
	}

	deletedLocal, err := m.deleteLocalIfUnreferenced(ctx, ref)
	return deletedLocal, false, err
}
