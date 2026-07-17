package adapter_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/adapter"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// seedRecurringTask inserts a minimal recurring_task row, the parent every
// task_instance needs.
func seedRecurringTask(t *testing.T, pool *pgxpool.Pool, hh household.HouseholdID) string {
	t.Helper()
	id := domain.NewTaskInstancePhotoID().String() // any fresh UUID string
	if _, err := pool.Exec(testCtx(t),
		`INSERT INTO recurring_task (id, household_id, title, category, cadence, rotation_policy)
		 VALUES ($1, $2, 'Dishes', 'chore', '{}'::jsonb, 'claimable')`,
		id, hh.String()); err != nil {
		t.Fatalf("seed recurring task: %v", err)
	}
	return id
}

// seedTaskInstance inserts a minimal task_instance row scoped to hh, returning
// its id as a domain.TaskInstanceID (media's own reference type).
func seedTaskInstance(t *testing.T, pool *pgxpool.Pool, hh household.HouseholdID) domain.TaskInstanceID {
	t.Helper()
	recurringTaskID := seedRecurringTask(t, pool, hh)
	instanceID := domain.NewTaskInstancePhotoID().String()
	if _, err := pool.Exec(testCtx(t),
		`INSERT INTO task_instance (id, household_id, recurring_task_id, due_on, status)
		 VALUES ($1, $2, $3, CURRENT_DATE, 'pending')`,
		instanceID, hh.String(), recurringTaskID); err != nil {
		t.Fatalf("seed task instance: %v", err)
	}
	id, err := domain.ParseTaskInstanceID(instanceID)
	if err != nil {
		t.Fatalf("parse seeded task instance id: %v", err)
	}
	return id
}

// freshTaskInstanceID returns a syntactically valid TaskInstanceID that was
// never inserted into task_instance — used to exercise the "unknown
// instance" FK-violation path.
func freshTaskInstanceID(t *testing.T) domain.TaskInstanceID {
	t.Helper()
	id, err := domain.ParseTaskInstanceID(domain.NewTaskInstancePhotoID().String())
	if err != nil {
		t.Fatalf("build a fresh task instance id: %v", err)
	}
	return id
}

func newTaskInstancePhoto(hh household.HouseholdID, instanceID domain.TaskInstanceID, kind domain.PhotoKind, taken time.Time, uploader *household.MemberID) *domain.TaskInstancePhoto {
	return &domain.TaskInstancePhoto{
		ID:             domain.NewTaskInstancePhotoID(),
		HouseholdID:    hh,
		TaskInstanceID: instanceID,
		Kind:           kind,
		StorageRef:     domain.StorageRef("hh/chore-photos/aa/abc.jpg"),
		ContentHash:    fakeHash(kind.String() + taken.String()),
		SizeBytes:      2048,
		ContentType:    domain.ContentTypeJPEG,
		TakenAt:        taken,
		UploadedBy:     uploader,
	}
}

func TestTaskInstancePhotoRepositoryCreateAndList(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool)
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Alex")
	instance := seedTaskInstance(t, pool, hh)
	ctx := testCtx(t)

	before := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	after := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	beforePhoto := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, before, &member)
	if err := repo.Create(ctx, beforePhoto); err != nil {
		t.Fatalf("Create before photo: %v", err)
	}
	if beforePhoto.UploadedAt.IsZero() {
		t.Fatal("Create did not populate UploadedAt")
	}
	afterPhoto := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, after, &member)
	if err := repo.Create(ctx, afterPhoto); err != nil {
		t.Fatalf("Create after photo: %v", err)
	}

	list, err := repo.ListByInstance(ctx, hh, instance)
	if err != nil {
		t.Fatalf("ListByInstance: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListByInstance = %d photos, want 2", len(list))
	}
	if list[0].ID != beforePhoto.ID || list[1].ID != afterPhoto.ID {
		t.Fatalf("ListByInstance order = %+v, want [before, after] by taken_at ascending", list)
	}
	if list[0].UploadedBy == nil || *list[0].UploadedBy != member {
		t.Fatalf("uploader attribution wrong: %+v", list[0])
	}
}

func TestTaskInstancePhotoRepositoryLatestTakenAt(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool)
	hh := seedHousehold(t, pool)
	instance := seedTaskInstance(t, pool, hh)
	ctx := testCtx(t)

	// No "before" photo yet.
	if _, ok, err := repo.LatestTakenAt(ctx, hh, instance, domain.PhotoKindBefore); err != nil || ok {
		t.Fatalf("LatestTakenAt with no photos = ok:%v err:%v, want ok:false", ok, err)
	}

	earlier := time.Date(2026, 3, 1, 7, 0, 0, 0, time.UTC)
	later := time.Date(2026, 3, 1, 7, 30, 0, 0, time.UTC)
	if err := repo.Create(ctx, newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, earlier, nil)); err != nil {
		t.Fatalf("Create first before photo: %v", err)
	}
	if err := repo.Create(ctx, newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, later, nil)); err != nil {
		t.Fatalf("Create second before photo: %v", err)
	}

	got, ok, err := repo.LatestTakenAt(ctx, hh, instance, domain.PhotoKindBefore)
	if err != nil || !ok {
		t.Fatalf("LatestTakenAt = ok:%v err:%v, want ok:true", ok, err)
	}
	if !got.Equal(later) {
		t.Fatalf("LatestTakenAt = %s, want the most recent (%s), not the first", got, later)
	}

	// "after" kind is unaffected by "before" rows for the same instance.
	if _, ok, err := repo.LatestTakenAt(ctx, hh, instance, domain.PhotoKindAfter); err != nil || ok {
		t.Fatalf("LatestTakenAt(after) with no after photos = ok:%v err:%v, want ok:false", ok, err)
	}
}

// TestTaskInstancePhotoRepositoryInstanceExists covers the preflight
// convenience (NES-119 review, design resolution A): an existing,
// household-scoped instance reports true; an unknown id and a
// cross-household id (which the FK-based Create check cannot distinguish
// from "unknown" — both surface as domain.ErrTaskInstanceNotFound there)
// both report false here, and household-scoping is exercised directly.
func TestTaskInstancePhotoRepositoryInstanceExists(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool)
	hhA := seedHousehold(t, pool)
	hhB := seedHousehold(t, pool)
	instance := seedTaskInstance(t, pool, hhA)
	ctx := testCtx(t)

	if exists, err := repo.InstanceExists(ctx, hhA, instance); err != nil || !exists {
		t.Fatalf("InstanceExists(owning household) = %v, %v, want true, nil", exists, err)
	}
	if exists, err := repo.InstanceExists(ctx, hhA, freshTaskInstanceID(t)); err != nil || exists {
		t.Fatalf("InstanceExists(unknown id) = %v, %v, want false, nil", exists, err)
	}
	if exists, err := repo.InstanceExists(ctx, hhB, instance); err != nil || exists {
		t.Fatalf("InstanceExists(cross-household) = %v, %v, want false, nil", exists, err)
	}
}

func TestTaskInstancePhotoRepositoryCreateUnknownInstanceAndUploader(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool)
	hh := seedHousehold(t, pool)
	instance := seedTaskInstance(t, pool, hh)
	ctx := testCtx(t)

	photo := newTaskInstancePhoto(hh, freshTaskInstanceID(t), domain.PhotoKindBefore, time.Now().UTC(), nil)
	if err := repo.Create(ctx, photo); !errors.Is(err, domain.ErrTaskInstanceNotFound) {
		t.Fatalf("Create with unknown instance = %v, want ErrTaskInstanceNotFound", err)
	}

	stranger := household.NewMemberID()
	photo2 := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, time.Now().UTC(), &stranger)
	if err := repo.Create(ctx, photo2); !errors.Is(err, household.ErrMemberNotFound) {
		t.Fatalf("Create with unknown uploader = %v, want ErrMemberNotFound", err)
	}
}

func TestTaskInstancePhotoRepositoryCrossHouseholdInstanceRejected(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool)
	hhA := seedHousehold(t, pool)
	hhB := seedHousehold(t, pool)
	instanceInA := seedTaskInstance(t, pool, hhA)
	ctx := testCtx(t)

	// household_id/task_instance_id must belong to the SAME household — the
	// composite tenant FK makes a cross-household reference impossible even
	// when both ids individually exist.
	photo := newTaskInstancePhoto(hhB, instanceInA, domain.PhotoKindBefore, time.Now().UTC(), nil)
	if err := repo.Create(ctx, photo); !errors.Is(err, domain.ErrTaskInstanceNotFound) {
		t.Fatalf("Create with cross-household instance = %v, want ErrTaskInstanceNotFound", err)
	}
}

// TestChoreProofPhotoNeverAppearsInAlbumQueries covers AC5 structurally: a
// chore-proof upload lands in task_instance_photo, a table entirely separate
// from photo/album_photo, so PhotoRepository.ListByHousehold (the query the
// /photos album page reads) never returns it.
func TestChoreProofPhotoNeverAppearsInAlbumQueries(t *testing.T) {
	pool := newTestPool(t)
	choreRepo := adapter.NewTaskInstancePhotoRepository(pool)
	albumPhotoRepo := adapter.NewPhotoRepository(pool)
	hh := seedHousehold(t, pool)
	instance := seedTaskInstance(t, pool, hh)
	ctx := testCtx(t)

	if err := choreRepo.Create(ctx, newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, time.Now().UTC(), nil)); err != nil {
		t.Fatalf("Create chore proof photo: %v", err)
	}

	list, err := albumPhotoRepo.ListByHousehold(ctx, hh)
	if err != nil {
		t.Fatalf("PhotoRepository.ListByHousehold: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("album ListByHousehold returned %d photos, want 0 — a chore-proof photo must never surface in the album/gallery query", len(list))
	}
}

// TestTaskInstancePhotoRepositoryCreateEnforcesOrdering covers AC3's second
// half at the repository layer (moved here from the app-service layer by
// the NES-119 atomicity review — see Create's own doc): an "after" photo
// earlier than the instance's most recent "before" is rejected with
// ErrAfterPrecedesBefore, one at or after it succeeds, and an "after" with
// no prior "before" at all succeeds (nothing to compare against).
func TestTaskInstancePhotoRepositoryCreateEnforcesOrdering(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	beforeTaken := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)

	t.Run("after earlier than before is rejected", func(t *testing.T) {
		instance := seedTaskInstance(t, pool, hh)
		if err := repo.Create(ctx, newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, beforeTaken, nil)); err != nil {
			t.Fatalf("seed before: %v", err)
		}
		violating := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, beforeTaken.Add(-1*time.Minute), nil)
		if err := repo.Create(ctx, violating); !errors.Is(err, domain.ErrAfterPrecedesBefore) {
			t.Fatalf("Create(violating after) = %v, want ErrAfterPrecedesBefore", err)
		}
	})

	t.Run("after at or later than before succeeds", func(t *testing.T) {
		instance := seedTaskInstance(t, pool, hh)
		if err := repo.Create(ctx, newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, beforeTaken, nil)); err != nil {
			t.Fatalf("seed before: %v", err)
		}
		valid := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, beforeTaken.Add(1*time.Minute), nil)
		if err := repo.Create(ctx, valid); err != nil {
			t.Fatalf("Create(valid after): %v", err)
		}
	})

	t.Run("after with no prior before succeeds", func(t *testing.T) {
		instance := seedTaskInstance(t, pool, hh)
		afterOnly := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, beforeTaken, nil)
		if err := repo.Create(ctx, afterOnly); err != nil {
			t.Fatalf("Create(after, no before): %v", err)
		}
	})
}

// TestTaskInstancePhotoRepositoryCreateSerializesOrderingUnderConcurrency is
// the gated regression test for the NES-119 atomicity review (MAJOR finding
// #1): the before/after ordering check and the insert happen atomically,
// inside one transaction, under a per-task-instance pg_advisory_xact_lock
// that EVERY Create for that instance acquires regardless of Kind — not
// just an "after" upload.
//
// Each attempt races a NEW "later before" insert (taken_at strictly after
// an already-seeded "before") against a violating "after" insert (taken_at
// strictly before even the ORIGINAL seeded "before", so it is guaranteed
// invalid no matter how the race with the concurrent later-before resolves).
// Without the per-instance lock serializing every Create for the instance,
// the violating after's read of "the latest before" could run in the gap
// before the concurrent later-before commits, observe only the (still
// valid-relative-to) original seed, and incorrectly succeed — exactly the
// TOCTOU race a separate read-then-insert pair (the pre-review app-service
// check) left open. With the lock, the violating after's check is always
// consistent with whatever is genuinely committed at that instant, so it
// must be rejected on every attempt, regardless of which goroutine's
// transaction happens to acquire the lock first.
func TestTaskInstancePhotoRepositoryCreateSerializesOrderingUnderConcurrency(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	const attempts = 15
	for i := 0; i < attempts; i++ {
		instance := seedTaskInstance(t, pool, hh)
		seed := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
		if err := repo.Create(ctx, newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, seed, nil)); err != nil {
			t.Fatalf("attempt %d: seed before: %v", i, err)
		}

		laterBefore := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, seed.Add(5*time.Minute), nil)
		violatingAfter := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, seed.Add(-1*time.Minute), nil)

		var wg sync.WaitGroup
		var afterErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = repo.Create(ctx, laterBefore) // outcome not asserted; it races the after below
		}()
		go func() {
			defer wg.Done()
			afterErr = repo.Create(ctx, violatingAfter)
		}()
		wg.Wait()

		if !errors.Is(afterErr, domain.ErrAfterPrecedesBefore) {
			t.Fatalf("attempt %d: violating after Create = %v, want ErrAfterPrecedesBefore (it precedes the seeded before regardless of the concurrent later-before's outcome)", i, afterErr)
		}
	}
}
