package main

import (
	"bytes"
	"context"
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

// --- media fakes ---

type fakeMediaStore struct {
	puts  int
	bytes []byte
}

func (f *fakeMediaStore) Put(context.Context, household.HouseholdID, []byte, string) (mediadomain.StorageRef, error) {
	f.puts++
	return mediadomain.StorageRef("hh/aa/stored.jpg"), nil
}

func (f *fakeMediaStore) Open(context.Context, mediadomain.StorageRef) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.bytes)), nil
}
func (f *fakeMediaStore) Delete(context.Context, mediadomain.StorageRef) error { return nil }

type fakeMediaExif struct{}

func (fakeMediaExif) TakenAt([]byte) *time.Time { return nil }

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
	handlers := mediaadapter.NewWebHandlers(albumService, photoService, householdRepo, sm, logger)

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
	if store.puts != 1 || len(repo.created) != 1 {
		t.Fatalf("upload side effects: puts=%d created=%d", store.puts, len(repo.created))
	}
	if repo.created[0].HouseholdID != member.HouseholdID || repo.created[0].UploadedBy == nil || *repo.created[0].UploadedBy != member.ID {
		t.Fatalf("uploaded photo attribution wrong: %+v", repo.created[0])
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
