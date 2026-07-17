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
}

var _ domain.TaskInstancePhotoRepository = (*TaskInstancePhotoRepository)(nil)

// NewTaskInstancePhotoRepository constructs the repository with an injected
// query executor.
func NewTaskInstancePhotoRepository(dbtx db.TX) *TaskInstancePhotoRepository {
	if dbtx == nil {
		panic("media/adapter: NewTaskInstancePhotoRepository requires a non-nil db.TX")
	}
	return &TaskInstancePhotoRepository{dbtx: dbtx}
}

const taskInstancePhotoColumns = `
	SELECT id, household_id, task_instance_id, kind, storage_ref, content_sha256,
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
// matters: without a "before" insert also taking the lock, a "before"
// insert racing an "after" upload's check-then-insert could land in the gap
// between the after's read of the latest "before" and its own insert,
// letting a decision that was accurate when read become stale by the time
// it commits. With every Create for the instance serialized through the
// same lock, an "after" upload's read of the latest "before" (below) is
// always consistent with whatever is genuinely committed at that instant —
// a concurrent Create for the same instance either already committed
// before this lock was acquired (and so is visible to the read) or is
// still blocked waiting for this transaction to release the lock (and so
// cannot yet have committed). The lock auto-releases at commit or
// rollback; different instances never contend with each other (see
// choreProofInstanceLockNamespace's doc).
//
// Maps an unknown/foreign task instance to domain.ErrTaskInstanceNotFound
// and an unknown uploader to household.ErrMemberNotFound (both via the
// composite tenant FK violations), and — for photo.Kind ==
// domain.PhotoKindAfter — domain.ErrAfterPrecedesBefore when photo.TakenAt
// is earlier than the instance's most recent domain.PhotoKindBefore photo.
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

	if photo.Kind == domain.PhotoKindAfter {
		beforeTakenAt, ok, err := latestTakenAt(ctx, tx, photo.HouseholdID, photo.TaskInstanceID, domain.PhotoKindBefore)
		if err != nil {
			return fmt.Errorf("create task instance photo: check before photo: %w", err)
		}
		if ok && domain.AfterPrecedesBefore(photo.TakenAt, beforeTakenAt) {
			return domain.ErrAfterPrecedesBefore
		}
	}

	const q = `
		INSERT INTO task_instance_photo
			(id, household_id, task_instance_id, kind, storage_ref, content_sha256,
			 size_bytes, content_type, taken_at, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING uploaded_at`
	err = tx.QueryRow(ctx, q,
		photo.ID.String(), photo.HouseholdID.String(), photo.TaskInstanceID.String(), photo.Kind.String(),
		photo.StorageRef.String(), photo.ContentHash, photo.SizeBytes, photo.ContentType,
		photo.TakenAt, memberArg(photo.UploadedBy),
	).Scan(&photo.UploadedAt)
	if err != nil {
		if mapped := mapFKViolation(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("create task instance photo: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("create task instance photo: commit: %w", err)
	}
	return nil
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
// (against the plain pool) and Create's transactional before/after check
// (against its own tx) — q is whichever queryRower the caller holds.
func latestTakenAt(ctx context.Context, q queryRower, householdID household.HouseholdID, taskInstanceID domain.TaskInstanceID, kind domain.PhotoKind) (time.Time, bool, error) {
	const query = `
		SELECT taken_at FROM task_instance_photo
		 WHERE household_id = $1 AND task_instance_id = $2 AND kind = $3
		 ORDER BY taken_at DESC
		 LIMIT 1`
	var takenAt time.Time
	err := q.QueryRow(ctx, query, householdID.String(), taskInstanceID.String(), kind.String()).Scan(&takenAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("latest taken_at: %w", err)
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

func scanTaskInstancePhoto(r row) (*domain.TaskInstancePhoto, error) {
	var (
		photo         domain.TaskInstancePhoto
		idStr         string
		hhStr         string
		instanceStr   string
		kindStr       string
		storageRef    string
		uploadedByStr *string
	)
	if err := r.Scan(
		&idStr, &hhStr, &instanceStr, &kindStr, &storageRef, &photo.ContentHash,
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
	photo.ID = id
	photo.HouseholdID = hh
	photo.TaskInstanceID = instanceID
	photo.Kind = kind
	photo.StorageRef = domain.StorageRef(storageRef)
	if uploadedByStr != nil {
		memberID, err := household.ParseMemberID(*uploadedByStr)
		if err != nil {
			return nil, fmt.Errorf("parse uploader id: %w", err)
		}
		photo.UploadedBy = &memberID
	}
	return &photo, nil
}
