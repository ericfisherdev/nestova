package adapter_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/adapter"
	"github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
)

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
	if err := migrate.Reset(setupCtx, dsn); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := migrate.Up(setupCtx, dsn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelCleanup()
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
	got, _ = repo.Get(ctx, album.ID)
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
	repo := adapter.NewPhotoRepository(pool)
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Alex")
	ctx := testCtx(t)

	taken := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	photo := newPhoto(hh, "hh/aa/abc.jpg", &member)
	photo.TakenAt = &taken
	photo.Caption = "Beach"
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

func TestPhotoCreateUnknownUploader(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewPhotoRepository(pool)
	hh := seedHousehold(t, pool)
	stranger := household.NewMemberID()
	photo := newPhoto(hh, "hh/aa/x.jpg", &stranger)
	if err := repo.Create(testCtx(t), photo); !errors.Is(err, household.ErrMemberNotFound) {
		t.Fatalf("Create with unknown uploader = %v, want ErrMemberNotFound", err)
	}
}

func TestAlbumPhotoOrderingAndCascade(t *testing.T) {
	pool := newTestPool(t)
	albums := adapter.NewAlbumRepository(pool)
	photos := adapter.NewPhotoRepository(pool)
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
	ordered, _ = members.ListByAlbumOrdered(ctx, album.ID)
	if ordered[0].ID != ids[2] || ordered[1].ID != ids[1] || ordered[2].ID != ids[0] {
		t.Fatalf("order after reorder = [%s %s %s]", ordered[0].ID, ordered[1].ID, ordered[2].ID)
	}

	// Remove one; it leaves the album.
	if err := members.Remove(ctx, album.ID, ids[1]); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	ordered, _ = members.ListByAlbumOrdered(ctx, album.ID)
	if len(ordered) != 2 {
		t.Fatalf("after Remove = %d photos, want 2", len(ordered))
	}

	// Deleting a photo cascades its membership.
	if err := photos.Delete(ctx, ids[2]); err != nil {
		t.Fatalf("Delete photo: %v", err)
	}
	ordered, _ = members.ListByAlbumOrdered(ctx, album.ID)
	if len(ordered) != 1 || ordered[0].ID != ids[0] {
		t.Fatalf("after photo delete = %v, want only the first photo", ordered)
	}
}

func TestAddPhotoFromAnotherHouseholdRejected(t *testing.T) {
	pool := newTestPool(t)
	albums := adapter.NewAlbumRepository(pool)
	photos := adapter.NewPhotoRepository(pool)
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
