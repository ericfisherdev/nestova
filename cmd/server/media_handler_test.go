package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediaapp "github.com/ericfisherdev/nestova/internal/media/app"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
)

// mediaTestMaxUploadBytes is the per-photo cap fed to NewWebHandlers in these
// tests, mirroring how main.go wires cfg.Media.MaxUploadBytes.
const mediaTestMaxUploadBytes = 10 << 20

// --- media fakes ---

type fakeMediaStore struct {
	puts   int
	bytes  []byte
	putErr error
}

// Put hashes the bytes it's given (like the real content-addressed store) so
// tests can exercise dedup by uploading identical content twice.
func (f *fakeMediaStore) Put(_ context.Context, _ household.HouseholdID, r io.Reader) (mediadomain.PutResult, error) {
	if f.putErr != nil {
		return mediadomain.PutResult{}, f.putErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return mediadomain.PutResult{}, err
	}
	f.puts++
	sum := sha256.Sum256(data)
	return mediadomain.PutResult{
		Ref:         mediadomain.StorageRef("hh/aa/stored.jpg"),
		ContentHash: hex.EncodeToString(sum[:]),
		SizeBytes:   int64(len(data)),
		ContentType: "image/jpeg",
	}, nil
}

func (f *fakeMediaStore) Open(context.Context, mediadomain.StorageRef) (mediadomain.PhotoReader, error) {
	return fakeMediaPhotoReader{bytes.NewReader(f.bytes)}, nil
}
func (f *fakeMediaStore) Delete(context.Context, mediadomain.StorageRef) error { return nil }

// fakeMediaPhotoReader adapts a *bytes.Reader (already Read+ReadAt+Seek) into
// a mediadomain.PhotoReader with a no-op Close.
type fakeMediaPhotoReader struct{ *bytes.Reader }

func (fakeMediaPhotoReader) Close() error { return nil }

type fakeMediaExif struct{}

func (fakeMediaExif) TakenAt(mediadomain.RandomAccessReader) *time.Time { return nil }

type fakeMediaPhotoRepo struct {
	created []*mediadomain.Photo
	store   map[mediadomain.PhotoID]*mediadomain.Photo
}

func newFakeMediaPhotoRepo() *fakeMediaPhotoRepo {
	return &fakeMediaPhotoRepo{store: map[mediadomain.PhotoID]*mediadomain.Photo{}}
}

func (f *fakeMediaPhotoRepo) Create(_ context.Context, p *mediadomain.Photo) error {
	f.store[p.ID] = p
	f.created = append(f.created, p)
	return nil
}

func (f *fakeMediaPhotoRepo) Get(_ context.Context, id mediadomain.PhotoID) (*mediadomain.Photo, error) {
	if p, ok := f.store[id]; ok {
		return p, nil
	}
	return nil, mediadomain.ErrPhotoNotFound
}

func (f *fakeMediaPhotoRepo) FindByContentHash(_ context.Context, householdID household.HouseholdID, hash string) (*mediadomain.Photo, error) {
	if hash == "" {
		return nil, mediadomain.ErrPhotoNotFound
	}
	for _, p := range f.store {
		if p.HouseholdID == householdID && p.ContentHash == hash {
			return p, nil
		}
	}
	return nil, mediadomain.ErrPhotoNotFound
}

func (f *fakeMediaPhotoRepo) ListByHousehold(context.Context, household.HouseholdID) ([]*mediadomain.Photo, error) {
	return nil, nil
}

func (f *fakeMediaPhotoRepo) Delete(_ context.Context, id mediadomain.PhotoID) error {
	delete(f.store, id)
	return nil
}

type fakeMediaAlbumRepo struct {
	store map[mediadomain.AlbumID]*mediadomain.Album
}

func newFakeMediaAlbumRepo() *fakeMediaAlbumRepo {
	return &fakeMediaAlbumRepo{store: map[mediadomain.AlbumID]*mediadomain.Album{}}
}

func (f *fakeMediaAlbumRepo) Create(_ context.Context, a *mediadomain.Album) error {
	f.store[a.ID] = a
	return nil
}

func (f *fakeMediaAlbumRepo) Get(_ context.Context, id mediadomain.AlbumID) (*mediadomain.Album, error) {
	if a, ok := f.store[id]; ok {
		return a, nil
	}
	return nil, mediadomain.ErrAlbumNotFound
}

func (f *fakeMediaAlbumRepo) Update(_ context.Context, a *mediadomain.Album) error {
	f.store[a.ID] = a
	return nil
}

func (f *fakeMediaAlbumRepo) ListByHousehold(_ context.Context, householdID household.HouseholdID) ([]*mediadomain.Album, error) {
	var out []*mediadomain.Album
	for _, a := range f.store {
		if a.HouseholdID == householdID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeMediaAlbumRepo) Delete(_ context.Context, id mediadomain.AlbumID) error {
	delete(f.store, id)
	return nil
}

type fakeMediaAlbumPhotoRepo struct{}

func (fakeMediaAlbumPhotoRepo) Add(context.Context, mediadomain.AlbumID, mediadomain.PhotoID) error {
	return nil
}

func (fakeMediaAlbumPhotoRepo) Remove(context.Context, mediadomain.AlbumID, mediadomain.PhotoID) error {
	return nil
}

func (fakeMediaAlbumPhotoRepo) Reorder(context.Context, mediadomain.AlbumID, []mediadomain.PhotoID) error {
	return nil
}

func (fakeMediaAlbumPhotoRepo) ListByAlbumOrdered(context.Context, mediadomain.AlbumID) ([]*mediadomain.Photo, error) {
	return nil, nil
}

func buildMediaTestHandler(t *testing.T, member *household.Member, store *fakeMediaStore, photoRepo *fakeMediaPhotoRepo) (http.Handler, *scs.SessionManager, *fakeMediaAlbumRepo) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	householdRepo := authedHouseholdRepo{member: member}
	authHandlers := authadapter.NewHandlers(sm, authapp.New(testCredRepo{}), logger)

	albumRepo := newFakeMediaAlbumRepo()
	photoService, err := mediaapp.NewPhotoService(store, fakeMediaExif{}, photoRepo)
	if err != nil {
		t.Fatalf("NewPhotoService: %v", err)
	}
	albumService, err := mediaapp.NewAlbumService(albumRepo, photoRepo, fakeMediaAlbumPhotoRepo{})
	if err != nil {
		t.Fatalf("NewAlbumService: %v", err)
	}
	handlers := mediaadapter.NewWebHandlers(albumService, photoService, householdRepo, sm, logger, mediaTestMaxUploadBytes)

	layoutFn := func(*household.Member) func(templ.Component) templ.Component {
		return func(c templ.Component) templ.Component { return c }
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	requireMember := authadapter.RequireMember(sm)
	mux.Handle("GET /photos", requireMember(http.HandlerFunc(handlers.Page(layoutFn))))
	mux.Handle("POST /photos", requireMember(http.HandlerFunc(handlers.Upload)))
	mux.Handle("GET /photos/{id}/raw", requireMember(http.HandlerFunc(handlers.Raw)))
	mux.Handle("GET /album/{id}", requireMember(http.HandlerFunc(handlers.AlbumViewer)))
	return sm.LoadAndSave(authadapter.Authenticate(sm, householdRepo)(mux)), sm, albumRepo
}

// multipartUpload builds a multipart body with a csrf field and a "photo" part.
func multipartUpload(t *testing.T, csrf string, data []byte) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("csrf_token", csrf)
	_ = mw.WriteField("caption", "Holiday")
	part, err := mw.CreateFormFile("photo", "p.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	_, _ = part.Write(data)
	_ = mw.Close()
	return mw.FormDataContentType(), &buf
}

func TestMediaUploadPersistsAndRedirects(t *testing.T) {
	member := testMember()
	store := &fakeMediaStore{}
	repo := newFakeMediaPhotoRepo()
	handler, sm, _ := buildMediaTestHandler(t, member, store, repo)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	ct, body := multipartUpload(t, csrf, []byte("imgbytes"))
	req := httptest.NewRequest(http.MethodPost, "/photos", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/photos" {
		t.Fatalf("upload: status=%d hx-redirect=%q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	if rec.Header().Get("X-Upload-Result") != "created" {
		t.Fatalf("X-Upload-Result = %q, want created", rec.Header().Get("X-Upload-Result"))
	}
	if store.puts != 1 || len(repo.created) != 1 {
		t.Fatalf("upload side effects: puts=%d created=%d", store.puts, len(repo.created))
	}
	if repo.created[0].HouseholdID != member.HouseholdID || repo.created[0].UploadedBy == nil || *repo.created[0].UploadedBy != member.ID {
		t.Fatalf("uploaded photo attribution wrong: %+v", repo.created[0])
	}
}

// TestMediaUploadDedupSetsResultHeader covers AC3 end-to-end through the
// handler: uploading the same bytes twice creates exactly one photo row (one
// PhotoRepository.Create), and the second response reports the upload as a
// duplicate via X-Upload-Result rather than erroring.
func TestMediaUploadDedupSetsResultHeader(t *testing.T) {
	member := testMember()
	store := &fakeMediaStore{}
	repo := newFakeMediaPhotoRepo()
	handler, sm, _ := buildMediaTestHandler(t, member, store, repo)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	upload := func() *httptest.ResponseRecorder {
		ct, body := multipartUpload(t, csrf, []byte("same-bytes"))
		req := httptest.NewRequest(http.MethodPost, "/photos", body)
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Cookie", cookie)
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	first := upload()
	if first.Code != http.StatusOK || first.Header().Get("X-Upload-Result") != "created" {
		t.Fatalf("first upload: status=%d result=%q", first.Code, first.Header().Get("X-Upload-Result"))
	}

	second := upload()
	if second.Code != http.StatusOK || second.Header().Get("X-Upload-Result") != "duplicate" {
		t.Fatalf("second (duplicate) upload: status=%d result=%q", second.Code, second.Header().Get("X-Upload-Result"))
	}
	if store.puts != 2 {
		t.Fatalf("PhotoStore.Put called %d times, want 2 (one per upload attempt)", store.puts)
	}
	if len(repo.created) != 1 {
		t.Fatalf("photo rows created = %d, want 1 (the duplicate must not create a second row)", len(repo.created))
	}
}

// TestMediaUploadRejectsUnsupportedMediaType covers AC2 at the handler level:
// a rejection surfaced by the store (as ErrUnsupportedMediaType — real
// content sniffing is exercised directly against LocalPhotoStore in
// internal/media/adapter/photo_store_test.go) maps to 415 and persists
// nothing.
func TestMediaUploadRejectsUnsupportedMediaType(t *testing.T) {
	member := testMember()
	store := &fakeMediaStore{putErr: mediadomain.ErrUnsupportedMediaType}
	repo := newFakeMediaPhotoRepo()
	handler, sm, _ := buildMediaTestHandler(t, member, store, repo)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	ct, body := multipartUpload(t, csrf, []byte("not really an image"))
	req := httptest.NewRequest(http.MethodPost, "/photos", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("unsupported-type upload: status=%d, want 415", rec.Code)
	}
	if len(repo.created) != 0 {
		t.Fatal("rejected upload must not persist")
	}
}

// TestMediaUploadRejectsOversizeRequestBody covers AC1 at the handler level:
// a request body beyond the configured per-photo limit (plus overhead) is
// rejected before it ever reaches PhotoService/PhotoStore.
func TestMediaUploadRejectsOversizeRequestBody(t *testing.T) {
	member := testMember()
	store := &fakeMediaStore{}
	repo := newFakeMediaPhotoRepo()
	handler, sm, _ := buildMediaTestHandler(t, member, store, repo)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	oversized := make([]byte, mediaTestMaxUploadBytes+(1<<20)) // 1 MiB past the configured cap
	ct, body := multipartUpload(t, csrf, oversized)
	req := httptest.NewRequest(http.MethodPost, "/photos", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload: status=%d, want 413", rec.Code)
	}
	if store.puts != 0 || len(repo.created) != 0 {
		t.Fatal("oversize upload must not persist")
	}
}

func TestMediaUploadRejectsBadCSRF(t *testing.T) {
	member := testMember()
	store := &fakeMediaStore{}
	repo := newFakeMediaPhotoRepo()
	handler, sm, _ := buildMediaTestHandler(t, member, store, repo)
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	ct, body := multipartUpload(t, "wrong", []byte("x"))
	req := httptest.NewRequest(http.MethodPost, "/photos", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad-CSRF upload: status=%d, want 403", rec.Code)
	}
	if store.puts != 0 || len(repo.created) != 0 {
		t.Fatal("bad-CSRF upload must not persist")
	}
}

func TestMediaUploadRequiresMember(t *testing.T) {
	handler, _, _ := buildMediaTestHandler(t, testMember(), &fakeMediaStore{}, newFakeMediaPhotoRepo())
	ct, body := multipartUpload(t, "x", []byte("x"))
	req := httptest.NewRequest(http.MethodPost, "/photos", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated upload: status=%d, want 401", rec.Code)
	}
}

func TestMediaRawStreamsToOwnerAndRejectsOthers(t *testing.T) {
	member := testMember()
	store := &fakeMediaStore{bytes: []byte("the-image-bytes")}
	repo := newFakeMediaPhotoRepo()
	handler, sm, _ := buildMediaTestHandler(t, member, store, repo)
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	// Owned photo: streams the bytes.
	owned := mediadomain.NewPhotoID()
	repo.store[owned] = &mediadomain.Photo{ID: owned, HouseholdID: member.HouseholdID, StorageRef: "hh/aa/x.jpg"}
	req := httptest.NewRequest(http.MethodGet, "/photos/"+owned.String()+"/raw", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "the-image-bytes") {
		t.Fatalf("owned raw: status=%d body=%q", rec.Code, rec.Body.String())
	}
	// Served with an explicit image content type and no MIME sniffing.
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing X-Content-Type-Options: nosniff")
	}

	// Cross-household photo: 404, no bytes.
	foreign := mediadomain.NewPhotoID()
	repo.store[foreign] = &mediadomain.Photo{ID: foreign, HouseholdID: household.NewHouseholdID(), StorageRef: "other/x.jpg"}
	req = httptest.NewRequest(http.MethodGet, "/photos/"+foreign.String()+"/raw", nil)
	req.Header.Set("Cookie", cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-household raw: status=%d, want 404", rec.Code)
	}
}

func TestAlbumViewerOwnershipAndRender(t *testing.T) {
	member := testMember()
	handler, sm, albumRepo := buildMediaTestHandler(t, member, &fakeMediaStore{}, newFakeMediaPhotoRepo())
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	rot, _ := mediadomain.NewRotationInterval(8)
	owned := mediadomain.NewAlbumID()
	albumRepo.store[owned] = &mediadomain.Album{ID: owned, HouseholdID: member.HouseholdID, Name: "Family", Rotation: rot}

	// Owned album renders the standalone viewer page.
	req := httptest.NewRequest(http.MethodGet, "/album/"+owned.String(), nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owned album viewer: status=%d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/static/js/album.js") || !strings.Contains(rec.Body.String(), "Family") {
		t.Fatalf("viewer page missing expected content")
	}

	// Cross-household album: 404.
	foreign := mediadomain.NewAlbumID()
	albumRepo.store[foreign] = &mediadomain.Album{ID: foreign, HouseholdID: household.NewHouseholdID(), Name: "Theirs", Rotation: rot}
	req = httptest.NewRequest(http.MethodGet, "/album/"+foreign.String(), nil)
	req.Header.Set("Cookie", cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-household album viewer: status=%d, want 404", rec.Code)
	}
}
