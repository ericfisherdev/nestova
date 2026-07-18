package adapter_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/adapter"
	"github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
)

// preResetSweep is a best-effort DELETE of any lingering s3-stamped photo/
// task_instance_photo rows, run immediately before EVERY migrate.Reset call
// in this package's test harness (both newTestPool's own setup and its
// t.Cleanup). Migration 00032's down-migration deliberately aborts
// (SQLSTATE P0001) while ANY row is stamped 's3' — correct, load-bearing
// behavior for a REAL production rollback (an operator must migrate those
// rows' bytes back to local storage and re-stamp them 'local' first) — but
// fatal to a disposable test fixture: a test that flips a row to 's3' and
// then fails to clean it up (or a genuinely concurrent test run against
// the same shared database) otherwise poisons Reset for every test that
// runs afterward, cascading a single leaked row into a whole-package
// failure. Reset is about to drop the entire schema anyway, so deleting
// these rows first loses nothing — it just satisfies 00032's rollback
// guard ahead of the drop. Errors are discarded entirely: on a virgin
// database neither table exists yet, and a connection failure here is not
// this helper's job to report (the real migrate.Reset/migrate.Up calls
// right after it will surface a genuine connectivity problem loudly).
func preResetSweep(ctx context.Context, dsn string) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(ctx) }()
	// task_instance_photo before photo: no FK relationship between the two
	// requires this order, but it mirrors the production migrate-back
	// guidance's own table order (see the 00032 migration's error text).
	_, _ = conn.Exec(ctx, `DELETE FROM task_instance_photo WHERE storage_backend = 's3'`)
	_, _ = conn.Exec(ctx, `DELETE FROM photo WHERE storage_backend = 's3'`)
}

// newTestPool returns a pool against NESTOVA_TEST_DATABASE_URL with a freshly
// reset+migrated schema. It refuses to run unless the DSN's database name is
// "test" or ends with "_test" so migrate.Reset can never wipe a real database.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the media adapter tests")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	name := strings.ToLower(cfg.ConnConfig.Database)
	if name != "test" && !strings.HasSuffix(name, "_test") {
		t.Fatalf("refusing to reset database %q; name must be \"test\" or end with \"_test\"", name)
	}

	setupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	preResetSweep(setupCtx, dsn)
	if err := migrate.Reset(setupCtx, dsn); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := migrate.Up(setupCtx, dsn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelCleanup()
		preResetSweep(cleanupCtx, dsn)
		if err := migrate.Reset(cleanupCtx, dsn); err != nil {
			t.Logf("cleanup reset failed: %v", err)
		}
	})

	poolCtx, cancelPool := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelPool()
	pool, err := db.New(poolCtx, config.DBConfig{DSN: dsn, ConnTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func seedHousehold(t *testing.T, pool *pgxpool.Pool) household.HouseholdID {
	t.Helper()
	id := household.NewHouseholdID()
	if _, err := pool.Exec(testCtx(t), `INSERT INTO household (id, name) VALUES ($1, $2)`, id.String(), "The Fishers"); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	return id
}

func seedMember(t *testing.T, pool *pgxpool.Pool, hh household.HouseholdID, name string) household.MemberID {
	t.Helper()
	id := household.NewMemberID()
	if _, err := pool.Exec(testCtx(t),
		`INSERT INTO member (id, household_id, display_name, role, color_key) VALUES ($1, $2, $3, 'owner', 'sage')`,
		id.String(), hh.String(), name); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	return id
}

func newAlbum(t *testing.T, hh household.HouseholdID, name string, rotSeconds int, filter domain.AlbumFilter) *domain.Album {
	t.Helper()
	rot, err := domain.NewRotationInterval(rotSeconds)
	if err != nil {
		t.Fatalf("NewRotationInterval: %v", err)
	}
	return &domain.Album{ID: domain.NewAlbumID(), HouseholdID: hh, Name: name, Rotation: rot, Filter: filter}
}

func newPhoto(hh household.HouseholdID, ref string, uploader *household.MemberID) *domain.Photo {
	return &domain.Photo{
		ID: domain.NewPhotoID(), HouseholdID: hh,
		StorageRef: domain.StorageRef(ref), Caption: "", UploadedBy: uploader,
	}
}

// fakeHash returns a syntactically valid (64-character lowercase hex) content
// hash derived from seed, satisfying photo_content_sha256_format (00023)
// without needing real photo bytes — these tests exercise the repository,
// not PhotoStore.Put, so the hash's provenance is irrelevant, only its shape.
func fakeHash(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func TestAlbumRepositoryCRUD(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewAlbumRepository(pool)
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Alex")
	ctx := testCtx(t)

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	album := newAlbum(t, hh, "Family", 8, domain.AlbumFilter{MemberIDs: []household.MemberID{member}, Since: &since})
	if err := repo.Create(ctx, album); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if album.CreatedAt.IsZero() {
		t.Fatal("Create did not populate CreatedAt")
	}

	got, err := repo.Get(ctx, album.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "Family" || got.Rotation.Seconds() != 8 {
		t.Fatalf("Get album = %+v", got)
	}
	// Filter jsonb round-trips.
	if len(got.Filter.MemberIDs) != 1 || got.Filter.MemberIDs[0] != member || got.Filter.Since == nil || !got.Filter.Since.Equal(since) {
		t.Fatalf("filter did not round-trip: %+v", got.Filter)
	}

	album.Name = "Holidays"
	album.Rotation, _ = domain.NewRotationInterval(12)
	album.Filter = domain.AlbumFilter{}
	if err := repo.Update(ctx, album); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err = repo.Get(ctx, album.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.Name != "Holidays" || got.Rotation.Seconds() != 12 || len(got.Filter.MemberIDs) != 0 {
		t.Fatalf("Update not applied: %+v", got)
	}

	list, err := repo.ListByHousehold(ctx, hh)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByHousehold = %d albums (err %v), want 1", len(list), err)
	}

	if err := repo.Delete(ctx, album.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, album.ID); !errors.Is(err, domain.ErrAlbumNotFound) {
		t.Fatalf("Get after delete = %v, want ErrAlbumNotFound", err)
	}
	if err := repo.Delete(ctx, album.ID); !errors.Is(err, domain.ErrAlbumNotFound) {
		t.Fatalf("Delete unknown = %v, want ErrAlbumNotFound", err)
	}
}

func TestAlbumUpdateAndDeleteUnknown(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewAlbumRepository(pool)
	ctx := testCtx(t)
	// Update/Delete on an id that was never created report not-found.
	ghost := newAlbum(t, seedHousehold(t, pool), "Ghost", 5, domain.AlbumFilter{})
	if err := repo.Update(ctx, ghost); !errors.Is(err, domain.ErrAlbumNotFound) {
		t.Fatalf("Update unknown = %v, want ErrAlbumNotFound", err)
	}
	if err := repo.Delete(ctx, ghost.ID); !errors.Is(err, domain.ErrAlbumNotFound) {
		t.Fatalf("Delete unknown = %v, want ErrAlbumNotFound", err)
	}
}

func TestAlbumCreateUnknownHousehold(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewAlbumRepository(pool)
	album := newAlbum(t, household.NewHouseholdID(), "Orphan", 5, domain.AlbumFilter{})
	if err := repo.Create(testCtx(t), album); !errors.Is(err, household.ErrHouseholdNotFound) {
		t.Fatalf("Create with unknown household = %v, want ErrHouseholdNotFound", err)
	}
}

func TestPhotoRepositoryCRUD(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewPhotoRepository(pool, domain.StorageBackendLocal)
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Alex")
	ctx := testCtx(t)

	taken := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	photo := newPhoto(hh, "hh/aa/abc.jpg", &member)
	photo.TakenAt = &taken
	photo.Caption = "Beach"
	photo.ContentHash = fakeHash("abc123deadbeef")
	photo.SizeBytes = 4096
	photo.ContentType = "image/jpeg"
	if err := repo.Create(ctx, photo); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, photo.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.StorageRef != "hh/aa/abc.jpg" || got.Caption != "Beach" || got.TakenAt == nil || !got.TakenAt.Equal(taken) || got.UploadedBy == nil || *got.UploadedBy != member {
		t.Fatalf("Get photo = %+v", got)
	}
	if got.ContentHash != fakeHash("abc123deadbeef") || got.SizeBytes != 4096 || got.ContentType != "image/jpeg" {
		t.Fatalf("Get photo upload facts = %+v", got)
	}

	list, err := repo.ListByHousehold(ctx, hh)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByHousehold = %d (err %v), want 1", len(list), err)
	}

	if err := repo.Delete(ctx, photo.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, photo.ID); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("Get after delete = %v, want ErrPhotoNotFound", err)
	}
	if err := repo.Delete(ctx, photo.ID); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("Delete unknown = %v, want ErrPhotoNotFound", err)
	}
}

// TestPhotoRepositoryCreateStampsConfiguredBackend covers NES-132's CodeRabbit
// finding: Create must stamp storage_backend from the repository's OWN
// configured backend (never the column's DEFAULT), and a repository
// constructed for one backend must never contaminate rows with another's
// value. A repo built with StorageBackendLocal always writes 'local'; one
// built with StorageBackendS3 always writes 's3' — asserted both on the
// struct Create populates in place and on a fresh Get (proving the column
// itself, not just the in-memory value, is correct).
func TestPhotoRepositoryCreateStampsConfiguredBackend(t *testing.T) {
	pool := newTestPool(t)
	hh := seedHousehold(t, pool)
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
			repo := adapter.NewPhotoRepository(pool, tc.backend)
			photo := newPhoto(hh, "households/"+hh.String()+"/photos/aa/"+tc.name+".jpg", nil)
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
			// Clean up the 's3'-tagged row explicitly: the shared test
			// harness's cleanup rolls every migration back, including
			// 00032's down-migration (which re-narrows storage_backend's
			// CHECK to 'local' only) — a leftover 's3' row would make that
			// down-migration itself fail and silently corrupt the DB for
			// every test that runs after this one (newTestPool's Cleanup
			// only t.Logf's a failure, it does not Fatal).
			if err := repo.Delete(ctx, photo.ID); err != nil {
				t.Fatalf("cleanup Delete: %v", err)
			}
		})
	}
}

// TestNewPhotoRepositoryRejectsInvalidBackend covers the constructor's panic
// guard: an unknown StorageBackend must fail loudly at construction, not
// silently persist a bogus value the CHECK constraint would reject anyway.
func TestNewPhotoRepositoryRejectsInvalidBackend(t *testing.T) {
	pool := newTestPool(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewPhotoRepository with an invalid backend should have panicked")
		}
	}()
	adapter.NewPhotoRepository(pool, domain.StorageBackend("azure-blob"))
}

func TestPhotoCreateUnknownUploader(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewPhotoRepository(pool, domain.StorageBackendLocal)
	hh := seedHousehold(t, pool)
	stranger := household.NewMemberID()
	photo := newPhoto(hh, "hh/aa/x.jpg", &stranger)
	if err := repo.Create(testCtx(t), photo); !errors.Is(err, household.ErrMemberNotFound) {
		t.Fatalf("Create with unknown uploader = %v, want ErrMemberNotFound", err)
	}
}

// TestPhotoRepositoryListAllStorageRefs covers the storage reaper's source
// of truth (NES-132, ReaperService.referencedRefs): every photo's
// StorageRef, across every household (bucket-wide, not household-scoped —
// see the domain port doc), and an empty (not nil) slice when there are no
// photos at all.
func TestPhotoRepositoryListAllStorageRefs(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewPhotoRepository(pool, domain.StorageBackendLocal)
	ctx := testCtx(t)

	if refs, err := repo.ListAllStorageRefs(ctx, domain.StorageBackendLocal); err != nil || len(refs) != 0 {
		t.Fatalf("ListAllStorageRefs on an empty table = %v (err %v), want an empty slice", refs, err)
	}

	hhA := seedHousehold(t, pool)
	hhB := seedHousehold(t, pool)
	photoA := newPhoto(hhA, "households/"+hhA.String()+"/photos/aa/one.jpg", nil)
	photoA.ContentHash = fakeHash("refs-one")
	photoB := newPhoto(hhB, "households/"+hhB.String()+"/photos/bb/two.jpg", nil)
	photoB.ContentHash = fakeHash("refs-two")
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

// TestPhotoRepositoryListAllStorageRefsFiltersByBackend covers the
// NES-132 mixed-state reaper fix directly: content-addressed keys are
// IDENTICAL across backends for the same bytes, so two rows can
// legitimately share one storage_ref while being stamped with DIFFERENT
// backends. Without a backend filter, a local-backed row would shield a
// genuine S3 orphan of the same ref forever (or vice versa) — this proves
// ListAllStorageRefs and ExistsByStorageRef both filter on
// storage_backend, not just storage_ref.
func TestPhotoRepositoryListAllStorageRefsFiltersByBackend(t *testing.T) {
	pool := newTestPool(t)
	localRepo := adapter.NewPhotoRepository(pool, domain.StorageBackendLocal)
	s3Repo := adapter.NewPhotoRepository(pool, domain.StorageBackendS3)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	sharedRef := domain.StorageRef("households/" + hh.String() + "/photos/aa/shared.jpg")
	localPhoto := newPhoto(hh, sharedRef.String(), nil)
	localPhoto.ContentHash = fakeHash("shared-local")
	if err := localRepo.Create(ctx, localPhoto); err != nil {
		t.Fatalf("Create local-backed row: %v", err)
	}
	s3Photo := newPhoto(hh, sharedRef.String(), nil)
	s3Photo.ContentHash = fakeHash("shared-s3")
	if err := s3Repo.Create(ctx, s3Photo); err != nil {
		t.Fatalf("Create s3-backed row: %v", err)
	}
	// The down-migration for 00032 hard-aborts while any 's3' row lingers
	// (NES-132 review) — clean up explicitly so the shared test harness's
	// Reset (which rolls migrations all the way back) never trips it.
	t.Cleanup(func() { _ = s3Repo.Delete(ctx, s3Photo.ID) })

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

	// Deleting only the s3-backed row must leave the local-backed row (same
	// ref) fully intact and still reported for the local backend — proving
	// a reaper sweeping s3 that deletes this ref's OBJECT never implies
	// anything about the local row/object sharing that ref.
	if err := s3Repo.Delete(ctx, s3Photo.ID); err != nil {
		t.Fatalf("Delete s3-backed row: %v", err)
	}
	if exists, err := localRepo.ExistsByStorageRef(ctx, sharedRef, domain.StorageBackendS3); err != nil || exists {
		t.Fatalf("ExistsByStorageRef(ref, s3) after deleting the s3 row = %v, %v, want false, nil", exists, err)
	}
	if exists, err := localRepo.ExistsByStorageRef(ctx, sharedRef, domain.StorageBackendLocal); err != nil || !exists {
		t.Fatalf("ExistsByStorageRef(ref, local) after deleting the UNRELATED s3 row = %v, %v, want true, nil (still referenced)", exists, err)
	}
}

// TestPhotoRepositoryListByBackend covers NES-133's storage migrator's
// keyset-paginated batch source: rows stamped with the requested backend,
// ordered by id ascending, respecting afterID and limit, and excluding rows
// on the OTHER backend entirely.
func TestPhotoRepositoryListByBackend(t *testing.T) {
	pool := newTestPool(t)
	localRepo := adapter.NewPhotoRepository(pool, domain.StorageBackendLocal)
	s3Repo := adapter.NewPhotoRepository(pool, domain.StorageBackendS3)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	const n = 3
	var local []*domain.Photo
	for i := 0; i < n; i++ {
		p := newPhoto(hh, "households/"+hh.String()+"/photos/aa/list-by-backend.jpg", nil)
		p.ContentHash = fakeHash(fmt.Sprintf("list-by-backend-local-%d", i))
		if err := localRepo.Create(ctx, p); err != nil {
			t.Fatalf("Create local photo %d: %v", i, err)
		}
		local = append(local, p)
	}
	s3Photo := newPhoto(hh, "households/"+hh.String()+"/photos/bb/s3.jpg", nil)
	s3Photo.ContentHash = fakeHash("list-by-backend-s3")
	if err := s3Repo.Create(ctx, s3Photo); err != nil {
		t.Fatalf("Create s3 photo: %v", err)
	}
	t.Cleanup(func() { _ = s3Repo.Delete(ctx, s3Photo.ID) })

	// Page through with limit=1 to prove the cursor/limit contract, not just
	// "everything at once."
	var (
		afterID domain.PhotoID
		got     []*domain.Photo
	)
	for {
		page, err := localRepo.ListByBackend(ctx, domain.StorageBackendLocal, afterID, 1)
		if err != nil {
			t.Fatalf("ListByBackend: %v", err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) != 1 {
			t.Fatalf("ListByBackend page = %d rows, want at most limit=1", len(page))
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
	for _, p := range got {
		if p.StorageBackend != domain.StorageBackendLocal {
			t.Fatalf("ListByBackend(local) returned a %q-backed row", p.StorageBackend)
		}
	}

	s3Page, err := localRepo.ListByBackend(ctx, domain.StorageBackendS3, domain.PhotoID{}, 10)
	if err != nil {
		t.Fatalf("ListByBackend(s3): %v", err)
	}
	if len(s3Page) != 1 || s3Page[0].ID != s3Photo.ID {
		t.Fatalf("ListByBackend(s3) = %v, want exactly [%s]", s3Page, s3Photo.ID)
	}
	_ = local
}

// TestPhotoRepositoryMigrateStorageBackend covers NES-133's migrator's flip:
// a local-backend row is updated to newBackend/newRef, a legacy NULL
// content_sha256 is backfilled, an already-hashed row's hash is left
// unchanged, and re-running against an already-migrated (or unknown) id is
// a safe no-op reporting done=false — the migrator's resumability
// mechanism (AC1).
func TestPhotoRepositoryMigrateStorageBackend(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewPhotoRepository(pool, domain.StorageBackendLocal)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	t.Run("backfills a legacy NULL hash", func(t *testing.T) {
		legacy := newPhoto(hh, "households/"+hh.String()+"/photos/aa/legacy.jpg", nil)
		// ContentHash left blank: Create's nullableText stores this as NULL,
		// mirroring a pre-NES-123 row.
		if err := repo.Create(ctx, legacy); err != nil {
			t.Fatalf("Create legacy: %v", err)
		}
		t.Cleanup(func() { _ = repo.Delete(ctx, legacy.ID) })

		newRef := domain.StorageRef("households/" + hh.String() + "/photos/aa/legacy-migrated.jpg")
		computedHash := fakeHash("computed-during-migration")
		done, err := repo.MigrateStorageBackend(ctx, legacy.ID, newRef, domain.StorageBackendS3, computedHash)
		if err != nil {
			t.Fatalf("MigrateStorageBackend: %v", err)
		}
		if !done {
			t.Fatal("MigrateStorageBackend done = false, want true")
		}

		s3Repo := adapter.NewPhotoRepository(pool, domain.StorageBackendS3)
		got, err := s3Repo.Get(ctx, legacy.ID)
		if err != nil {
			t.Fatalf("Get after migrate: %v", err)
		}
		if got.StorageBackend != domain.StorageBackendS3 || got.StorageRef != newRef {
			t.Fatalf("Get after migrate = backend %q ref %q, want s3 / %q", got.StorageBackend, got.StorageRef, newRef)
		}
		if got.ContentHash != computedHash {
			t.Fatalf("Get after migrate ContentHash = %q, want backfilled %q", got.ContentHash, computedHash)
		}

		// Idempotent resume: re-running against the now-s3-backed row must
		// be a no-op, not a second write.
		doneAgain, err := repo.MigrateStorageBackend(ctx, legacy.ID, "households/should-not-apply.jpg", domain.StorageBackendS3, "ignored")
		if err != nil {
			t.Fatalf("second MigrateStorageBackend: %v", err)
		}
		if doneAgain {
			t.Fatal("second MigrateStorageBackend done = true, want false (row is no longer local-backend)")
		}
		gotAgain, err := s3Repo.Get(ctx, legacy.ID)
		if err != nil {
			t.Fatalf("Get after second migrate attempt: %v", err)
		}
		if gotAgain.StorageRef != newRef {
			t.Fatalf("StorageRef changed on the no-op second call: got %q, want unchanged %q", gotAgain.StorageRef, newRef)
		}
	})

	t.Run("does not overwrite an existing hash", func(t *testing.T) {
		hashed := newPhoto(hh, "households/"+hh.String()+"/photos/aa/hashed.jpg", nil)
		hashed.ContentHash = fakeHash("already-hashed")
		if err := repo.Create(ctx, hashed); err != nil {
			t.Fatalf("Create hashed: %v", err)
		}
		t.Cleanup(func() { _ = repo.Delete(ctx, hashed.ID) })

		newRef := domain.StorageRef("households/" + hh.String() + "/photos/aa/hashed-migrated.jpg")
		done, err := repo.MigrateStorageBackend(ctx, hashed.ID, newRef, domain.StorageBackendS3, fakeHash("computed-but-should-be-ignored"))
		if err != nil {
			t.Fatalf("MigrateStorageBackend: %v", err)
		}
		if !done {
			t.Fatal("MigrateStorageBackend done = false, want true")
		}
		s3Repo := adapter.NewPhotoRepository(pool, domain.StorageBackendS3)
		got, err := s3Repo.Get(ctx, hashed.ID)
		if err != nil {
			t.Fatalf("Get after migrate: %v", err)
		}
		if got.ContentHash != fakeHash("already-hashed") {
			t.Fatalf("ContentHash = %q, want the ORIGINAL hash preserved (COALESCE must not overwrite it)", got.ContentHash)
		}
	})

	t.Run("unknown id is a safe no-op", func(t *testing.T) {
		ghostID := domain.NewPhotoID()
		done, err := repo.MigrateStorageBackend(ctx, ghostID, "households/ghost.jpg", domain.StorageBackendS3, "")
		if err != nil {
			t.Fatalf("MigrateStorageBackend on unknown id: %v", err)
		}
		if done {
			t.Fatal("MigrateStorageBackend on unknown id: done = true, want false")
		}
	})
}

// TestPhotoFindByContentHash covers the repository half of AC3 (content-hash
// dedup): a matching hash within the household is found, an unknown hash and
// a blank hash both report ErrPhotoNotFound, and a photo with no hash at all
// (the pre-NES-123/legacy state) never matches.
func TestPhotoFindByContentHash(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewPhotoRepository(pool, domain.StorageBackendLocal)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	photo := newPhoto(hh, "hh/aa/abc.jpg", nil)
	photo.ContentHash = fakeHash("deadbeefdeadbeef")
	if err := repo.Create(ctx, photo); err != nil {
		t.Fatalf("Create: %v", err)
	}
	legacy := newPhoto(hh, "hh/bb/legacy.jpg", nil) // no ContentHash, like a pre-NES-123 row
	if err := repo.Create(ctx, legacy); err != nil {
		t.Fatalf("Create legacy: %v", err)
	}

	got, err := repo.FindByContentHash(ctx, hh, fakeHash("deadbeefdeadbeef"))
	if err != nil {
		t.Fatalf("FindByContentHash: %v", err)
	}
	if got.ID != photo.ID {
		t.Fatalf("FindByContentHash returned %s, want %s", got.ID, photo.ID)
	}

	if _, err := repo.FindByContentHash(ctx, hh, "not-a-real-hash"); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("FindByContentHash(unknown hash) = %v, want ErrPhotoNotFound", err)
	}
	if _, err := repo.FindByContentHash(ctx, hh, ""); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("FindByContentHash(blank hash) = %v, want ErrPhotoNotFound (must not match the legacy row)", err)
	}
}

// TestPhotoCreateDuplicateContentHashRejected covers the database-level guard
// behind AC3's race path: two rows in the same household cannot share a
// content hash, but the same hash is fine across different households.
func TestPhotoCreateDuplicateContentHashRejected(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewPhotoRepository(pool, domain.StorageBackendLocal)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	first := newPhoto(hh, "hh/aa/one.jpg", nil)
	first.ContentHash = fakeHash("samehash")
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("Create first: %v", err)
	}

	second := newPhoto(hh, "hh/aa/two.jpg", nil)
	second.ContentHash = fakeHash("samehash")
	if err := repo.Create(ctx, second); !errors.Is(err, domain.ErrDuplicatePhoto) {
		t.Fatalf("Create with a colliding content hash = %v, want ErrDuplicatePhoto", err)
	}

	// The same hash in a different household is not a conflict — dedup is
	// scoped per household.
	otherHH := seedHousehold(t, pool)
	third := newPhoto(otherHH, "hh/aa/three.jpg", nil)
	third.ContentHash = fakeHash("samehash")
	if err := repo.Create(ctx, third); err != nil {
		t.Fatalf("Create with the same hash in a different household: %v", err)
	}
}

func TestAlbumPhotoOrderingAndCascade(t *testing.T) {
	pool := newTestPool(t)
	albums := adapter.NewAlbumRepository(pool)
	photos := adapter.NewPhotoRepository(pool, domain.StorageBackendLocal)
	members := adapter.NewAlbumPhotoRepository(pool)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	album := newAlbum(t, hh, "Slideshow", 6, domain.AlbumFilter{})
	if err := albums.Create(ctx, album); err != nil {
		t.Fatalf("Create album: %v", err)
	}
	var ids []domain.PhotoID
	for i, ref := range []string{"hh/a/1.jpg", "hh/b/2.jpg", "hh/c/3.jpg"} {
		p := newPhoto(hh, ref, nil)
		p.Caption = string(rune('A' + i))
		if err := photos.Create(ctx, p); err != nil {
			t.Fatalf("Create photo: %v", err)
		}
		ids = append(ids, p.ID)
		if err := members.Add(ctx, album.ID, p.ID); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	// Adding a duplicate is a no-op.
	if err := members.Add(ctx, album.ID, ids[0]); err != nil {
		t.Fatalf("duplicate Add: %v", err)
	}

	ordered, err := members.ListByAlbumOrdered(ctx, album.ID)
	if err != nil || len(ordered) != 3 {
		t.Fatalf("ListByAlbumOrdered = %d (err %v), want 3", len(ordered), err)
	}
	if ordered[0].ID != ids[0] || ordered[2].ID != ids[2] {
		t.Fatal("initial order wrong")
	}

	// Reverse and confirm the new order (validates end-of-statement uniqueness).
	if err := members.Reorder(ctx, album.ID, []domain.PhotoID{ids[2], ids[1], ids[0]}); err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	ordered, err = members.ListByAlbumOrdered(ctx, album.ID)
	if err != nil {
		t.Fatalf("ListByAlbumOrdered after reorder: %v", err)
	}
	if ordered[0].ID != ids[2] || ordered[1].ID != ids[1] || ordered[2].ID != ids[0] {
		t.Fatalf("order after reorder = [%s %s %s]", ordered[0].ID, ordered[1].ID, ordered[2].ID)
	}

	// Remove one; it leaves the album. A second Remove of the same photo is a no-op.
	if err := members.Remove(ctx, album.ID, ids[1]); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := members.Remove(ctx, album.ID, ids[1]); err != nil {
		t.Fatalf("second Remove must be a no-op, got: %v", err)
	}
	ordered, err = members.ListByAlbumOrdered(ctx, album.ID)
	if err != nil {
		t.Fatalf("ListByAlbumOrdered after remove: %v", err)
	}
	if len(ordered) != 2 {
		t.Fatalf("after Remove = %d photos, want 2", len(ordered))
	}

	// Deleting a photo cascades its membership.
	if err := photos.Delete(ctx, ids[2]); err != nil {
		t.Fatalf("Delete photo: %v", err)
	}
	ordered, err = members.ListByAlbumOrdered(ctx, album.ID)
	if err != nil {
		t.Fatalf("ListByAlbumOrdered after delete: %v", err)
	}
	if len(ordered) != 1 || ordered[0].ID != ids[0] {
		t.Fatalf("after photo delete = %v, want only the first photo", ordered)
	}
}

func TestAddToUnknownAlbumReportsNotFound(t *testing.T) {
	pool := newTestPool(t)
	members := adapter.NewAlbumPhotoRepository(pool)
	if err := members.Add(testCtx(t), domain.NewAlbumID(), domain.NewPhotoID()); !errors.Is(err, domain.ErrAlbumNotFound) {
		t.Fatalf("Add to unknown album = %v, want ErrAlbumNotFound", err)
	}
}

func TestReorderRejectsIncompleteOrder(t *testing.T) {
	pool := newTestPool(t)
	albums := adapter.NewAlbumRepository(pool)
	photos := adapter.NewPhotoRepository(pool, domain.StorageBackendLocal)
	members := adapter.NewAlbumPhotoRepository(pool)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	album := newAlbum(t, hh, "Slideshow", 6, domain.AlbumFilter{})
	if err := albums.Create(ctx, album); err != nil {
		t.Fatalf("Create album: %v", err)
	}
	var ids []domain.PhotoID
	for _, ref := range []string{"hh/a/1.jpg", "hh/b/2.jpg", "hh/c/3.jpg"} {
		p := newPhoto(hh, ref, nil)
		if err := photos.Create(ctx, p); err != nil {
			t.Fatalf("Create photo: %v", err)
		}
		ids = append(ids, p.ID)
		if err := members.Add(ctx, album.ID, p.ID); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	// An order missing a current member is rejected and rolls back (order unchanged).
	if err := members.Reorder(ctx, album.ID, []domain.PhotoID{ids[2], ids[0]}); err == nil {
		t.Fatal("incomplete Reorder should fail")
	}
	ordered, err := members.ListByAlbumOrdered(ctx, album.ID)
	if err != nil {
		t.Fatalf("ListByAlbumOrdered: %v", err)
	}
	if len(ordered) != 3 || ordered[0].ID != ids[0] || ordered[1].ID != ids[1] || ordered[2].ID != ids[2] {
		t.Fatal("failed Reorder must leave the original order intact")
	}
}

func TestAddPhotoFromAnotherHouseholdRejected(t *testing.T) {
	pool := newTestPool(t)
	albums := adapter.NewAlbumRepository(pool)
	photos := adapter.NewPhotoRepository(pool, domain.StorageBackendLocal)
	members := adapter.NewAlbumPhotoRepository(pool)
	ctx := testCtx(t)

	hhA := seedHousehold(t, pool)
	hhB := seedHousehold(t, pool)
	album := newAlbum(t, hhA, "A's album", 5, domain.AlbumFilter{})
	if err := albums.Create(ctx, album); err != nil {
		t.Fatalf("Create album: %v", err)
	}
	foreign := newPhoto(hhB, "hhB/aa/x.jpg", nil)
	if err := photos.Create(ctx, foreign); err != nil {
		t.Fatalf("Create foreign photo: %v", err)
	}
	// The composite tenant FK makes a cross-household link impossible.
	if err := members.Add(ctx, album.ID, foreign.ID); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("cross-household Add = %v, want ErrPhotoNotFound", err)
	}
}
