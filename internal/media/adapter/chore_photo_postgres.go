package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// choreProofInstanceLockNamespace is the first key of the two-integer form
// of Create's per-task-instance pg_advisory_xact_lock (see Create's doc).
// Postgres tracks the single-bigint form (e.g.
// cmd/server/provisioning.go's onboardingAdvisoryLock) and the two-integer
// form as genuinely separate lock spaces (the bigint form's pg_locks row has
// objsubid=1, the two-integer form's has objsubid=2 —
// postgresql.org/docs/current/view-pg-locks.html), and this is itself
// distinct from kioskHouseholdLockNamespace (activation_code_postgres.go),
// so none of these can collide regardless of numeric value. Chosen as the
// ASCII bytes of "CHRP" (chore proof) purely as a memorable, human-readable
// tag, mirroring kioskHouseholdLockNamespace's own precedent.
const choreProofInstanceLockNamespace int32 = 0x43485250

// TaskInstancePhotoRepository is the pgx-backed domain.TaskInstancePhotoRepository.
// It persists chore-proof photo metadata only; the bytes live behind the
// PhotoStore, under domain.PhotoClassChoreProof.
type TaskInstancePhotoRepository struct {
	dbtx db.TX
	// backend is the StorageBackend Create stamps onto every row it writes
	// (NES-132) — see NewTaskInstancePhotoRepository's doc.
	backend domain.StorageBackend
}

var _ domain.TaskInstancePhotoRepository = (*TaskInstancePhotoRepository)(nil)

// NewTaskInstancePhotoRepository constructs the repository with an injected
// query executor, bound to backend — mirrors NewPhotoRepository's contract
// exactly; see that constructor's doc. Panics on a nil dbtx or an invalid
// backend.
func NewTaskInstancePhotoRepository(dbtx db.TX, backend domain.StorageBackend) *TaskInstancePhotoRepository {
	if dbtx == nil {
		panic("media/adapter: NewTaskInstancePhotoRepository requires a non-nil db.TX")
	}
	if !backend.Valid() {
		panic(fmt.Sprintf("media/adapter: NewTaskInstancePhotoRepository requires a valid StorageBackend, got %q", backend))
	}
	return &TaskInstancePhotoRepository{dbtx: dbtx, backend: backend}
}

const taskInstancePhotoColumns = `
	SELECT id, household_id, task_instance_id, kind, storage_ref, storage_backend, content_sha256,
	       size_bytes, content_type, taken_at, uploaded_by, uploaded_at
	  FROM task_instance_photo`

// txBeginner is satisfied by *pgxpool.Pool (the executor every real caller
// injects); asserted against r.dbtx so Create can open its own transaction
// without widening the minimal db.TX interface (Exec/Query/QueryRow) every
// OTHER adapter method is deliberately kept to. Mirrors the identical
// pattern in kiosk/adapter's ActivationCodeRepository.Redeem.
type txBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// queryRower is the read surface Create's transactional before/after check
// shares with the standalone LatestTakenAt: both pgx.Tx and db.TX satisfy
// it, so latestTakenAt (below) runs identically inside Create's transaction
// or against the plain pool.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Create inserts a chore-proof photo and populates its UploadedAt.
//
// The insert runs inside its own transaction, which first acquires a
// per-task-instance pg_advisory_xact_lock — taken by EVERY Create for a
// given instance, regardless of Kind, not just an "after" upload. This
// matters for two reasons:
//
//  1. Without a "before" insert also taking the lock, a "before" insert
//     racing an "after" upload's check-then-insert could land in the gap
//     between the after's read of the latest "before" and its own insert,
//     letting a decision that was accurate when read become stale by the
//     time it commits.
//  2. The ordering check itself is SYMMETRIC (see below), so a "before"
//     insert has its own read-then-decide step that needs the exact same
//     protection an "after" insert's does.
//
// With every Create for the instance serialized through the same lock, a
// Create's read of the OTHER kind's relevant extreme (below) is always
// consistent with whatever is genuinely committed at that instant — a
// concurrent Create for the same instance either already committed before
// this lock was acquired (and so is visible to the read) or is still
// blocked waiting for this transaction to release the lock (and so cannot
// yet have committed). The lock auto-releases at commit or rollback;
// different instances never contend with each other (see
// choreProofInstanceLockNamespace's doc).
//
// The ordering check is symmetric in what it protects (one invariant: no
// "after" may precede any "before" for the same instance) but asymmetric in
// which extreme each direction reads, since either kind can be the one
// being newly inserted:
//   - photo.Kind == PhotoKindAfter: rejected (ErrAfterPrecedesBefore) when
//     photo.TakenAt is earlier than the instance's LATEST existing "before"
//     — a later "before" would make an otherwise-fine "after" retroactively
//     invalid, so the newest one is what must be checked against.
//   - photo.Kind == PhotoKindBefore: rejected (ErrAfterPrecedesBefore) when
//     photo.TakenAt is LATER than the instance's EARLIEST existing "after"
//     — inserting a "before" chronologically after work was already
//     photographed as finished is the same invariant violation, just
//     approached from the other direction; the earliest "after" is the
//     tightest existing bound to check a new "before" against.
//
// Maps an unknown/foreign task instance to domain.ErrTaskInstanceNotFound
// and an unknown uploader to household.ErrMemberNotFound (both via the
// composite tenant FK violations).
func (r *TaskInstancePhotoRepository) Create(ctx context.Context, photo *domain.TaskInstancePhoto) error {
	if photo == nil {
		return errors.New("media/adapter: create task instance photo: nil photo")
	}
	beginner, ok := r.dbtx.(txBeginner)
	if !ok {
		return errors.New("media/adapter: create task instance photo: executor does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("create task instance photo: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`,
		choreProofInstanceLockNamespace, photo.TaskInstanceID.String()); err != nil {
		return fmt.Errorf("create task instance photo: acquire instance lock: %w", err)
	}

	switch photo.Kind {
	case domain.PhotoKindAfter:
		beforeTakenAt, ok, err := latestTakenAt(ctx, tx, photo.HouseholdID, photo.TaskInstanceID, domain.PhotoKindBefore)
		if err != nil {
			return fmt.Errorf("create task instance photo: check latest before photo: %w", err)
		}
		if ok && domain.AfterPrecedesBefore(photo.TakenAt, beforeTakenAt) {
			return domain.ErrAfterPrecedesBefore
		}
	case domain.PhotoKindBefore:
		afterTakenAt, ok, err := earliestTakenAt(ctx, tx, photo.HouseholdID, photo.TaskInstanceID, domain.PhotoKindAfter)
		if err != nil {
			return fmt.Errorf("create task instance photo: check earliest after photo: %w", err)
		}
		if ok && domain.AfterPrecedesBefore(afterTakenAt, photo.TakenAt) {
			return domain.ErrAfterPrecedesBefore
		}
	}

	const q = `
		INSERT INTO task_instance_photo
			(id, household_id, task_instance_id, kind, storage_ref, storage_backend, content_sha256,
			 size_bytes, content_type, taken_at, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING uploaded_at`
	err = tx.QueryRow(ctx, q,
		photo.ID.String(), photo.HouseholdID.String(), photo.TaskInstanceID.String(), photo.Kind.String(),
		photo.StorageRef.String(), r.backend.String(), photo.ContentHash, photo.SizeBytes, photo.ContentType,
		photo.TakenAt, memberArg(photo.UploadedBy),
	).Scan(&photo.UploadedAt)
	if err != nil {
		if mapped := mapFKViolation(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("create task instance photo: %w", err)
	}
	photo.StorageBackend = r.backend

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("create task instance photo: commit: %w", err)
	}
	return nil
}

// Get returns the chore-proof photo identified by id (NES-120), or
// domain.ErrTaskInstancePhotoNotFound when id is unknown. Deliberately
// ID-only — see the domain port doc: household ownership is enforced by
// the caller (ChoreProofPhotoService.OpenBytes), mirroring PhotoRepository.
// Get's identical album-path contract.
func (r *TaskInstancePhotoRepository) Get(ctx context.Context, id domain.TaskInstancePhotoID) (*domain.TaskInstancePhoto, error) {
	photo, err := scanTaskInstancePhoto(r.dbtx.QueryRow(ctx, taskInstancePhotoColumns+` WHERE id = $1`, id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTaskInstancePhotoNotFound
		}
		return nil, fmt.Errorf("get task instance photo: %w", err)
	}
	return photo, nil
}

// InstanceExists reports whether taskInstanceID exists within householdID's
// task_instance table. See the domain port doc: this is a best-effort
// preflight convenience for ChoreProofPhotoService.Upload, not the
// authoritative existence check — Create's own FK violation is.
func (r *TaskInstancePhotoRepository) InstanceExists(ctx context.Context, householdID household.HouseholdID, taskInstanceID domain.TaskInstanceID) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM task_instance WHERE household_id = $1 AND id = $2)`
	var exists bool
	if err := r.dbtx.QueryRow(ctx, q, householdID.String(), taskInstanceID.String()).Scan(&exists); err != nil {
		return false, fmt.Errorf("check task instance exists: %w", err)
	}
	return exists, nil
}

// LatestTakenAt returns the most recent taken_at among the instance's photos
// of the given kind, and ok=true, or ok=false when none exist. A plain,
// unlocked read against the pool — see the domain port doc for why this is
// not part of Create's own atomicity (Create re-reads the same fact itself,
// inside its own transaction, via the shared latestTakenAt helper below).
func (r *TaskInstancePhotoRepository) LatestTakenAt(ctx context.Context, householdID household.HouseholdID, taskInstanceID domain.TaskInstanceID, kind domain.PhotoKind) (time.Time, bool, error) {
	return latestTakenAt(ctx, r.dbtx, householdID, taskInstanceID, kind)
}

// latestTakenAt runs the shared query behind both the public LatestTakenAt
// (against the plain pool) and Create's transactional check of an "after"
// insert against the LATEST existing "before" (against its own tx) — q is
// whichever queryRower the caller holds.
func latestTakenAt(ctx context.Context, q queryRower, householdID household.HouseholdID, taskInstanceID domain.TaskInstanceID, kind domain.PhotoKind) (time.Time, bool, error) {
	const query = `
		SELECT taken_at FROM task_instance_photo
		 WHERE household_id = $1 AND task_instance_id = $2 AND kind = $3
		 ORDER BY taken_at DESC
		 LIMIT 1`
	return scanTakenAt(ctx, q, query, householdID, taskInstanceID, kind)
}

// earliestTakenAt is latestTakenAt's mirror (ORDER BY ASC instead of DESC),
// used only by Create's transactional check of a "before" insert against
// the EARLIEST existing "after" — see Create's doc for why the before
// direction checks the earliest, not the latest.
func earliestTakenAt(ctx context.Context, q queryRower, householdID household.HouseholdID, taskInstanceID domain.TaskInstanceID, kind domain.PhotoKind) (time.Time, bool, error) {
	const query = `
		SELECT taken_at FROM task_instance_photo
		 WHERE household_id = $1 AND task_instance_id = $2 AND kind = $3
		 ORDER BY taken_at ASC
		 LIMIT 1`
	return scanTakenAt(ctx, q, query, householdID, taskInstanceID, kind)
}

// scanTakenAt runs query (either latestTakenAt's or earliestTakenAt's,
// differing only in ORDER BY direction) and scans the single taken_at
// result, or reports ok=false (not an error) when no row matches.
func scanTakenAt(ctx context.Context, q queryRower, query string, householdID household.HouseholdID, taskInstanceID domain.TaskInstanceID, kind domain.PhotoKind) (time.Time, bool, error) {
	var takenAt time.Time
	err := q.QueryRow(ctx, query, householdID.String(), taskInstanceID.String(), kind.String()).Scan(&takenAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("taken_at: %w", err)
	}
	return takenAt, true, nil
}

// ListByInstance returns every chore-proof photo for the instance ordered by
// taken_at ascending, or an empty slice when none exist.
func (r *TaskInstancePhotoRepository) ListByInstance(ctx context.Context, householdID household.HouseholdID, taskInstanceID domain.TaskInstanceID) ([]*domain.TaskInstancePhoto, error) {
	rows, err := r.dbtx.Query(ctx, taskInstancePhotoColumns+` WHERE household_id = $1 AND task_instance_id = $2 ORDER BY taken_at`,
		householdID.String(), taskInstanceID.String())
	if err != nil {
		return nil, fmt.Errorf("list task instance photos: %w", err)
	}
	defer rows.Close()
	photos := make([]*domain.TaskInstancePhoto, 0)
	for rows.Next() {
		photo, err := scanTaskInstancePhoto(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task instance photo: %w", err)
		}
		photos = append(photos, photo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan task instance photos: %w", err)
	}
	return photos, nil
}

// ListByInstances is ListByInstance's batch counterpart (NES-120): every
// chore-proof photo across all of taskInstanceIDs, household-scoped, via a
// single `= ANY($2)` query instead of one round trip per instance — see the
// domain port doc for the N+1 this exists to avoid. Returns an empty slice
// (not an error) when taskInstanceIDs is empty; the query is skipped
// entirely in that case (`= ANY('{}')` would just as validly return zero
// rows, but skipping avoids the round trip altogether for a caller that
// legitimately has nothing to ask for).
func (r *TaskInstancePhotoRepository) ListByInstances(ctx context.Context, householdID household.HouseholdID, taskInstanceIDs []domain.TaskInstanceID) ([]*domain.TaskInstancePhoto, error) {
	if len(taskInstanceIDs) == 0 {
		return []*domain.TaskInstancePhoto{}, nil
	}
	ids := make([]string, len(taskInstanceIDs))
	for i, id := range taskInstanceIDs {
		ids[i] = id.String()
	}
	rows, err := r.dbtx.Query(ctx, taskInstancePhotoColumns+` WHERE household_id = $1 AND task_instance_id = ANY($2)`,
		householdID.String(), ids)
	if err != nil {
		return nil, fmt.Errorf("list task instance photos by instances: %w", err)
	}
	defer rows.Close()
	photos := make([]*domain.TaskInstancePhoto, 0)
	for rows.Next() {
		photo, err := scanTaskInstancePhoto(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task instance photo: %w", err)
		}
		photos = append(photos, photo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan task instance photos: %w", err)
	}
	return photos, nil
}

// ListAllStorageRefs returns the StorageRef of every chore-proof photo row
// stamped with backend, across every household (see the domain port doc:
// the storage reaper's source of truth for referenced chore-proof-class
// objects of that specific backend), or an empty slice when there are none.
func (r *TaskInstancePhotoRepository) ListAllStorageRefs(ctx context.Context, backend domain.StorageBackend) ([]domain.StorageRef, error) {
	rows, err := r.dbtx.Query(ctx, `SELECT storage_ref FROM task_instance_photo WHERE storage_backend = $1`, backend.String())
	if err != nil {
		return nil, fmt.Errorf("list all task instance photo storage refs: %w", err)
	}
	defer rows.Close()
	refs := make([]domain.StorageRef, 0)
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("scan task instance photo storage ref: %w", err)
		}
		refs = append(refs, domain.StorageRef(ref))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan task instance photo storage refs: %w", err)
	}
	return refs, nil
}

// DeleteUploadedBefore deletes every chore-proof photo row uploaded strictly
// before cutoff and reports how many rows were removed — see the domain
// port doc for why this deletes rows only, never the underlying object.
func (r *TaskInstancePhotoRepository) DeleteUploadedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := r.dbtx.Exec(ctx, `DELETE FROM task_instance_photo WHERE uploaded_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete task instance photos uploaded before cutoff: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ExistsByStorageRef reports whether any chore-proof photo row STAMPED WITH
// backend currently references ref (see the domain port doc: the reaper's
// targeted, pre-delete TOCTOU-narrowing recheck, filtered by backend).
func (r *TaskInstancePhotoRepository) ExistsByStorageRef(ctx context.Context, ref domain.StorageRef, backend domain.StorageBackend) (bool, error) {
	var exists bool
	if err := r.dbtx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_instance_photo WHERE storage_ref = $1 AND storage_backend = $2)`, ref.String(), backend.String()).Scan(&exists); err != nil {
		return false, fmt.Errorf("check task instance photo storage ref exists: %w", err)
	}
	return exists, nil
}

func scanTaskInstancePhoto(r row) (*domain.TaskInstancePhoto, error) {
	var (
		photo          domain.TaskInstancePhoto
		idStr          string
		hhStr          string
		instanceStr    string
		kindStr        string
		storageRef     string
		storageBackend string
		uploadedByStr  *string
	)
	if err := r.Scan(
		&idStr, &hhStr, &instanceStr, &kindStr, &storageRef, &storageBackend, &photo.ContentHash,
		&photo.SizeBytes, &photo.ContentType, &photo.TakenAt, &uploadedByStr, &photo.UploadedAt,
	); err != nil {
		return nil, err
	}
	id, err := domain.ParseTaskInstancePhotoID(idStr)
	if err != nil {
		return nil, fmt.Errorf("parse task instance photo id: %w", err)
	}
	hh, err := household.ParseHouseholdID(hhStr)
	if err != nil {
		return nil, fmt.Errorf("parse household id: %w", err)
	}
	instanceID, err := domain.ParseTaskInstanceID(instanceStr)
	if err != nil {
		return nil, fmt.Errorf("parse task instance id: %w", err)
	}
	kind, err := domain.ParsePhotoKind(kindStr)
	if err != nil {
		return nil, fmt.Errorf("parse photo kind: %w", err)
	}
	backend, err := domain.ParseStorageBackend(storageBackend)
	if err != nil {
		return nil, fmt.Errorf("parse storage backend: %w", err)
	}
	photo.ID = id
	photo.HouseholdID = hh
	photo.TaskInstanceID = instanceID
	photo.Kind = kind
	photo.StorageRef = domain.StorageRef(storageRef)
	photo.StorageBackend = backend
	if uploadedByStr != nil {
		memberID, err := household.ParseMemberID(*uploadedByStr)
		if err != nil {
			return nil, fmt.Errorf("parse uploader id: %w", err)
		}
		photo.UploadedBy = &memberID
	}
	return &photo, nil
}
