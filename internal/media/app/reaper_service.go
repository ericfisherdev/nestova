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
//     referenced by ANY row in EITHER table (ListAllStorageRefs), and
//     delete any object that is BOTH unreferenced AND older than
//     graceWindow — an object younger than the grace window might be
//     mid-upload (Put has written bytes but Create has not yet committed
//     the referencing row), not genuinely orphaned; see
//     domain.ObjectInfo.LastModified's doc. The referenced set is
//     deliberately built from BOTH repositories regardless of which class
//     is currently being swept: a cross-prefix row (see cmd/storage's
//     verifier.go doc) can reference an object filed under the OTHER
//     class's own prefix than the table it lives in, and that reference
//     must protect the object exactly as much as an ordinarily-prefixed
//     one would — see referencedRefs' doc.
//
// This design makes "restore a week-old DB backup against the same bucket"
// safe by construction: a restored row's StorageRef reappears in
// ListAllStorageRefs on the very next Run, so an object the reaper has not
// yet reached (or already skipped once, on an earlier grace-window-bounded
// pass) is protected again before it is ever deleted.
//
// TOCTOU note (deletion race): orphanCandidates' referenced-refs snapshot
// and its object listing are each taken once, up front, but a row can
// commit — e.g. a restore re-inserting it — at any point after that
// snapshot. To narrow that window, Run re-checks EACH candidate
// individually, via a targeted ExistsByStorageRef query, in the SAME loop
// iteration immediately before deleting it (not the stale bulk snapshot,
// and not a separate recheck pass completed before any deletes begin) —
// this closes the gap between the snapshot and each delete down to the
// single instant between that final check and the delete call itself. That
// residual instant is NOT
// closed by this design (a true row-locking, two-phase-commit coordination
// between the database and the object store would be needed to close it
// entirely, and is not attempted here) — it is accepted as part of this
// reaper's operator contract: Run is intended to be invoked by an operator
// (NES-133's `nestova storage reap` command), never automatically (see
// Run's own doc), and MUST NOT be run concurrently with a database restore.
// A restore is expected to be performed with the application (and so the
// reaper) fully quiesced — not serving traffic, not running a reaper pass —
// which is the realistic operating mode for a single-operator family
// appliance, not a multi-tenant service under continuous, uncoordinated
// write load.
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

// DryRunResult summarizes one DryRun: how many chore-proof rows the
// retention pass WOULD delete, and exactly which orphaned objects each
// class's sweep WOULD delete — refs, not just a count (unlike
// ReaperResult.OrphansDeleted), so NES-133's `storage reap --dry-run`
// command can list them for the operator to inspect before ever running
// the destructive Run.
type DryRunResult struct {
	RetentionRowsWouldDelete int64
	OrphansWouldDelete       map[domain.PhotoClass][]domain.StorageRef
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

	// Retention is scoped to r.backend (NES-133/149 fix — see
	// DeleteUploadedBefore's own doc): a reaper instance bound to ONE
	// backend must never delete a row belonging to the OTHER backend,
	// since only a reaper instance for that row's OWN backend's
	// lister/store could ever reclaim the now-unreferenced object
	// afterward — an unscoped delete would permanently strand it.
	//
	// Operator note: in a MIXED-STATE deployment (some chore-proof rows
	// still local, some already migrated to this reaper's backend),
	// retention only ever considers rows on r.backend — an old LOCAL row
	// survives a Run against the S3 reaper untouched, exactly as it
	// should (deleting it here would orphan a local file no S3-bound
	// reaper could ever clean up). Run NES-133's migration to completion
	// before relying on retention to actually reduce the chore-proof
	// table's size in a mixed-state install; see docs/storage.md.
	if r.choreProofRetention > 0 {
		cutoff := now.Add(-r.choreProofRetention)
		n, err := r.choreProofPhotos.DeleteUploadedBefore(ctx, r.backend, cutoff)
		if err != nil {
			return ReaperResult{}, fmt.Errorf("media/app: apply chore-proof retention: %w", err)
		}
		result.RetentionRowsDeleted = n
	}

	// referenced is computed ONCE, globally across both tables (see
	// referencedRefs' doc), and reused for every class's candidate
	// selection below — cheaper than one query pair per class, and (unlike
	// the per-object recheck) there is no safety reason to re-fetch it
	// per class: the bulk snapshot was never the final word on any
	// individual candidate to begin with (see the type doc's TOCTOU note).
	referenced, err := r.referencedRefs(ctx)
	if err != nil {
		return ReaperResult{}, fmt.Errorf("media/app: list referenced refs: %w", err)
	}

	cutoff := now.Add(-r.graceWindow)
	for _, class := range reapedClasses {
		candidates, err := r.orphanCandidates(ctx, class, cutoff, referenced)
		if err != nil {
			return ReaperResult{}, err
		}
		deleted := 0
		for _, ref := range candidates {
			// Targeted recheck, IMMEDIATELY before deleting: see the type
			// doc's TOCTOU note for why this — not the bulk candidate list
			// above — is what actually gates the delete. Doing this inside
			// the SAME loop iteration as the Delete call (rather than as a
			// separate pass over every candidate first) is what keeps the
			// window between the check and the delete down to a single
			// instant: a row that commits while an EARLIER candidate in
			// this loop is being processed must still be caught here, not
			// missed because its own recheck already happened before that
			// commit landed.
			stillReferenced, err := r.existsByStorageRef(ctx, ref)
			if err != nil {
				return ReaperResult{}, fmt.Errorf("media/app: recheck object %s before delete: %w", ref, err)
			}
			if stillReferenced {
				continue
			}
			if err := r.store.Delete(ctx, ref); err != nil {
				return ReaperResult{}, fmt.Errorf("media/app: delete orphaned object %s: %w", ref, err)
			}
			deleted++
		}
		result.OrphansDeleted[class] = deleted
	}
	return result, nil
}

// DryRun previews exactly what the next Run call would delete — the
// retention pass's row count and each class's orphan sweep's object refs —
// WITHOUT deleting or removing anything. It is NES-133's `storage reap
// --dry-run` command's non-destructive preview, driven by the identical
// candidate-selection logic Run itself uses (same cutoffs, same
// referencedRefs computation), so its output is a faithful preview of
// Run's next call, provided no row commits or object ages past the grace
// window in between (the type doc's TOCTOU note applies here too, as a
// staleness risk on the preview rather than a deletion race).
//
// Unlike Run, DryRun does NOT perform Run's per-candidate
// existsByStorageRef recheck: that recheck exists specifically to catch a
// row committing in the gap between the bulk snapshot and an ACTUAL
// delete call (see the type doc's TOCTOU note) — DryRun never deletes
// anything, so there is no such gap to protect, and calling it here would
// actively contradict the retention-cascade modeling below (it queries
// LIVE database state, which still shows a retention-doomed row as
// "referencing" its object, since DryRun's own retention preview never
// actually removes that row).
//
// DryRun MODELS retention's cascading effect on the orphan preview
// (NES-133/149): unlike Run, where the retention DeleteUploadedBefore call
// has already committed by the time the orphan sweep's referencedRefs
// query runs (so that query naturally reflects the post-retention state),
// DryRun never deletes anything — the chore-proof table still holds every
// row retention WOULD remove when the orphan preview is computed. Without
// correcting for this, DryRun would UNDER-report: a row retention would
// delete still counts as "referencing" its object in a naive snapshot, so
// that object would never appear as an orphan candidate in the preview
// even though the very next Run WOULD reap it (retention, then sweep, in
// the same call). dryRunReferencedRefs closes this gap by excluding refs
// ListStorageRefsUploadedBefore reports as retention-doomed — see that
// helper's doc for how it still protects a ref shared by a dedup pair (one
// doomed row, one surviving row referencing identical content-addressed
// bytes).
func (r *ReaperService) DryRun(ctx context.Context, now time.Time) (DryRunResult, error) {
	result := DryRunResult{OrphansWouldDelete: make(map[domain.PhotoClass][]domain.StorageRef, len(reapedClasses))}

	var retentionRefs []domain.StorageRef
	if r.choreProofRetention > 0 {
		retentionCutoff := now.Add(-r.choreProofRetention)
		refs, err := r.choreProofPhotos.ListStorageRefsUploadedBefore(ctx, r.backend, retentionCutoff)
		if err != nil {
			return DryRunResult{}, fmt.Errorf("media/app: preview chore-proof retention: %w", err)
		}
		retentionRefs = refs
		result.RetentionRowsWouldDelete = int64(len(refs))
	}

	referenced, err := r.dryRunReferencedRefs(ctx, retentionRefs)
	if err != nil {
		return DryRunResult{}, fmt.Errorf("media/app: list referenced refs: %w", err)
	}

	cutoff := now.Add(-r.graceWindow)
	for _, class := range reapedClasses {
		candidates, err := r.orphanCandidates(ctx, class, cutoff, referenced)
		if err != nil {
			return DryRunResult{}, err
		}
		result.OrphansWouldDelete[class] = candidates
	}
	return result, nil
}

// orphanCandidates computes class's orphaned-object CANDIDATES from the
// BULK snapshot only — every stored object not referenced by any row in
// EITHER table (the referenced set the caller supplies — see
// referencedRefs/dryRunReferencedRefs), and older than cutoff — WITHOUT any
// per-object recheck. This bulk list is deliberately not the final word on
// any individual candidate (see the type doc's TOCTOU note): Run performs
// its own recheck immediately before each Delete, in the same loop
// iteration, so a row that commits partway through processing this list
// still protects whichever candidate it now references; DryRun performs a
// single recheck pass over this same list for its preview.
func (r *ReaperService) orphanCandidates(ctx context.Context, class domain.PhotoClass, cutoff time.Time, referenced map[domain.StorageRef]struct{}) ([]domain.StorageRef, error) {
	objects, err := r.lister.ListObjects(ctx, class)
	if err != nil {
		return nil, fmt.Errorf("media/app: list stored objects for class %s: %w", class, err)
	}

	candidates := make([]domain.StorageRef, 0)
	for _, obj := range objects {
		if _, ok := referenced[obj.Key]; ok {
			continue
		}
		if obj.LastModified.After(cutoff) {
			continue
		}
		candidates = append(candidates, obj.Key)
	}
	return candidates, nil
}

// referencedRefs builds the set of StorageRefs ANY row STAMPED WITH
// r.backend still references, across BOTH tables and every household —
// deliberately NOT scoped to whichever repository "owns" a given class:
// a cross-prefix row (see cmd/storage's verifier.go classifyS3 doc) can
// reference an object filed under a DIFFERENT class's own prefix than the
// table it is persisted in, so checking only the nominally-matching
// repository would let that reference go unseen — the sweep would then
// treat a genuinely-referenced object as an orphan and delete it. Computed
// ONCE per Run/DryRun call (not once per class) since it is already
// bucket-wide and backend-scoped, not class-scoped; PhotoClassRewardImage
// never needs its own entry here since neither repository stores rows for
// it (see reapedClasses' doc). This is the bulk CANDIDATE-selection
// snapshot orphanCandidates filters against — see existsByStorageRef for
// the per-object recheck that has the final say.
func (r *ReaperService) referencedRefs(ctx context.Context) (map[domain.StorageRef]struct{}, error) {
	albumRefs, err := r.photos.ListAllStorageRefs(ctx, r.backend)
	if err != nil {
		return nil, fmt.Errorf("list album refs: %w", err)
	}
	choreProofRefs, err := r.choreProofPhotos.ListAllStorageRefs(ctx, r.backend)
	if err != nil {
		return nil, fmt.Errorf("list chore-proof refs: %w", err)
	}
	set := make(map[domain.StorageRef]struct{}, len(albumRefs)+len(choreProofRefs))
	for _, ref := range albumRefs {
		set[ref] = struct{}{}
	}
	for _, ref := range choreProofRefs {
		set[ref] = struct{}{}
	}
	return set, nil
}

// dryRunReferencedRefs is referencedRefs' DryRun-only variant: the SAME
// global, both-tables referenced set, but as of the HYPOTHETICAL state
// immediately after retention (if any) ran — see DryRun's own doc for why
// this modeling exists. retentionRefs is
// ListStorageRefsUploadedBefore's result: the refs of chore-proof rows
// retention WOULD delete.
//
// A ref is excluded from the result only when EVERY chore-proof row
// referencing it is retention-doomed — counted via a multiset (occurrence
// counts), not a plain set difference, specifically to protect the
// content-addressed dedup case: two chore-proof rows can share one
// StorageRef (see domain.PhotoClass's dedup note), and if only ONE of a
// pair is old enough for retention to remove while the other survives,
// the shared ref must still count as referenced — the surviving row's
// evidence, not the doomed one's, is what protects the object.
func (r *ReaperService) dryRunReferencedRefs(ctx context.Context, retentionRefs []domain.StorageRef) (map[domain.StorageRef]struct{}, error) {
	albumRefs, err := r.photos.ListAllStorageRefs(ctx, r.backend)
	if err != nil {
		return nil, fmt.Errorf("list album refs: %w", err)
	}
	choreProofRefs, err := r.choreProofPhotos.ListAllStorageRefs(ctx, r.backend)
	if err != nil {
		return nil, fmt.Errorf("list chore-proof refs: %w", err)
	}

	referenced := make(map[domain.StorageRef]struct{}, len(albumRefs)+len(choreProofRefs))
	for _, ref := range albumRefs {
		referenced[ref] = struct{}{}
	}

	doomedCounts := make(map[domain.StorageRef]int, len(retentionRefs))
	for _, ref := range retentionRefs {
		doomedCounts[ref]++
	}
	totalCounts := make(map[domain.StorageRef]int, len(choreProofRefs))
	for _, ref := range choreProofRefs {
		totalCounts[ref]++
	}
	for ref, total := range totalCounts {
		if total > doomedCounts[ref] {
			// At least one row referencing ref survives retention.
			referenced[ref] = struct{}{}
		}
	}
	return referenced, nil
}

// existsByStorageRef is referencedRefs' single-ref counterpart: a fresh,
// targeted query (filtered to r.backend, for the same reason
// referencedRefs' doc explains) against BOTH repositories — not just
// whichever one nominally "owns" the class an object was listed under —
// run immediately before Run's loop would otherwise delete ref's object.
// Checking both is required for the identical cross-prefix reason
// referencedRefs' bulk snapshot does: a row from the OTHER table can
// legitimately reference ref, and that reference must be honored
// regardless of which class's bucket listing surfaced ref as a candidate.
// See the type doc's TOCTOU note for why this, not the bulk snapshot, is
// authoritative at delete time.
func (r *ReaperService) existsByStorageRef(ctx context.Context, ref domain.StorageRef) (bool, error) {
	existsAlbum, err := r.photos.ExistsByStorageRef(ctx, ref, r.backend)
	if err != nil {
		return false, fmt.Errorf("check album reference: %w", err)
	}
	if existsAlbum {
		return true, nil
	}
	return r.choreProofPhotos.ExistsByStorageRef(ctx, ref, r.backend)
}
