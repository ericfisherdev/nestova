package app_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/app"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// --- fakes ---

type fakePhotoStore struct {
	putErr  error
	puts    int
	deleted []domain.StorageRef
}

func (f *fakePhotoStore) Put(_ context.Context, _ household.HouseholdID, _ []byte, _ string) (domain.StorageRef, error) {
	if f.putErr != nil {
		return "", f.putErr
	}
	f.puts++
	return domain.StorageRef(fmt.Sprintf("hh/aa/ref%d.jpg", f.puts)), nil
}

func (f *fakePhotoStore) Open(context.Context, domain.StorageRef) (io.ReadCloser, error) {
	return nil, nil
}

func (f *fakePhotoStore) Delete(_ context.Context, ref domain.StorageRef) error {
	f.deleted = append(f.deleted, ref)
	return nil
}

type fakeExif struct{ taken *time.Time }

func (f fakeExif) TakenAt([]byte) *time.Time { return f.taken }

type fakePhotoRepo struct {
	store     map[domain.PhotoID]*domain.Photo
	createErr error
	created   []*domain.Photo
	deleted   []domain.PhotoID
}

func newFakePhotoRepo() *fakePhotoRepo {
	return &fakePhotoRepo{store: map[domain.PhotoID]*domain.Photo{}}
}

func (f *fakePhotoRepo) Create(_ context.Context, p *domain.Photo) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.store[p.ID] = p
	f.created = append(f.created, p)
	return nil
}

func (f *fakePhotoRepo) Get(_ context.Context, id domain.PhotoID) (*domain.Photo, error) {
	if p, ok := f.store[id]; ok {
		return p, nil
	}
	return nil, domain.ErrPhotoNotFound
}

func (f *fakePhotoRepo) ListByHousehold(context.Context, household.HouseholdID) ([]*domain.Photo, error) {
	return nil, nil
}

func (f *fakePhotoRepo) Delete(_ context.Context, id domain.PhotoID) error {
	if _, ok := f.store[id]; !ok {
		return domain.ErrPhotoNotFound
	}
	delete(f.store, id)
	f.deleted = append(f.deleted, id)
	return nil
}

type fakeAlbumRepo struct {
	store   map[domain.AlbumID]*domain.Album
	updated []*domain.Album
}

func newFakeAlbumRepo() *fakeAlbumRepo {
	return &fakeAlbumRepo{store: map[domain.AlbumID]*domain.Album{}}
}

func (f *fakeAlbumRepo) Create(_ context.Context, a *domain.Album) error {
	f.store[a.ID] = a
	return nil
}

func (f *fakeAlbumRepo) Get(_ context.Context, id domain.AlbumID) (*domain.Album, error) {
	if a, ok := f.store[id]; ok {
		return a, nil
	}
	return nil, domain.ErrAlbumNotFound
}

func (f *fakeAlbumRepo) Update(_ context.Context, a *domain.Album) error {
	f.store[a.ID] = a
	f.updated = append(f.updated, a)
	return nil
}

func (f *fakeAlbumRepo) ListByHousehold(context.Context, household.HouseholdID) ([]*domain.Album, error) {
	return nil, nil
}
func (f *fakeAlbumRepo) Delete(context.Context, domain.AlbumID) error { return nil }

type fakeAlbumPhotoRepo struct {
	added   []domain.PhotoID
	ordered []*domain.Photo
}

func (f *fakeAlbumPhotoRepo) Add(_ context.Context, _ domain.AlbumID, p domain.PhotoID) error {
	f.added = append(f.added, p)
	return nil
}

func (f *fakeAlbumPhotoRepo) Remove(context.Context, domain.AlbumID, domain.PhotoID) error {
	return nil
}

func (f *fakeAlbumPhotoRepo) Reorder(context.Context, domain.AlbumID, []domain.PhotoID) error {
	return nil
}

func (f *fakeAlbumPhotoRepo) ListByAlbumOrdered(context.Context, domain.AlbumID) ([]*domain.Photo, error) {
	return f.ordered, nil
}

// --- helpers ---

func rotation(t *testing.T, secs int) domain.RotationInterval {
	t.Helper()
	r, err := domain.NewRotationInterval(secs)
	if err != nil {
		t.Fatalf("NewRotationInterval: %v", err)
	}
	return r
}

// --- PhotoService ---

func TestPhotoServiceUpload(t *testing.T) {
	store := &fakePhotoStore{}
	taken := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	repo := newFakePhotoRepo()
	svc, err := app.NewPhotoService(store, fakeExif{taken: &taken}, repo)
	if err != nil {
		t.Fatalf("NewPhotoService: %v", err)
	}
	hh := household.NewHouseholdID()
	uploader := household.NewMemberID()

	photo, err := svc.Upload(context.Background(), hh, uploader, []byte("imgbytes"), "image/jpeg", "  Beach  ")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if photo.StorageRef != "hh/aa/ref1.jpg" || photo.Caption != "Beach" || photo.TakenAt == nil || !photo.TakenAt.Equal(taken) {
		t.Fatalf("uploaded photo = %+v", photo)
	}
	if photo.UploadedBy == nil || *photo.UploadedBy != uploader || photo.HouseholdID != hh {
		t.Fatalf("attribution wrong: %+v", photo)
	}
	if len(repo.created) != 1 {
		t.Fatalf("created %d photos, want 1", len(repo.created))
	}
}

func TestPhotoServiceUploadStoreErrorPropagates(t *testing.T) {
	store := &fakePhotoStore{putErr: domain.ErrUnsupportedMediaType}
	repo := newFakePhotoRepo()
	svc, _ := app.NewPhotoService(store, fakeExif{}, repo)
	if _, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), []byte("x"), "application/pdf", ""); !errors.Is(err, domain.ErrUnsupportedMediaType) {
		t.Fatalf("Upload error = %v, want ErrUnsupportedMediaType", err)
	}
	if len(repo.created) != 0 {
		t.Fatal("store error must not persist a photo")
	}
}

func TestPhotoServiceUploadCleansUpOnCreateError(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakePhotoRepo()
	repo.createErr = errors.New("db down")
	svc, _ := app.NewPhotoService(store, fakeExif{}, repo)
	if _, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), []byte("x"), "image/png", ""); err == nil {
		t.Fatal("Upload should fail when Create fails")
	}
	if len(store.deleted) != 1 || store.deleted[0] != "hh/aa/ref1.jpg" {
		t.Fatalf("stored bytes not cleaned up: deleted=%v", store.deleted)
	}
}

func TestPhotoServiceDeleteRejectsOtherHousehold(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakePhotoRepo()
	other := household.NewHouseholdID()
	id := domain.NewPhotoID()
	repo.store[id] = &domain.Photo{ID: id, HouseholdID: other, StorageRef: "x/y/z.jpg"}
	svc, _ := app.NewPhotoService(store, fakeExif{}, repo)

	if err := svc.Delete(context.Background(), household.NewHouseholdID(), id); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("cross-household Delete = %v, want ErrPhotoNotFound", err)
	}
	if len(repo.deleted) != 0 || len(store.deleted) != 0 {
		t.Fatal("cross-household Delete must not remove anything")
	}
}

// --- AlbumService ---

func TestAlbumServiceCreateValidates(t *testing.T) {
	svc, _ := app.NewAlbumService(newFakeAlbumRepo(), newFakePhotoRepo(), &fakeAlbumPhotoRepo{})
	if _, err := svc.Create(context.Background(), household.NewHouseholdID(), app.AlbumInput{Name: "  ", Rotation: rotation(t, 5)}); !errors.Is(err, domain.ErrInvalidAlbum) {
		t.Fatalf("Create blank name = %v, want ErrInvalidAlbum", err)
	}
}

func TestAlbumServiceConfigureValidatesAndChecksOwnership(t *testing.T) {
	albums := newFakeAlbumRepo()
	hh := household.NewHouseholdID()
	album := &domain.Album{ID: domain.NewAlbumID(), HouseholdID: hh, Name: "A", Rotation: rotation(t, 5)}
	albums.store[album.ID] = album
	svc, _ := app.NewAlbumService(albums, newFakePhotoRepo(), &fakeAlbumPhotoRepo{})

	// A blank name is rejected.
	if err := svc.Configure(context.Background(), hh, album.ID, app.AlbumInput{Name: " ", Rotation: rotation(t, 5)}); !errors.Is(err, domain.ErrInvalidAlbum) {
		t.Fatalf("Configure blank name = %v, want ErrInvalidAlbum", err)
	}
	// Configuring another household's album reports not-found and does not update.
	if err := svc.Configure(context.Background(), household.NewHouseholdID(), album.ID, app.AlbumInput{Name: "X", Rotation: rotation(t, 5)}); !errors.Is(err, domain.ErrAlbumNotFound) {
		t.Fatalf("cross-household Configure = %v, want ErrAlbumNotFound", err)
	}
	if len(albums.updated) != 0 {
		t.Fatal("invalid/cross-household Configure must not update")
	}
}

func TestAlbumServiceAddPhotoRejectsCrossHousehold(t *testing.T) {
	albums := newFakeAlbumRepo()
	photos := newFakePhotoRepo()
	hh := household.NewHouseholdID()
	album := &domain.Album{ID: domain.NewAlbumID(), HouseholdID: hh, Name: "A", Rotation: rotation(t, 5)}
	albums.store[album.ID] = album
	foreign := &domain.Photo{ID: domain.NewPhotoID(), HouseholdID: household.NewHouseholdID(), StorageRef: "x.jpg"}
	photos.store[foreign.ID] = foreign

	apr := &fakeAlbumPhotoRepo{}
	svc, _ := app.NewAlbumService(albums, photos, apr)
	if err := svc.AddPhoto(context.Background(), hh, album.ID, foreign.ID); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("cross-household AddPhoto = %v, want ErrPhotoNotFound", err)
	}
	if len(apr.added) != 0 {
		t.Fatal("cross-household AddPhoto must not add")
	}
}

func TestAlbumServicePlaylistAppliesFilterAndOrder(t *testing.T) {
	albums := newFakeAlbumRepo()
	photos := newFakePhotoRepo()
	apr := &fakeAlbumPhotoRepo{}
	hh := household.NewHouseholdID()
	alex := household.NewMemberID()
	sam := household.NewMemberID()

	// Album filtered to Alex's photos.
	album := &domain.Album{
		ID: domain.NewAlbumID(), HouseholdID: hh, Name: "Alex", Rotation: rotation(t, 5),
		Filter: domain.AlbumFilter{MemberIDs: []household.MemberID{alex}},
	}
	albums.store[album.ID] = album

	p1 := &domain.Photo{ID: domain.NewPhotoID(), HouseholdID: hh, StorageRef: "1.jpg", UploadedBy: &alex, Caption: "one"}
	p2 := &domain.Photo{ID: domain.NewPhotoID(), HouseholdID: hh, StorageRef: "2.jpg", UploadedBy: &sam, Caption: "two"}
	p3 := &domain.Photo{ID: domain.NewPhotoID(), HouseholdID: hh, StorageRef: "3.jpg", UploadedBy: &alex, Caption: "three"}
	apr.ordered = []*domain.Photo{p1, p2, p3} // position order

	svc, _ := app.NewAlbumService(albums, photos, apr)
	items, err := svc.Playlist(context.Background(), hh, album.ID)
	if err != nil {
		t.Fatalf("Playlist: %v", err)
	}
	// Only Alex's photos, in their original order.
	if len(items) != 2 || items[0].PhotoID != p1.ID || items[1].PhotoID != p3.ID {
		t.Fatalf("playlist = %d items %+v, want [p1 p3]", len(items), items)
	}
}

func TestAlbumServicePlaylistRejectsOtherHousehold(t *testing.T) {
	albums := newFakeAlbumRepo()
	album := &domain.Album{ID: domain.NewAlbumID(), HouseholdID: household.NewHouseholdID(), Name: "A", Rotation: rotation(t, 5)}
	albums.store[album.ID] = album
	svc, _ := app.NewAlbumService(albums, newFakePhotoRepo(), &fakeAlbumPhotoRepo{})
	if _, err := svc.Playlist(context.Background(), household.NewHouseholdID(), album.ID); !errors.Is(err, domain.ErrAlbumNotFound) {
		t.Fatalf("cross-household Playlist = %v, want ErrAlbumNotFound", err)
	}
}
