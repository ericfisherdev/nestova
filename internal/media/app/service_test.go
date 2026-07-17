package app_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	putErr    error
	openErr   error
	urlErr    error
	puts      int
	openCalls int
	// urlCalls counts URL invocations — asserted at 0 by ownership-rejection
	// tests (RawServe must reject a cross-household id BEFORE ever
	// consulting the store, on either the Open or the URL branch).
	urlCalls     int
	deleted      []domain.StorageRef
	lastPutClass domain.PhotoClass
	// lastPutBytes records the exact bytes the most recent Put call read, so
	// a test can assert what a caller actually sent to storage (e.g.
	// ChoreProofPhotoService.Upload must send EXIF-scrubbed bytes, never the
	// raw upload — see chore_photo_service_test.go).
	lastPutBytes []byte
	// directURL backs SupportsDirectURL — false (LocalPhotoStore-like)
	// unless a test opts into the S3-like redirect path.
	directURL bool
}

// Put hashes the bytes it's given and derives Ref from the hash — like the
// real content-addressed LocalPhotoStore — so identical content always
// produces the identical ref, letting a test detect an unsafe delete of a ref
// a still-valid photo row shares (rather than an incrementing counter, which
// would give every Put a distinct ref and hide that class of bug). class is
// recorded (lastPutClass) so a test can assert PhotoService always uploads
// under domain.PhotoClassAlbum, but otherwise does not affect the fake's
// bytes-in/ref-out behavior — the fake is not itself testing class
// namespacing, which is LocalPhotoStore's concern (see photo_store_test.go).
func (f *fakePhotoStore) Put(_ context.Context, _ household.HouseholdID, class domain.PhotoClass, r io.Reader) (domain.PutResult, error) {
	f.lastPutClass = class
	if f.putErr != nil {
		return domain.PutResult{}, f.putErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return domain.PutResult{}, err
	}
	f.puts++
	f.lastPutBytes = data
	hash := sha256Hex(string(data))
	return domain.PutResult{
		Ref:         refFor(hash),
		ContentHash: hash,
		SizeBytes:   int64(len(data)),
		ContentType: "image/jpeg",
	}, nil
}

// refFor mirrors LocalPhotoStore's content-addressed layout
// (<household>/<aa>/<hash>.<ext>), collapsed to a fixed household segment
// since these tests don't exercise cross-household path separation.
func refFor(hash string) domain.StorageRef {
	return domain.StorageRef(fmt.Sprintf("hh/%s/%s.jpg", hash[:2], hash))
}

func (f *fakePhotoStore) Open(context.Context, domain.StorageRef) (domain.PhotoReader, error) {
	f.openCalls++
	if f.openErr != nil {
		return nil, f.openErr
	}
	return fakePhotoReader{bytes.NewReader(nil)}, nil
}

func (f *fakePhotoStore) Delete(_ context.Context, ref domain.StorageRef) error {
	f.deleted = append(f.deleted, ref)
	return nil
}

// URL mirrors LocalPhotoStore's contract closely enough for a unit test: ref
// itself, back as a stable locator, since nothing under test exercises a
// real URL/ttl semantic.
func (f *fakePhotoStore) URL(_ context.Context, ref domain.StorageRef, _ time.Duration) (string, error) {
	f.urlCalls++
	if f.urlErr != nil {
		return "", f.urlErr
	}
	return ref.String(), nil
}

// SupportsDirectURL defaults to false (mirroring LocalPhotoStore) so
// existing tests that never set directURL keep exercising the
// Open-and-stream path; RawServe tests flip it to exercise the redirect path.
func (f *fakePhotoStore) SupportsDirectURL() bool { return f.directURL }

// fakePhotoReader adapts a *bytes.Reader (already Read+ReadAt+Seek) into a
// domain.PhotoReader with a no-op Close.
type fakePhotoReader struct{ *bytes.Reader }

func (fakePhotoReader) Close() error { return nil }

// sha256Hex mirrors what fakePhotoStore.Put (and the real LocalPhotoStore)
// computes for content s, so a test can seed a photo with the exact hash a
// later Upload of the same bytes will produce.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type fakeExif struct{ taken *time.Time }

func (f fakeExif) TakenAt(domain.RandomAccessReader) *time.Time { return f.taken }

type fakePhotoRepo struct {
	store     map[domain.PhotoID]*domain.Photo
	createErr error
	created   []*domain.Photo
	deleted   []domain.PhotoID

	// raceHash/raceWinner/raceFindCalls simulate a concurrent upload winning
	// the unique-hash race between PhotoService's pre-Create dedup check and
	// its retry after Create fails with ErrDuplicatePhoto: FindByContentHash
	// reports ErrPhotoNotFound the first time it's asked about raceHash (the
	// winner hasn't committed yet), then returns raceWinner on every
	// subsequent call (the winner has since committed).
	raceHash      string
	raceWinner    *domain.Photo
	raceFindCalls int

	// existsOverride lets a test make ExistsByStorageRef answer DIFFERENTLY
	// from what a ListAllStorageRefs snapshot (taken earlier in the same
	// Run) would show — simulating a row that commits in the gap between
	// the reaper's bulk snapshot and its per-object recheck (e.g. a
	// restore), which the recheck must catch even though the snapshot
	// could not have. Unset refs fall back to a live f.store lookup.
	existsOverride map[domain.StorageRef]bool
	existsCalls    []domain.StorageRef
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

func (f *fakePhotoRepo) FindByContentHash(_ context.Context, householdID household.HouseholdID, hash string) (*domain.Photo, error) {
	if hash == "" {
		return nil, domain.ErrPhotoNotFound
	}
	if f.raceHash != "" && hash == f.raceHash {
		f.raceFindCalls++
		if f.raceFindCalls == 1 {
			return nil, domain.ErrPhotoNotFound
		}
		return f.raceWinner, nil
	}
	for _, p := range f.store {
		if p.HouseholdID == householdID && p.ContentHash == hash {
			return p, nil
		}
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

func (f *fakePhotoRepo) ListAllStorageRefs(context.Context) ([]domain.StorageRef, error) {
	refs := make([]domain.StorageRef, 0, len(f.store))
	for _, p := range f.store {
		refs = append(refs, p.StorageRef)
	}
	return refs, nil
}

// ExistsByStorageRef checks existsOverride first (see its doc), otherwise
// falls back to a live lookup against f.store.
func (f *fakePhotoRepo) ExistsByStorageRef(_ context.Context, ref domain.StorageRef) (bool, error) {
	f.existsCalls = append(f.existsCalls, ref)
	if v, ok := f.existsOverride[ref]; ok {
		return v, nil
	}
	for _, p := range f.store {
		if p.StorageRef == ref {
			return true, nil
		}
	}
	return false, nil
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

	result, err := svc.Upload(context.Background(), hh, uploader, bytes.NewReader([]byte("imgbytes")), "  Beach  ")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if result.Duplicate {
		t.Fatal("first upload of new content must not be a duplicate")
	}
	photo := result.Photo
	if photo.StorageRef != refFor(sha256Hex("imgbytes")) || photo.Caption != "Beach" || photo.TakenAt == nil || !photo.TakenAt.Equal(taken) {
		t.Fatalf("uploaded photo = %+v", photo)
	}
	if photo.ContentHash == "" {
		t.Fatal("uploaded photo must carry the content hash PhotoStore.Put computed")
	}
	if photo.UploadedBy == nil || *photo.UploadedBy != uploader || photo.HouseholdID != hh {
		t.Fatalf("attribution wrong: %+v", photo)
	}
	if len(repo.created) != 1 {
		t.Fatalf("created %d photos, want 1", len(repo.created))
	}
	if store.lastPutClass != domain.PhotoClassAlbum {
		t.Fatalf("Upload called Put with class %v, want PhotoClassAlbum", store.lastPutClass)
	}
}

// TestPhotoServiceUploadDeduplicatesByContentHash covers AC3: uploading the
// same bytes twice for a household creates exactly one photo row, and the
// second Upload reports Duplicate instead of erroring.
func TestPhotoServiceUploadDeduplicatesByContentHash(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakePhotoRepo()
	svc, _ := app.NewPhotoService(store, fakeExif{}, repo)
	hh := household.NewHouseholdID()
	uploader := household.NewMemberID()

	first, err := svc.Upload(context.Background(), hh, uploader, bytes.NewReader([]byte("same-bytes")), "")
	if err != nil {
		t.Fatalf("first Upload: %v", err)
	}
	if first.Duplicate {
		t.Fatal("first upload must not be reported as a duplicate")
	}

	second, err := svc.Upload(context.Background(), hh, uploader, bytes.NewReader([]byte("same-bytes")), "")
	if err != nil {
		t.Fatalf("second Upload: %v", err)
	}
	if !second.Duplicate {
		t.Fatal("re-uploading identical bytes must be reported as a duplicate")
	}
	if second.Photo.ID != first.Photo.ID {
		t.Fatalf("duplicate upload returned a different photo: got %s, want %s", second.Photo.ID, first.Photo.ID)
	}
	if len(repo.created) != 1 {
		t.Fatalf("created %d photo rows, want 1 (dedup must not create a second row)", len(repo.created))
	}
}

// TestPhotoServiceUploadResolvesConcurrentDuplicate covers the race where two
// uploads of the same bytes both pass the pre-check and only one wins the
// unique-index insert. The fake repo's raceHash/raceWinner make
// FindByContentHash miss (ErrPhotoNotFound) on Upload's first, pre-Create
// check — exactly as it would for a genuinely new upload — and only return
// the winner on the second call Upload makes after Create reports
// ErrDuplicatePhoto, so this exercises the actual race-resolution branch
// rather than short-circuiting at the pre-check.
func TestPhotoServiceUploadResolvesConcurrentDuplicate(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakePhotoRepo()
	hh := household.NewHouseholdID()
	hash := sha256Hex("raced-bytes")
	winner := &domain.Photo{
		ID: domain.NewPhotoID(), HouseholdID: hh,
		StorageRef: refFor(hash), ContentHash: hash,
	}
	repo.raceHash = hash
	repo.raceWinner = winner
	repo.createErr = domain.ErrDuplicatePhoto
	svc, _ := app.NewPhotoService(store, fakeExif{}, repo)

	result, err := svc.Upload(context.Background(), hh, household.NewMemberID(), bytes.NewReader([]byte("raced-bytes")), "")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !result.Duplicate || result.Photo.ID != winner.ID {
		t.Fatalf("Upload = %+v, want the pre-existing winner reported as a duplicate", result)
	}
	if repo.raceFindCalls != 2 {
		t.Fatalf("FindByContentHash called %d times, want 2 (pre-check miss, then a hit after Create's ErrDuplicatePhoto)", repo.raceFindCalls)
	}
}

func TestPhotoServiceUploadStoreErrorPropagates(t *testing.T) {
	store := &fakePhotoStore{putErr: domain.ErrUnsupportedMediaType}
	repo := newFakePhotoRepo()
	svc, _ := app.NewPhotoService(store, fakeExif{}, repo)
	if _, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), bytes.NewReader([]byte("x")), ""); !errors.Is(err, domain.ErrUnsupportedMediaType) {
		t.Fatalf("Upload error = %v, want ErrUnsupportedMediaType", err)
	}
	if len(repo.created) != 0 {
		t.Fatal("store error must not persist a photo")
	}
}

// TestPhotoServiceUploadDoesNotCleanUpOnCreateError covers the invariant a
// failure after Put must not delete stored bytes: the object is
// content-addressed and may be shared by another (or a soon-to-commit
// concurrent) photo row, so the upload path leaves it in place on any
// post-Put failure — an orphan candidate for the planned NES-132/133 reaper,
// never a synchronous delete.
func TestPhotoServiceUploadDoesNotCleanUpOnCreateError(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakePhotoRepo()
	repo.createErr = errors.New("db down")
	svc, _ := app.NewPhotoService(store, fakeExif{}, repo)
	if _, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), bytes.NewReader([]byte("x")), ""); err == nil {
		t.Fatal("Upload should fail when Create fails")
	}
	if len(store.deleted) != 0 {
		t.Fatalf("Upload must not delete stored bytes on a Create failure, deleted=%v", store.deleted)
	}
}

// TestPhotoServiceUploadDoesNotCleanUpOnExifReopenError covers the same
// no-synchronous-delete invariant for the failure path where PhotoStore.Open
// (used to feed the ExifReader) errors after Put already succeeded.
func TestPhotoServiceUploadDoesNotCleanUpOnExifReopenError(t *testing.T) {
	store := &fakePhotoStore{openErr: errors.New("disk hiccup")}
	repo := newFakePhotoRepo()
	svc, _ := app.NewPhotoService(store, fakeExif{}, repo)
	if _, err := svc.Upload(context.Background(), household.NewHouseholdID(), household.NewMemberID(), bytes.NewReader([]byte("x")), ""); err == nil {
		t.Fatal("Upload should fail when the exif reopen fails")
	}
	if len(store.deleted) != 0 {
		t.Fatalf("Upload must not delete stored bytes on an exif reopen failure, deleted=%v", store.deleted)
	}
	if len(repo.created) != 0 {
		t.Fatal("exif reopen error must not persist a photo")
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

// TestPhotoServiceDeleteIsRowsOnly covers the invariant documented on
// Delete: a successful delete removes the metadata row but never touches the
// stored bytes, since owning a row is not the same as exclusively owning its
// ref (a legacy duplicate row, or a concurrent re-upload racing this delete,
// can still depend on it).
func TestPhotoServiceDeleteIsRowsOnly(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakePhotoRepo()
	hh := household.NewHouseholdID()
	id := domain.NewPhotoID()
	repo.store[id] = &domain.Photo{ID: id, HouseholdID: hh, StorageRef: "hh/aa/x.jpg"}
	svc, _ := app.NewPhotoService(store, fakeExif{}, repo)

	if err := svc.Delete(context.Background(), hh, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(repo.deleted) != 1 || repo.deleted[0] != id {
		t.Fatalf("Delete did not remove the metadata row: deleted=%v", repo.deleted)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("Delete must never remove stored bytes, got deleted=%v", store.deleted)
	}
}

// TestPhotoServiceRawServeStreamsWhenBackendLacksDirectURL covers
// RawServe's local-backend branch (NES-132): SupportsDirectURL false means
// RawServe opens and returns a Body to stream, never a RedirectURL.
func TestPhotoServiceRawServeStreamsWhenBackendLacksDirectURL(t *testing.T) {
	store := &fakePhotoStore{}
	repo := newFakePhotoRepo()
	hh := household.NewHouseholdID()
	id := domain.NewPhotoID()
	repo.store[id] = &domain.Photo{ID: id, HouseholdID: hh, StorageRef: "hh/aa/x.jpg"}
	svc, _ := app.NewPhotoService(store, fakeExif{}, repo)

	result, err := svc.RawServe(context.Background(), hh, id)
	if err != nil {
		t.Fatalf("RawServe: %v", err)
	}
	if result.RedirectURL != "" {
		t.Fatalf("RedirectURL = %q, want empty for a local-like backend", result.RedirectURL)
	}
	if result.Body == nil {
		t.Fatal("Body is nil, want a stream for a local-like backend")
	}
	_ = result.Body.Close()
	if store.openCalls != 1 {
		t.Fatalf("Open was called %d times, want 1", store.openCalls)
	}
}

// TestPhotoServiceRawServeRedirectsWhenBackendSupportsDirectURL covers
// RawServe's S3-like backend branch: SupportsDirectURL true means RawServe
// calls URL and returns a RedirectURL, never opening/streaming a body.
func TestPhotoServiceRawServeRedirectsWhenBackendSupportsDirectURL(t *testing.T) {
	store := &fakePhotoStore{directURL: true}
	repo := newFakePhotoRepo()
	hh := household.NewHouseholdID()
	id := domain.NewPhotoID()
	repo.store[id] = &domain.Photo{ID: id, HouseholdID: hh, StorageRef: "households/hh/photos/aa/x.jpg"}
	svc, _ := app.NewPhotoService(store, fakeExif{}, repo)

	result, err := svc.RawServe(context.Background(), hh, id)
	if err != nil {
		t.Fatalf("RawServe: %v", err)
	}
	if result.RedirectURL != "households/hh/photos/aa/x.jpg" {
		t.Fatalf("RedirectURL = %q, want the fake store's URL() result", result.RedirectURL)
	}
	if result.Body != nil {
		t.Fatal("Body is non-nil, want none for an S3-like backend redirect")
	}
	if store.openCalls != 0 {
		t.Fatalf("Open was called %d times, want 0 (redirect must never open/stream)", store.openCalls)
	}
}

// TestPhotoServiceRawServeRejectsOtherHousehold mirrors
// TestPhotoServiceDeleteRejectsOtherHousehold: RawServe must enforce
// ownership BEFORE consulting the store at all, regardless of backend.
func TestPhotoServiceRawServeRejectsOtherHousehold(t *testing.T) {
	store := &fakePhotoStore{directURL: true}
	repo := newFakePhotoRepo()
	other := household.NewHouseholdID()
	id := domain.NewPhotoID()
	repo.store[id] = &domain.Photo{ID: id, HouseholdID: other, StorageRef: "x/y/z.jpg"}
	svc, _ := app.NewPhotoService(store, fakeExif{}, repo)

	if _, err := svc.RawServe(context.Background(), household.NewHouseholdID(), id); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("cross-household RawServe = %v, want ErrPhotoNotFound", err)
	}
	if store.openCalls != 0 {
		t.Fatal("cross-household RawServe must never touch the store")
	}
	// directURL is true on this fake specifically so a bug that checked
	// ownership AFTER branching on SupportsDirectURL would still be caught:
	// URL must never be called either.
	if store.urlCalls != 0 {
		t.Fatal("cross-household RawServe must never call URL")
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

func TestAlbumServiceRemoveAndReorderCheckPhotoOwnership(t *testing.T) {
	albums := newFakeAlbumRepo()
	photos := newFakePhotoRepo()
	hh := household.NewHouseholdID()
	album := &domain.Album{ID: domain.NewAlbumID(), HouseholdID: hh, Name: "A", Rotation: rotation(t, 5)}
	albums.store[album.ID] = album
	foreign := &domain.Photo{ID: domain.NewPhotoID(), HouseholdID: household.NewHouseholdID(), StorageRef: "x.jpg"}
	photos.store[foreign.ID] = foreign
	svc, _ := app.NewAlbumService(albums, photos, &fakeAlbumPhotoRepo{})

	if err := svc.RemovePhoto(context.Background(), hh, album.ID, foreign.ID); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("cross-household RemovePhoto = %v, want ErrPhotoNotFound", err)
	}
	if err := svc.Reorder(context.Background(), hh, album.ID, []domain.PhotoID{foreign.ID}); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("cross-household Reorder = %v, want ErrPhotoNotFound", err)
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
