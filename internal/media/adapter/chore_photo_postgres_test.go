package adapter_test

import (
	"errors"
	"fmt"
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
			if _, err := repo.DeleteUploadedBefore(ctx, tc.backend, photo.UploadedAt.Add(time.Second)); err != nil {
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

	if refs, err := repo.ListAllStorageRefs(ctx, domain.StorageBackendLocal); err != nil || len(refs) != 0 {
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

	refs, err := repo.ListAllStorageRefs(ctx, domain.StorageBackendLocal)
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

// TestTaskInstancePhotoRepositoryListAllStorageRefsFiltersByBackend mirrors
// TestPhotoRepositoryListAllStorageRefsFiltersByBackend (postgres_test.go)
// one table over: content-addressed keys are identical across backends, so
// two rows can legitimately share one storage_ref while stamped with
// DIFFERENT backends — this proves ListAllStorageRefs and
// ExistsByStorageRef both filter on storage_backend, not just storage_ref.
func TestTaskInstancePhotoRepositoryListAllStorageRefsFiltersByBackend(t *testing.T) {
	pool := newTestPool(t)
	localRepo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
	s3Repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendS3)
	hh := seedHousehold(t, pool)
	instance := seedTaskInstance(t, pool, hh)
	ctx := testCtx(t)

	sharedRef := domain.StorageRef("households/" + hh.String() + "/chore-photos/aa/shared.jpg")
	now := time.Now().UTC()

	localPhoto := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, now.Add(-time.Hour), nil)
	localPhoto.StorageRef = sharedRef
	localPhoto.ContentHash = fakeHash("shared-local-chore")
	if err := localRepo.Create(ctx, localPhoto); err != nil {
		t.Fatalf("Create local-backed row: %v", err)
	}
	s3Photo := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, now, nil)
	s3Photo.StorageRef = sharedRef
	s3Photo.ContentHash = fakeHash("shared-s3-chore")
	if err := s3Repo.Create(ctx, s3Photo); err != nil {
		t.Fatalf("Create s3-backed row: %v", err)
	}
	// The down-migration for 00032 hard-aborts while any 's3' row lingers
	// (NES-132 review) — clean up explicitly. DeleteUploadedBefore is now
	// backend-scoped (NES-133/149), so a cutoff comfortably after both
	// rows' UploadedAt reliably removes ONLY the s3-backed row, with no
	// need to depend on upload-time ordering between the two rows the way
	// an earlier, unscoped version of this method required.
	t.Cleanup(func() { _, _ = s3Repo.DeleteUploadedBefore(ctx, domain.StorageBackendS3, now.Add(time.Hour)) })

	localRefs, err := localRepo.ListAllStorageRefs(ctx, domain.StorageBackendLocal)
	if err != nil {
		t.Fatalf("ListAllStorageRefs(local): %v", err)
	}
	if len(localRefs) != 1 || localRefs[0] != sharedRef {
		t.Fatalf("ListAllStorageRefs(local) = %v, want exactly [%s]", localRefs, sharedRef)
	}

	s3Refs, err := localRepo.ListAllStorageRefs(ctx, domain.StorageBackendS3)
	if err != nil {
		t.Fatalf("ListAllStorageRefs(s3): %v", err)
	}
	if len(s3Refs) != 1 || s3Refs[0] != sharedRef {
		t.Fatalf("ListAllStorageRefs(s3) = %v, want exactly [%s]", s3Refs, sharedRef)
	}

	if exists, err := localRepo.ExistsByStorageRef(ctx, sharedRef, domain.StorageBackendLocal); err != nil || !exists {
		t.Fatalf("ExistsByStorageRef(ref, local) = %v, %v, want true, nil", exists, err)
	}
	if exists, err := localRepo.ExistsByStorageRef(ctx, sharedRef, domain.StorageBackendS3); err != nil || !exists {
		t.Fatalf("ExistsByStorageRef(ref, s3) = %v, %v, want true, nil", exists, err)
	}

	// Removing only the s3-backed row must leave the local-backed row (same
	// ref) fully intact and still reported for the local backend.
	if _, err := s3Repo.DeleteUploadedBefore(ctx, domain.StorageBackendS3, now.Add(time.Hour)); err != nil {
		t.Fatalf("DeleteUploadedBefore (remove s3-backed row): %v", err)
	}
	if exists, err := localRepo.ExistsByStorageRef(ctx, sharedRef, domain.StorageBackendS3); err != nil || exists {
		t.Fatalf("ExistsByStorageRef(ref, s3) after deleting the s3 row = %v, %v, want false, nil", exists, err)
	}
	if exists, err := localRepo.ExistsByStorageRef(ctx, sharedRef, domain.StorageBackendLocal); err != nil || !exists {
		t.Fatalf("ExistsByStorageRef(ref, local) after deleting the UNRELATED s3 row = %v, %v, want true, nil (still referenced)", exists, err)
	}
}

// TestTaskInstancePhotoRepositoryDeleteUploadedBefore covers the optional
// per-class retention pass (NES-132, ReaperService): a LOCAL-backend row
// uploaded before cutoff is removed and counted; a LOCAL-backend row
// uploaded on/after cutoff survives; and — the NES-133/149 backend-scoping
// fix — an S3-backend row that is ALSO old enough survives a
// StorageBackendLocal-scoped call untouched, since retention must never
// delete a row belonging to a DIFFERENT backend than the one it was asked
// to scope to (doing so would strand that row's object with no
// same-backend reaper instance able to ever reclaim it). uploaded_at is
// server-assigned (DEFAULT now()) by Create, so this seeds three rows,
// captures the newest local row's actual uploaded_at as the cutoff, and
// asserts only the older LOCAL row is deleted.
func TestTaskInstancePhotoRepositoryDeleteUploadedBefore(t *testing.T) {
	pool := newTestPool(t)
	localRepo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
	s3Repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendS3)
	hh := seedHousehold(t, pool)
	instance := seedTaskInstance(t, pool, hh)
	ctx := testCtx(t)

	older := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, time.Now().UTC(), nil)
	older.ContentHash = fakeHash("retention-older")
	if err := localRepo.Create(ctx, older); err != nil {
		t.Fatalf("Create older: %v", err)
	}

	// An S3-backend row, also old, seeded between the two local rows so its
	// UploadedAt necessarily falls before the cutoff below too — it must
	// survive a StorageBackendLocal-scoped DeleteUploadedBefore regardless.
	oldS3 := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, time.Now().UTC(), nil)
	oldS3.ContentHash = fakeHash("retention-old-s3")
	if err := s3Repo.Create(ctx, oldS3); err != nil {
		t.Fatalf("Create old s3-backed row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s3Repo.DeleteUploadedBefore(ctx, domain.StorageBackendS3, time.Now().UTC().Add(time.Hour))
	})

	newer := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, time.Now().UTC(), nil)
	newer.ContentHash = fakeHash("retention-newer")
	if err := localRepo.Create(ctx, newer); err != nil {
		t.Fatalf("Create newer: %v", err)
	}

	// Use the server-assigned newer.UploadedAt (Create's own RETURNING
	// uploaded_at) as the cutoff, rather than a wall-clock time.Now()
	// bracketed by sleeps: this is deterministic and CI-pause-proof, and
	// only relies on the ordering guarantee Postgres itself already gives —
	// each Create runs in its own implicit (autocommit) transaction, so
	// newer's now() genuinely postdates older's (and oldS3's).
	if !older.UploadedAt.Before(newer.UploadedAt) {
		t.Fatalf("test precondition failed: older.UploadedAt (%v) is not before newer.UploadedAt (%v)", older.UploadedAt, newer.UploadedAt)
	}
	if !oldS3.UploadedAt.Before(newer.UploadedAt) {
		t.Fatalf("test precondition failed: oldS3.UploadedAt (%v) is not before newer.UploadedAt (%v)", oldS3.UploadedAt, newer.UploadedAt)
	}

	n, err := localRepo.DeleteUploadedBefore(ctx, domain.StorageBackendLocal, newer.UploadedAt)
	if err != nil {
		t.Fatalf("DeleteUploadedBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteUploadedBefore deleted %d rows, want 1", n)
	}

	remaining, err := localRepo.ListByInstance(ctx, hh, instance)
	if err != nil {
		t.Fatalf("ListByInstance: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining rows = %+v, want 2 (newer local row + the untouched s3 row)", remaining)
	}
	foundNewer, foundOldS3 := false, false
	for _, p := range remaining {
		switch p.ID {
		case newer.ID:
			foundNewer = true
		case oldS3.ID:
			foundOldS3 = true
		}
	}
	if !foundNewer {
		t.Fatal("the newer local row should have survived (not old enough for the cutoff)")
	}
	if !foundOldS3 {
		t.Fatal("the old s3-backed row should have survived: DeleteUploadedBefore(local, ...) must not delete an s3-backend row")
	}
}

// TestTaskInstancePhotoRepositoryListStorageRefsUploadedBefore mirrors
// TestTaskInstancePhotoRepositoryDeleteUploadedBefore's setup but asserts
// ListStorageRefsUploadedBefore (NES-133/149's ReaperService.DryRun
// retention preview) reports exactly the REFS DeleteUploadedBefore would
// remove for the SAME backend, WITHOUT removing anything — including the
// identical backend-scoping guarantee (an s3-backend row's ref is never
// reported by a StorageBackendLocal-scoped call, even when it is old
// enough).
func TestTaskInstancePhotoRepositoryListStorageRefsUploadedBefore(t *testing.T) {
	pool := newTestPool(t)
	localRepo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
	s3Repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendS3)
	hh := seedHousehold(t, pool)
	instance := seedTaskInstance(t, pool, hh)
	ctx := testCtx(t)

	older := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, time.Now().UTC(), nil)
	older.StorageRef = domain.StorageRef("households/" + hh.String() + "/chore-photos/aa/older.jpg")
	older.ContentHash = fakeHash("list-refs-older")
	if err := localRepo.Create(ctx, older); err != nil {
		t.Fatalf("Create older: %v", err)
	}
	oldS3 := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, time.Now().UTC(), nil)
	oldS3.StorageRef = domain.StorageRef("households/" + hh.String() + "/chore-photos/bb/old-s3.jpg")
	oldS3.ContentHash = fakeHash("list-refs-old-s3")
	if err := s3Repo.Create(ctx, oldS3); err != nil {
		t.Fatalf("Create old s3-backed row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s3Repo.DeleteUploadedBefore(ctx, domain.StorageBackendS3, time.Now().UTC().Add(time.Hour))
	})

	newer := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, time.Now().UTC(), nil)
	newer.ContentHash = fakeHash("list-refs-newer")
	if err := localRepo.Create(ctx, newer); err != nil {
		t.Fatalf("Create newer: %v", err)
	}
	if !older.UploadedAt.Before(newer.UploadedAt) || !oldS3.UploadedAt.Before(newer.UploadedAt) {
		t.Fatalf("test precondition failed: older/oldS3 UploadedAt must both precede newer.UploadedAt")
	}

	refs, err := localRepo.ListStorageRefsUploadedBefore(ctx, domain.StorageBackendLocal, newer.UploadedAt)
	if err != nil {
		t.Fatalf("ListStorageRefsUploadedBefore: %v", err)
	}
	if len(refs) != 1 || refs[0] != older.StorageRef {
		t.Fatalf("ListStorageRefsUploadedBefore(local) = %v, want exactly [%s]", refs, older.StorageRef)
	}

	// Nothing was actually deleted: all three rows (older + newer local,
	// plus the untouched s3 row — ListByInstance is not backend-filtered)
	// are still present.
	remaining, err := localRepo.ListByInstance(ctx, hh, instance)
	if err != nil {
		t.Fatalf("ListByInstance: %v", err)
	}
	if len(remaining) != 3 {
		t.Fatalf("ListStorageRefsUploadedBefore deleted rows: remaining = %d, want 3", len(remaining))
	}
}

// TestTaskInstancePhotoRepositoryListByBackend mirrors
// TestPhotoRepositoryListByBackend one table over: rows stamped with the
// requested backend, ordered by id ascending, respecting afterID and limit.
func TestTaskInstancePhotoRepositoryListByBackend(t *testing.T) {
	pool := newTestPool(t)
	localRepo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
	s3Repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendS3)
	hh := seedHousehold(t, pool)
	instance := seedTaskInstance(t, pool, hh)
	ctx := testCtx(t)

	const n = 3
	for i := 0; i < n; i++ {
		p := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, time.Now().UTC(), nil)
		p.StorageRef = domain.StorageRef(fmt.Sprintf("households/%s/chore-photos/aa/list-by-backend-%d.jpg", hh, i))
		p.ContentHash = fakeHash(fmt.Sprintf("list-by-backend-local-%d", i))
		if err := localRepo.Create(ctx, p); err != nil {
			t.Fatalf("Create local photo %d: %v", i, err)
		}
	}
	s3Photo := newTaskInstancePhoto(hh, instance, domain.PhotoKindAfter, time.Now().UTC(), nil)
	s3Photo.StorageRef = domain.StorageRef("households/" + hh.String() + "/chore-photos/bb/s3.jpg")
	s3Photo.ContentHash = fakeHash("list-by-backend-s3")
	if err := s3Repo.Create(ctx, s3Photo); err != nil {
		t.Fatalf("Create s3 photo: %v", err)
	}
	// Clean up the 's3'-tagged row explicitly: see
	// TestTaskInstancePhotoRepositoryCreateStampsConfiguredBackend's
	// identical comment for why a leftover 's3' row would corrupt the
	// shared test harness's Cleanup for every test that runs after this
	// one (also backstopped by newTestPool's own preResetSweep, but this
	// is the correct place to prevent the leak, not just paper over it).
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM task_instance_photo WHERE id = $1`, s3Photo.ID.String()) })

	var (
		afterID domain.TaskInstancePhotoID
		got     []*domain.TaskInstancePhoto
	)
	for {
		page, err := localRepo.ListByBackend(ctx, domain.StorageBackendLocal, afterID, 1)
		if err != nil {
			t.Fatalf("ListByBackend: %v", err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, page[0])
		afterID = page[0].ID
	}
	if len(got) != n {
		t.Fatalf("ListByBackend paged %d rows total, want %d", len(got), n)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ID.String() >= got[i].ID.String() {
			t.Fatalf("ListByBackend did not return ascending id order: %s then %s", got[i-1].ID, got[i].ID)
		}
	}

	s3Page, err := localRepo.ListByBackend(ctx, domain.StorageBackendS3, domain.TaskInstancePhotoID{}, 10)
	if err != nil {
		t.Fatalf("ListByBackend(s3): %v", err)
	}
	if len(s3Page) != 1 || s3Page[0].ID != s3Photo.ID {
		t.Fatalf("ListByBackend(s3) = %v, want exactly [%s]", s3Page, s3Photo.ID)
	}
}

// TestTaskInstancePhotoRepositoryMigrateStorageBackend covers NES-133's
// migrator's flip for the chore-proof table: a local-backend row is updated
// to newBackend/newRef, content_sha256 is left untouched (unlike
// PhotoRepository's counterpart, this table has no legacy-NULL case to
// backfill), and re-running against an already-migrated (or unknown) id is
// a safe no-op reporting done=false.
func TestTaskInstancePhotoRepositoryMigrateStorageBackend(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendLocal)
	hh := seedHousehold(t, pool)
	instance := seedTaskInstance(t, pool, hh)
	ctx := testCtx(t)

	photo := newTaskInstancePhoto(hh, instance, domain.PhotoKindBefore, time.Now().UTC(), nil)
	photo.StorageRef = domain.StorageRef("households/" + hh.String() + "/chore-photos/aa/migrate-me.jpg")
	photo.ContentHash = fakeHash("migrate-storage-backend")
	if err := repo.Create(ctx, photo); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// This row is flipped to s3-backend below; clean it up explicitly so
	// the shared test harness's Cleanup (which rolls migrations all the
	// way back, including 00032's abort-while-any-s3-row-lingers guard)
	// never trips over it — mirrors
	// TestPhotoRepositoryCreateStampsConfiguredBackend's identical pattern.
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM task_instance_photo WHERE id = $1`, photo.ID.String())
	})

	newRef := domain.StorageRef("households/" + hh.String() + "/chore-photos/aa/migrated.jpg")
	done, err := repo.MigrateStorageBackend(ctx, photo.ID, newRef, domain.StorageBackendS3)
	if err != nil {
		t.Fatalf("MigrateStorageBackend: %v", err)
	}
	if !done {
		t.Fatal("MigrateStorageBackend done = false, want true")
	}

	s3Repo := adapter.NewTaskInstancePhotoRepository(pool, domain.StorageBackendS3)
	got, err := s3Repo.Get(ctx, photo.ID)
	if err != nil {
		t.Fatalf("Get after migrate: %v", err)
	}
	if got.StorageBackend != domain.StorageBackendS3 || got.StorageRef != newRef {
		t.Fatalf("Get after migrate = backend %q ref %q, want s3 / %q", got.StorageBackend, got.StorageRef, newRef)
	}
	if got.ContentHash != fakeHash("migrate-storage-backend") {
		t.Fatalf("ContentHash changed: got %q, want unchanged", got.ContentHash)
	}

	doneAgain, err := repo.MigrateStorageBackend(ctx, photo.ID, "households/should-not-apply.jpg", domain.StorageBackendS3)
	if err != nil {
		t.Fatalf("second MigrateStorageBackend: %v", err)
	}
	if doneAgain {
		t.Fatal("second MigrateStorageBackend done = true, want false (row is no longer local-backend)")
	}

	ghostID := domain.NewTaskInstancePhotoID()
	doneGhost, err := repo.MigrateStorageBackend(ctx, ghostID, "households/ghost.jpg", domain.StorageBackendS3)
	if err != nil {
		t.Fatalf("MigrateStorageBackend on unknown id: %v", err)
	}
	if doneGhost {
		t.Fatal("MigrateStorageBackend on unknown id: done = true, want false")
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
