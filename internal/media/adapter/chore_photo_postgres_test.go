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
	repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
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

// TestTaskInstancePhotoRepositoryCreateStampsConfiguredBackend mirrors
// TestPhotoRepositoryCreateStampsConfiguredBackend (postgres_test.go) one
// table over: Create stamps storage_backend from the repository's OWN
// configured backend, never the column DEFAULT, verified on both the
// in-place struct Create populates and a fresh Get.
func TestTaskInstancePhotoRepositoryCreateStampsConfiguredBackend(t *testing.T) {
	pool := newTestPool(t)
	hh := seedHousehold(t, pool)
	instance := seedTaskInstance(t, pool, hh)
	ctx := testCtx(t)

	cases := []struct {
		name    string
		backend domain.StorageBackend
	}{
		{"local backend", domain.StorageBackendLocal},
		{"s3 backend", domain.StorageBackendS3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := adapter.NewTaskInstancePhotoRepository(pool, tc.backend)
			photo := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, time.Now().UTC(), nil)
			photo.ContentHash = fakeHash("backend-stamp-" + tc.name)
			if err := repo.Create(ctx, photo); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if photo.StorageBackend != tc.backend {
				t.Fatalf("Create did not stamp photo.StorageBackend: got %q, want %q", photo.StorageBackend, tc.backend)
			}

			got, err := repo.Get(ctx, photo.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.StorageBackend != tc.backend {
				t.Fatalf("Get returned StorageBackend %q, want %q (the persisted column value)", got.StorageBackend, tc.backend)
			}
			// Clean up the 's3'-tagged row explicitly — see
			// TestPhotoRepositoryCreateStampsConfiguredBackend's identical
			// comment for why a leftover 's3' row would corrupt the shared
			// test harness's cleanup. TaskInstancePhotoRepository has no
			// plain Delete-by-id; DeleteUploadedBefore with a cutoff just
			// past this row's UploadedAt removes exactly it.
			if _, err := repo.DeleteUploadedBefore(ctx, photo.UploadedAt.Add(time.Second)); err != nil {
				t.Fatalf("cleanup DeleteUploadedBefore: %v", err)
			}
		})
	}
}

// TestNewTaskInstancePhotoRepositoryRejectsInvalidBackend mirrors
// TestNewPhotoRepositoryRejectsInvalidBackend: an unknown StorageBackend
// must fail loudly at construction.
func TestNewTaskInstancePhotoRepositoryRejectsInvalidBackend(t *testing.T) {
	pool := newTestPool(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewTaskInstancePhotoRepository with an invalid backend should have panicked")
		}
	}()
	adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackend("azure-blob"))
}

// TestTaskInstancePhotoRepositoryGet verifies NES-120's raw-serving lookup:
// Get returns the exact photo by id, and ErrTaskInstancePhotoNotFound for an
// unknown id. Get is deliberately ID-only (mirrors PhotoRepository.Get) —
// household ownership is enforced by ChoreProofPhotoService.OpenBytes, not
// this repository, so a cross-household id is NOT exercised here; see
// chore_photo_service_test.go's OpenBytes tests for that enforcement.
func TestTaskInstancePhotoRepositoryGet(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Alex")
	instance := seedTaskInstance(t, pool, hh)
	ctx := testCtx(t)

	photo := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC), &member)
	if err := repo.Create(ctx, photo); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, photo.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != photo.ID || got.Kind != domain.PhotoKindAfter || got.ContentType != domain.ContentTypeJPEG {
		t.Errorf("Get returned mismatched photo: %+v", got)
	}
	if got.HouseholdID != hh {
		t.Errorf("Get: HouseholdID = %v, want %v (still returned so the caller can check ownership)", got.HouseholdID, hh)
	}

	if _, err := repo.Get(ctx, domain.NewTaskInstancePhotoID()); !errors.Is(err, domain.ErrTaskInstancePhotoNotFound) {
		t.Errorf("Get(unknown id) = %v, want ErrTaskInstancePhotoNotFound", err)
	}
}

// TestTaskInstancePhotoRepositoryListByInstances verifies NES-120's batch
// lookup: one query returns every photo across MULTIPLE instances,
// household-scoped, an empty result for an instance with none, and an empty
// slice (no query) for an empty id list — the N+1 avoidance the /tasks list
// builder relies on.
func TestTaskInstancePhotoRepositoryListByInstances(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Alex")
	instanceA := seedTaskInstance(t, pool, hh)
	instanceB := seedTaskInstance(t, pool, hh)
	instanceC := seedTaskInstance(t, pool, hh) // no photos at all
	ctx := testCtx(t)

	photoA := newTaskInstancePhoto(hh, instanceA, domain.PhotoKindBefore, time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC), &member)
	if err := repo.Create(ctx, photoA); err != nil {
		t.Fatalf("Create photoA: %v", err)
	}
	photoB := newTaskInstancePhoto(hh, instanceB, domain.PhotoKindAfter, time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC), &member)
	if err := repo.Create(ctx, photoB); err != nil {
		t.Fatalf("Create photoB: %v", err)
	}

	// A photo for an instance NOT in the requested list must never leak in.
	otherInstance := seedTaskInstance(t, pool, hh)
	otherPhoto := newTaskInstancePhoto(hh, otherInstance, domain.PhotoKindBefore, time.Date(2026, 3, 3, 8, 0, 0, 0, time.UTC), &member)
	if err := repo.Create(ctx, otherPhoto); err != nil {
		t.Fatalf("Create otherPhoto: %v", err)
	}

	got, err := repo.ListByInstances(ctx, hh, []domain.TaskInstanceID{instanceA, instanceB, instanceC})
	if err != nil {
		t.Fatalf("ListByInstances: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByInstances returned %d photos, want 2: %+v", len(got), got)
	}
	byID := make(map[domain.TaskInstancePhotoID]*domain.TaskInstancePhoto, len(got))
	for _, p := range got {
		byID[p.ID] = p
	}
	if _, ok := byID[photoA.ID]; !ok {
		t.Errorf("ListByInstances missing photoA: %+v", got)
	}
	if _, ok := byID[photoB.ID]; !ok {
		t.Errorf("ListByInstances missing photoB: %+v", got)
	}
	if _, ok := byID[otherPhoto.ID]; ok {
		t.Errorf("ListByInstances leaked a photo from an instance not in the request: %+v", got)
	}

	empty, err := repo.ListByInstances(ctx, hh, nil)
	if err != nil {
		t.Fatalf("ListByInstances(empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListByInstances(empty) = %d photos, want 0", len(empty))
	}
}

func TestTaskInstancePhotoRepositoryLatestTakenAt(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
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
	repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
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
	repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
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
	repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
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

// TestTaskInstancePhotoRepositoryListAllStorageRefs covers the storage
// reaper's chore-proof source of truth (NES-132,
// ReaperService.referencedRefs): every chore-proof photo's StorageRef,
// across every household, and an empty (not nil) slice when there are none.
func TestTaskInstancePhotoRepositoryListAllStorageRefs(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
	ctx := testCtx(t)

	if refs, err := repo.ListAllStorageRefs(ctx); err != nil || len(refs) != 0 {
		t.Fatalf("ListAllStorageRefs on an empty table = %v (err %v), want an empty slice", refs, err)
	}

	hhA := seedHousehold(t, pool)
	hhB := seedHousehold(t, pool)
	instanceA := seedTaskInstance(t, pool, hhA)
	instanceB := seedTaskInstance(t, pool, hhB)

	photoA := newTaskInstancePhoto(hhA, instanceA, domain.PhotoKindBefore, time.Now().UTC(), nil)
	photoA.StorageRef = domain.StorageRef("households/" + hhA.String() + "/chore-photos/aa/one.jpg")
	photoA.ContentHash = fakeHash("refs-chore-one")
	photoB := newTaskInstancePhoto(hhB, instanceB, domain.PhotoKindBefore, time.Now().UTC(), nil)
	photoB.StorageRef = domain.StorageRef("households/" + hhB.String() + "/chore-photos/bb/two.jpg")
	photoB.ContentHash = fakeHash("refs-chore-two")
	if err := repo.Create(ctx, photoA); err != nil {
		t.Fatalf("Create photoA: %v", err)
	}
	if err := repo.Create(ctx, photoB); err != nil {
		t.Fatalf("Create photoB: %v", err)
	}

	refs, err := repo.ListAllStorageRefs(ctx)
	if err != nil {
		t.Fatalf("ListAllStorageRefs: %v", err)
	}
	want := map[domain.StorageRef]bool{photoA.StorageRef: true, photoB.StorageRef: true}
	if len(refs) != 2 {
		t.Fatalf("ListAllStorageRefs = %v, want exactly 2 refs across both households", refs)
	}
	for _, ref := range refs {
		if !want[ref] {
			t.Fatalf("ListAllStorageRefs returned unexpected ref %q", ref)
		}
		delete(want, ref)
	}
	if len(want) != 0 {
		t.Fatalf("ListAllStorageRefs missing refs: %v", want)
	}
}

// TestTaskInstancePhotoRepositoryDeleteUploadedBefore covers the optional
// per-class retention pass (NES-132, ReaperService): a row uploaded before
// cutoff is removed and counted; a row uploaded on/after cutoff survives.
// uploaded_at is server-assigned (DEFAULT now()) by Create, so this seeds
// two rows, captures the second's actual uploaded_at as the cutoff, and
// asserts only the first (necessarily earlier) row is deleted.
func TestTaskInstancePhotoRepositoryDeleteUploadedBefore(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
	hh := seedHousehold(t, pool)
	instance := seedTaskInstance(t, pool, hh)
	ctx := testCtx(t)

	older := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, time.Now().UTC(), nil)
	older.ContentHash = fakeHash("retention-older")
	if err := repo.Create(ctx, older); err != nil {
		t.Fatalf("Create older: %v", err)
	}

	newer := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, time.Now().UTC(), nil)
	newer.ContentHash = fakeHash("retention-newer")
	if err := repo.Create(ctx, newer); err != nil {
		t.Fatalf("Create newer: %v", err)
	}

	// Use the server-assigned newer.UploadedAt (Create's own RETURNING
	// uploaded_at) as the cutoff, rather than a wall-clock time.Now()
	// bracketed by sleeps: this is deterministic and CI-pause-proof, and
	// only relies on the ordering guarantee Postgres itself already gives —
	// each Create runs in its own implicit (autocommit) transaction, so
	// newer's now() genuinely postdates older's.
	if !older.UploadedAt.Before(newer.UploadedAt) {
		t.Fatalf("test precondition failed: older.UploadedAt (%v) is not before newer.UploadedAt (%v)", older.UploadedAt, newer.UploadedAt)
	}

	n, err := repo.DeleteUploadedBefore(ctx, newer.UploadedAt)
	if err != nil {
		t.Fatalf("DeleteUploadedBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteUploadedBefore deleted %d rows, want 1", n)
	}

	remaining, err := repo.ListByInstance(ctx, hh, instance)
	if err != nil {
		t.Fatalf("ListByInstance: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != newer.ID {
		t.Fatalf("remaining rows = %+v, want only the newer row", remaining)
	}
}

// TestChoreProofPhotoNeverAppearsInAlbumQueries covers AC5 structurally: a
// chore-proof upload lands in task_instance_photo, a table entirely separate
// from photo/album_photo, so PhotoRepository.ListByHousehold (the query the
// /photos album page reads) never returns it.
func TestChoreProofPhotoNeverAppearsInAlbumQueries(t *testing.T) {
	pool := newTestPool(t)
	choreRepo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
	albumPhotoRepo := adapter.NewPhotoRepository(pool, domain.StorageBackendLocal)
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
// the NES-119 atomicity review — see Create's own doc), from BOTH insertion
// directions (see ErrAfterPrecedesBefore's doc for why the check is
// symmetric): an "after" photo earlier than the instance's latest "before"
// is rejected, and — the direction the NES-119 review round 2 added — a
// "before" photo later than the instance's earliest "after" is ALSO
// rejected; the non-violating and no-counterpart-yet cases succeed in both
// directions.
func TestTaskInstancePhotoRepositoryCreateEnforcesOrdering(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	base := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)

	t.Run("after earlier than before is rejected", func(t *testing.T) {
		instance := seedTaskInstance(t, pool, hh)
		if err := repo.Create(ctx, newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, base, nil)); err != nil {
			t.Fatalf("seed before: %v", err)
		}
		violating := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, base.Add(-1*time.Minute), nil)
		if err := repo.Create(ctx, violating); !errors.Is(err, domain.ErrAfterPrecedesBefore) {
			t.Fatalf("Create(violating after) = %v, want ErrAfterPrecedesBefore", err)
		}
	})

	t.Run("after at or later than before succeeds", func(t *testing.T) {
		instance := seedTaskInstance(t, pool, hh)
		if err := repo.Create(ctx, newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, base, nil)); err != nil {
			t.Fatalf("seed before: %v", err)
		}
		valid := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, base.Add(1*time.Minute), nil)
		if err := repo.Create(ctx, valid); err != nil {
			t.Fatalf("Create(valid after): %v", err)
		}
	})

	t.Run("after with no prior before succeeds", func(t *testing.T) {
		instance := seedTaskInstance(t, pool, hh)
		afterOnly := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, base, nil)
		if err := repo.Create(ctx, afterOnly); err != nil {
			t.Fatalf("Create(after, no before): %v", err)
		}
	})

	t.Run("before later than an existing after is rejected", func(t *testing.T) {
		instance := seedTaskInstance(t, pool, hh)
		if err := repo.Create(ctx, newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, base, nil)); err != nil {
			t.Fatalf("seed after: %v", err)
		}
		violating := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, base.Add(1*time.Minute), nil)
		if err := repo.Create(ctx, violating); !errors.Is(err, domain.ErrAfterPrecedesBefore) {
			t.Fatalf("Create(violating before) = %v, want ErrAfterPrecedesBefore", err)
		}
	})

	t.Run("before at or earlier than an existing after succeeds", func(t *testing.T) {
		instance := seedTaskInstance(t, pool, hh)
		if err := repo.Create(ctx, newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, base, nil)); err != nil {
			t.Fatalf("seed after: %v", err)
		}
		valid := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, base.Add(-1*time.Minute), nil)
		if err := repo.Create(ctx, valid); err != nil {
			t.Fatalf("Create(valid before): %v", err)
		}
	})

	t.Run("before with no existing after succeeds", func(t *testing.T) {
		instance := seedTaskInstance(t, pool, hh)
		beforeOnly := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, base, nil)
		if err := repo.Create(ctx, beforeOnly); err != nil {
			t.Fatalf("Create(before, no after): %v", err)
		}
	})
}

// TestTaskInstancePhotoRepositoryCreateSerializesOrderingUnderConcurrency is
// the gated regression test for the NES-119 atomicity review (MAJOR finding
// #1, round 2): it specifically exercises the check-then-insert race the
// per-task-instance pg_advisory_xact_lock exists to close, rather than a
// scenario a single, already-committed seed row would have rejected on its
// own regardless of any locking.
//
// Each attempt seeds NO photos at all for a fresh instance, then races two
// concurrent Creates against each other: a "before" at base+10m and an
// "after" at base+5m (earlier). Without any synchronization, both
// transactions' ordering checks can run before either has committed —
// before's check ("is there an existing after I'd violate?") sees none yet,
// and after's check ("is there an existing before I'd violate?") ALSO sees
// none yet — so BOTH would pass their own check and both would commit,
// leaving a persisted after (base+5m) that precedes a persisted before
// (base+10m): a genuine invariant violation neither individual check caught
// because each was only ever comparing itself against what existed AT ITS
// OWN read, not against the other write racing it. The per-instance
// advisory lock closes exactly this gap: whichever Create acquires the lock
// first commits (with nothing yet to conflict with, so it always succeeds),
// and the second, once unblocked, now reads a state that includes the
// first's already-committed row — so it is correctly rejected. This holds
// for EITHER winner (see the symmetric check argued in Create's own doc),
// so exactly one of the two racing writes must succeed on every attempt,
// never both and never neither.
func TestTaskInstancePhotoRepositoryCreateSerializesOrderingUnderConcurrency(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	const attempts = 15
	for i := 0; i < attempts; i++ {
		instance := seedTaskInstance(t, pool, hh)
		base := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
		before := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, base.Add(10*time.Minute), nil)
		after := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, base.Add(5*time.Minute), nil)

		var wg sync.WaitGroup
		var beforeErr, afterErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			beforeErr = repo.Create(ctx, before)
		}()
		go func() {
			defer wg.Done()
			afterErr = repo.Create(ctx, after)
		}()
		wg.Wait()

		beforeOK := beforeErr == nil
		afterOK := afterErr == nil
		if beforeOK == afterOK {
			t.Fatalf("attempt %d: before ok=%v (err=%v), after ok=%v (err=%v) — want exactly one to succeed and the other rejected with ErrAfterPrecedesBefore", i, beforeOK, beforeErr, afterOK, afterErr)
		}
		if !beforeOK && !errors.Is(beforeErr, domain.ErrAfterPrecedesBefore) {
			t.Fatalf("attempt %d: before Create failed with %v, want ErrAfterPrecedesBefore", i, beforeErr)
		}
		if !afterOK && !errors.Is(afterErr, domain.ErrAfterPrecedesBefore) {
			t.Fatalf("attempt %d: after Create failed with %v, want ErrAfterPrecedesBefore", i, afterErr)
		}

		list, err := repo.ListByInstance(ctx, hh, instance)
		if err != nil {
			t.Fatalf("attempt %d: ListByInstance: %v", i, err)
		}
		assertNoOrderingViolation(t, i, list)
	}
}

// assertNoOrderingViolation fails the test if photos' persisted state has an
// "after" whose taken_at precedes any "before"'s taken_at for the same
// instance — the invariant ErrAfterPrecedesBefore exists to protect,
// checked directly against what actually got committed rather than
// trusting the two Create calls' own return values alone.
func assertNoOrderingViolation(t *testing.T, attempt int, photos []*domain.TaskInstancePhoto) {
	t.Helper()
	var latestBefore, earliestAfter *time.Time
	for _, p := range photos {
		switch p.Kind {
		case domain.PhotoKindBefore:
			if latestBefore == nil || p.TakenAt.After(*latestBefore) {
				ta := p.TakenAt
				latestBefore = &ta
			}
		case domain.PhotoKindAfter:
			if earliestAfter == nil || p.TakenAt.Before(*earliestAfter) {
				ta := p.TakenAt
				earliestAfter = &ta
			}
		}
	}
	if latestBefore != nil && earliestAfter != nil && domain.AfterPrecedesBefore(*earliestAfter, *latestBefore) {
		t.Fatalf("attempt %d: persisted state violates the invariant: earliest after %s precedes latest before %s", attempt, earliestAfter, latestBefore)
	}
}
