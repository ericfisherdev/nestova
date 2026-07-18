package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// reapedClasses is every domain.PhotoClass the storage reaper walks —
// PhotoClassAlbum and PhotoClassChoreProof, mirroring the ticket's "walking
// BOTH photo classes under their prefixes." PhotoClassRewardImage is
// deliberately excluded: it only reserves a key namespace (see
// domain.PhotoClass's doc) — no upload path exists for it yet, so it never
// has objects to reap.
var reapedClasses = []domain.PhotoClass{domain.PhotoClassAlbum, domain.PhotoClassChoreProof}

// ReaperService reclaims orphaned PhotoStore objects for an object-store
// backend (NES-132/NES-133): objects a Delete's "rows-only" invariant (see
// PhotoService.Delete's doc) leaves behind once nothing references them.
// Never invoked automatically by this ticket's composition root — see
// Run's doc for why invocation is deliberately left to a caller (NES-133's
// planned `nestova storage` commands, and/or a future scheduler step).
//
// Two independent passes, run in this order every time Run is called:
//
//  1. Optional chore-proof retention (choreProofRetention > 0): deletes
//     TaskInstancePhoto ROWS uploaded before the retention cutoff. This is
//     a row deletion only — see DeleteUploadedBefore's doc — so the objects
//     those rows referenced become orphan candidates for pass 2, not
//     necessarily reaped in the SAME Run call (they still have to clear the
//     grace window below).
//  2. Orphan sweep, per reapedClasses: list every stored object of the
//     class (lister.ListObjects), compute the set of StorageRefs still
//     referenced by a row in either table (ListAllStorageRefs), and delete
//     any object that is BOTH unreferenced AND older than graceWindow — an
//     object younger than the grace window might be mid-upload (Put has
//     written bytes but Create has not yet committed the referencing row),
//     not genuinely orphaned; see domain.ObjectInfo.LastModified's doc.
//
// This design makes "restore a week-old DB backup against the same bucket"
// safe by construction: a restored row's StorageRef reappears in
// ListAllStorageRefs on the very next Run, so an object the reaper has not
// yet reached (or already skipped once, on an earlier grace-window-bounded
// pass) is protected again before it is ever deleted.
//
// TOCTOU note (deletion race): sweepClass's referenced-refs snapshot and
// its object listing are each taken once, up front, but a row can commit —
// e.g. a restore re-inserting it — at any point after that snapshot. To
// narrow that window, sweepClass re-checks EACH candidate individually,
// via a targeted ExistsByStorageRef query, immediately before deleting it
// (not the stale bulk snapshot) — this closes the gap between the snapshot
// and each delete down to the single instant between that final check and
// the delete call itself. That residual instant is NOT closed by this
// design (a true row-locking, two-phase-commit coordination between the
// database and the object store would be needed to close it entirely, and
// is not attempted here) — it is accepted as part of this reaper's
// operator contract: Run is intended to be invoked by an operator (NES-133's
// planned `nestova storage` CLI), never automatically (see Run's own doc),
// and MUST NOT be run concurrently with a database restore. A restore is
// expected to be performed with the application (and so the reaper) fully
// quiesced — not serving traffic, not running a reaper pass — which is the
// realistic operating mode for a single-operator family appliance, not a
// multi-tenant service under continuous, uncoordinated write load.
type ReaperService struct {
	lister domain.ObjectLister
	store  domain.PhotoStore
	// backend is which StorageBackend THIS reaper instance sweeps — lister
	// and store are themselves backend-specific concrete adapters (e.g. the
	// S3 store/lister), and referencedRefs/existsByStorageRef must filter
	// the shared photo/task_instance_photo tables to ONLY this backend's
	// rows: content-addressed keys are identical across backends, so an
	// unfiltered query would let a content-identical row stamped with a
	// DIFFERENT backend shield a genuine orphan of THIS backend forever.
	backend             domain.StorageBackend
	photos              domain.PhotoRepository
	choreProofPhotos    domain.TaskInstancePhotoRepository
	graceWindow         time.Duration
	choreProofRetention time.Duration
}

// NewReaperService constructs a ReaperService bound to backend — the
// StorageBackend lister/store actually sweep (e.g. domain.StorageBackendS3
// for an S3-backed deployment's reaper) — returning an error instead of
// panicking on a nil dependency, an invalid backend, or a non-positive
// graceWindow. choreProofRetention of zero (or less) disables the retention
// pass entirely — "keep forever," MediaConfig.ChoreProofRetention's
// documented default.
func NewReaperService(
	lister domain.ObjectLister,
	store domain.PhotoStore,
	backend domain.StorageBackend,
	photos domain.PhotoRepository,
	choreProofPhotos domain.TaskInstancePhotoRepository,
	graceWindow time.Duration,
	choreProofRetention time.Duration,
) (*ReaperService, error) {
	switch {
	case lister == nil:
		return nil, errors.New("media/app: NewReaperService requires a non-nil ObjectLister")
	case store == nil:
		return nil, errors.New("media/app: NewReaperService requires a non-nil PhotoStore")
	case !backend.Valid():
		return nil, fmt.Errorf("media/app: NewReaperService requires a valid StorageBackend, got %q", backend)
	case photos == nil:
		return nil, errors.New("media/app: NewReaperService requires a non-nil PhotoRepository")
	case choreProofPhotos == nil:
		return nil, errors.New("media/app: NewReaperService requires a non-nil TaskInstancePhotoRepository")
	case graceWindow <= 0:
		return nil, fmt.Errorf("media/app: NewReaperService requires a positive graceWindow, got %v", graceWindow)
	}
	return &ReaperService{
		lister: lister, store: store, backend: backend, photos: photos, choreProofPhotos: choreProofPhotos,
		graceWindow: graceWindow, choreProofRetention: choreProofRetention,
	}, nil
}

// ReaperResult summarizes one Run: how many chore-proof rows the retention
// pass deleted, and how many orphaned objects the sweep deleted per class.
type ReaperResult struct {
	RetentionRowsDeleted int64
	OrphansDeleted       map[domain.PhotoClass]int
}

// Run executes one reaper pass (see the type doc for the two-pass order and
// the restore-safety argument) against now, and returns a summary. now is
// caller-supplied (mirroring ChoreProofPhotoService.Upload's identical
// pattern) so a test can pin it rather than depending on the wall clock.
//
// Run is exposed for a caller to invoke — NES-133's planned `nestova
// storage` CLI commands, and/or a scheduler step — but this ticket's
// composition root (cmd/server/main.go) does not wire an automatic
// scheduler for it: unlike the app's other background workers (dispatcher,
// task/restock/renewal/calendar-sync schedulers), reaping is destructive
// (it permanently deletes S3 objects and, when retention is configured,
// database rows) and NES-133 is the ticket that defines the operator-facing
// surface (dry-run/verify modes, CLI invocation) this deserves; wiring an
// unattended timer here ahead of that surface existing would be adding a
// destructive background job with no way to inspect or pause it.
func (r *ReaperService) Run(ctx context.Context, now time.Time) (ReaperResult, error) {
	result := ReaperResult{OrphansDeleted: make(map[domain.PhotoClass]int, len(reapedClasses))}

	// KNOWN GAP (not addressed by NES-132's mixed-state fix): unlike
	// referencedRefs/existsByStorageRef below, DeleteUploadedBefore is NOT
	// filtered by r.backend — it deletes any sufficiently-old chore-proof
	// ROW regardless of which backend actually stored its bytes. In a
	// mixed-state deployment (some rows local, some s3), a reaper instance
	// bound to ONE backend's lister/store could therefore delete a row
	// backed by the OTHER backend, permanently orphaning that row's object
	// with no reaper instance able to ever reclaim it (this reaper only
	// lists/deletes ITS OWN backend's objects). Left as-is because it was
	// not part of this review's explicit scope; NES-133 should either scope
	// DeleteUploadedBefore by backend too, or run retention from a
	// combined-backend context that can route the resulting orphan to the
	// right reaper.
	if r.choreProofRetention > 0 {
		cutoff := now.Add(-r.choreProofRetention)
		n, err := r.choreProofPhotos.DeleteUploadedBefore(ctx, cutoff)
		if err != nil {
			return ReaperResult{}, fmt.Errorf("media/app: apply chore-proof retention: %w", err)
		}
		result.RetentionRowsDeleted = n
	}

	cutoff := now.Add(-r.graceWindow)
	for _, class := range reapedClasses {
		deleted, err := r.sweepClass(ctx, class, cutoff)
		if err != nil {
			return ReaperResult{}, err
		}
		result.OrphansDeleted[class] = deleted
	}
	return result, nil
}

// sweepClass reclaims class's orphaned objects: every stored object not
// referenced by any row in either table, and older than cutoff (see Run's
// doc for the grace-window rationale). Returns how many objects were
// deleted.
//
// The bulk referencedRefs snapshot decides which objects are CANDIDATES for
// deletion; it is deliberately NOT the final word on any individual one —
// see the type doc's TOCTOU note. Each candidate gets its own fresh
// existsByStorageRef check immediately before its Delete call, so a row
// that committed after the snapshot (but before this candidate's turn in
// the loop) still protects its object.
func (r *ReaperService) sweepClass(ctx context.Context, class domain.PhotoClass, cutoff time.Time) (int, error) {
	referenced, err := r.referencedRefs(ctx, class)
	if err != nil {
		return 0, fmt.Errorf("media/app: list referenced refs for class %s: %w", class, err)
	}
	objects, err := r.lister.ListObjects(ctx, class)
	if err != nil {
		return 0, fmt.Errorf("media/app: list stored objects for class %s: %w", class, err)
	}

	deleted := 0
	for _, obj := range objects {
		if _, ok := referenced[obj.Key]; ok {
			continue
		}
		if obj.LastModified.After(cutoff) {
			continue
		}
		// Targeted recheck, immediately before deleting: see the type doc's
		// TOCTOU note for why this — not the bulk snapshot above — is what
		// actually gates the delete.
		stillReferenced, err := r.existsByStorageRef(ctx, class, obj.Key)
		if err != nil {
			return deleted, fmt.Errorf("media/app: recheck object %s before delete: %w", obj.Key, err)
		}
		if stillReferenced {
			continue
		}
		if err := r.store.Delete(ctx, obj.Key); err != nil {
			return deleted, fmt.Errorf("media/app: delete orphaned object %s: %w", obj.Key, err)
		}
		deleted++
	}
	return deleted, nil
}

// referencedRefs builds the set of StorageRefs class's rows STAMPED WITH
// r.backend still reference, across every household — album refs come from
// PhotoRepository, chore-proof refs from TaskInstancePhotoRepository;
// PhotoClassRewardImage never reaches here (see reapedClasses' doc).
// r.backend is passed explicitly (see the type doc's field comment): a
// content-identical row stamped with a DIFFERENT backend must never shield
// r.backend's genuine orphan. This is the bulk CANDIDATE-selection snapshot
// sweepClass filters against — see existsByStorageRef for the per-object
// recheck that has the final say.
func (r *ReaperService) referencedRefs(ctx context.Context, class domain.PhotoClass) (map[domain.StorageRef]struct{}, error) {
	var refs []domain.StorageRef
	var err error
	switch class {
	case domain.PhotoClassAlbum:
		refs, err = r.photos.ListAllStorageRefs(ctx, r.backend)
	case domain.PhotoClassChoreProof:
		refs, err = r.choreProofPhotos.ListAllStorageRefs(ctx, r.backend)
	default:
		return nil, fmt.Errorf("media/app: reaper does not support class %s", class)
	}
	if err != nil {
		return nil, err
	}
	set := make(map[domain.StorageRef]struct{}, len(refs))
	for _, ref := range refs {
		set[ref] = struct{}{}
	}
	return set, nil
}

// existsByStorageRef is referencedRefs' single-ref counterpart: a fresh,
// targeted query (filtered to r.backend, for the same reason
// referencedRefs' doc explains) against whichever repository owns class,
// run by sweepClass immediately before it would otherwise delete ref's
// object — see the type doc's TOCTOU note for why this, not the bulk
// snapshot, is authoritative at delete time.
func (r *ReaperService) existsByStorageRef(ctx context.Context, class domain.PhotoClass, ref domain.StorageRef) (bool, error) {
	switch class {
	case domain.PhotoClassAlbum:
		return r.photos.ExistsByStorageRef(ctx, ref, r.backend)
	case domain.PhotoClassChoreProof:
		return r.choreProofPhotos.ExistsByStorageRef(ctx, ref, r.backend)
	default:
		return false, fmt.Errorf("media/app: reaper does not support class %s", class)
	}
}
