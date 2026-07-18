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

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediaapp "github.com/ericfisherdev/nestova/internal/media/app"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
)

// choreProofTestMaxUploadBytes mirrors how main.go wires cfg.Media.MaxUploadBytes.
const choreProofTestMaxUploadBytes = 10 << 20

// choreProofTestFreshnessWindow mirrors main.go wiring cfg.Media.ChoreProofFreshnessWindow.
const choreProofTestFreshnessWindow = time.Hour

// --- fakes ---

type fakeChoreProofExifHandler struct {
	taken       *time.Time
	orientation int
}

func (f fakeChoreProofExifHandler) TakenAtAndOrientation([]byte) (*time.Time, int) {
	return f.taken, f.orientation
}

func (f fakeChoreProofExifHandler) Scrub(data []byte, _ int) ([]byte, error) { return data, nil }

// fakeTaskInstancePhotoRepoHandler fakes mediadomain.TaskInstancePhotoRepository.
// instanceExists defaults to true via newFakeTaskInstancePhotoRepoHandler
// (below), so a test not exercising the InstanceExists preflight sails past
// it.
type fakeTaskInstancePhotoRepoHandler struct {
	created        []*mediadomain.TaskInstancePhoto
	latestTakenAt  time.Time
	latestOK       bool
	instanceExists bool
	// store backs Get (NES-120/NES-132's raw-serving route tests); nil/empty
	// means every Get reports ErrTaskInstancePhotoNotFound, matching a
	// genuinely unknown id.
	store map[mediadomain.TaskInstancePhotoID]*mediadomain.TaskInstancePhoto
	// backend is the StorageBackend Create stamps onto every row it writes
	// (mirroring the real TaskInstancePhotoRepository.Create), defaulting
	// to mediadomain.StorageBackendLocal via newFakeTaskInstancePhotoRepoHandler.
	backend mediadomain.StorageBackend
}

func newFakeTaskInstancePhotoRepoHandler() *fakeTaskInstancePhotoRepoHandler {
	return &fakeTaskInstancePhotoRepoHandler{instanceExists: true, backend: mediadomain.StorageBackendLocal}
}

func (f *fakeTaskInstancePhotoRepoHandler) Create(_ context.Context, p *mediadomain.TaskInstancePhoto) error {
	p.UploadedAt = time.Now().UTC()
	p.StorageBackend = f.backend
	f.created = append(f.created, p)
	return nil
}

func (f *fakeTaskInstancePhotoRepoHandler) InstanceExists(context.Context, household.HouseholdID, mediadomain.TaskInstanceID) (bool, error) {
	return f.instanceExists, nil
}

func (f *fakeTaskInstancePhotoRepoHandler) LatestTakenAt(context.Context, household.HouseholdID, mediadomain.TaskInstanceID, mediadomain.PhotoKind) (time.Time, bool, error) {
	return f.latestTakenAt, f.latestOK, nil
}

func (f *fakeTaskInstancePhotoRepoHandler) ListByInstance(context.Context, household.HouseholdID, mediadomain.TaskInstanceID) ([]*mediadomain.TaskInstancePhoto, error) {
	return nil, nil
}

// ListByInstances is unused by this file's upload-focused tests; implemented
// only to satisfy the interface (NES-120 added it for the /tasks list
// builder's batched photo lookup).
func (f *fakeTaskInstancePhotoRepoHandler) ListByInstances(context.Context, household.HouseholdID, []mediadomain.TaskInstanceID) ([]*mediadomain.TaskInstancePhoto, error) {
	return nil, nil
}

// Get is deliberately ID-only (NES-120), mirroring PhotoRepository.Get:
// ownership is enforced by the caller (ChoreProofPhotoService.ownedPhoto),
// not this fake.
func (f *fakeTaskInstancePhotoRepoHandler) Get(_ context.Context, id mediadomain.TaskInstancePhotoID) (*mediadomain.TaskInstancePhoto, error) {
	if p, ok := f.store[id]; ok {
		return p, nil
	}
	return nil, mediadomain.ErrTaskInstancePhotoNotFound
}

// ListAllStorageRefs / DeleteUploadedBefore are unused by this file's
// upload-focused tests; implemented only to satisfy the interface (NES-132
// added them for the storage reaper).
func (f *fakeTaskInstancePhotoRepoHandler) ListAllStorageRefs(context.Context, mediadomain.StorageBackend) ([]mediadomain.StorageRef, error) {
	return nil, nil
}

func (f *fakeTaskInstancePhotoRepoHandler) DeleteUploadedBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}

// ExistsByStorageRef is unused by this file's upload-focused tests;
// implemented only to satisfy the interface (NES-132's reaper TOCTOU
// recheck).
func (f *fakeTaskInstancePhotoRepoHandler) ExistsByStorageRef(context.Context, mediadomain.StorageRef, mediadomain.StorageBackend) (bool, error) {
	return false, nil
}

// buildChoreProofTestHandler wires just enough of the composition root
// (login + the chore-proof upload route) to exercise
// ChoreProofWebHandlers.Upload end to end with fakes, mirroring
// buildMediaTestHandler's shape for the sibling /photos endpoint.
func buildChoreProofTestHandler(t *testing.T, member *household.Member, store *fakeMediaStore, exif fakeChoreProofExifHandler, repo *fakeTaskInstancePhotoRepoHandler) (http.Handler, *scs.SessionManager) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	householdRepo := authedHouseholdRepo{member: member}
	authHandlers := authadapter.NewHandlers(sm, authapp.New(testCredRepo{}), logger)

	svc, err := mediaapp.NewChoreProofPhotoService(newFakeStoreResolver(mediadomain.StorageBackendLocal, store), mediadomain.StorageBackendLocal, exif, repo, choreProofTestMaxUploadBytes, choreProofTestFreshnessWindow)
	if err != nil {
		t.Fatalf("NewChoreProofPhotoService: %v", err)
	}
	handlers := mediaadapter.NewChoreProofWebHandlers(svc, sm, logger, choreProofTestMaxUploadBytes)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	requireMember := authadapter.RequireMember(sm)
	mux.Handle("POST /tasks/{id}/photos", requireMember(http.HandlerFunc(handlers.Upload)))
	mux.Handle("GET /tasks/photos/{id}/raw", requireMember(http.HandlerFunc(handlers.Raw)))
	return sm.LoadAndSave(authadapter.Authenticate(sm, householdRepo)(mux)), sm
}

// choreProofMultipartUpload builds a multipart body with a csrf field, a kind
// field, and a "photo" part.
func choreProofMultipartUpload(t *testing.T, csrf, kind string, data []byte) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("csrf_token", csrf)
	_ = mw.WriteField("kind", kind)
	part, err := mw.CreateFormFile("photo", "p.jpg")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	_, _ = part.Write(data)
	_ = mw.Close()
	return mw.FormDataContentType(), &buf
}

func TestChoreProofUploadPersistsAndRedirects(t *testing.T) {
	member := testMember()
	store := &fakeMediaStore{}
	taken := time.Now().UTC().Add(-5 * time.Minute)
	exif := fakeChoreProofExifHandler{taken: &taken, orientation: 1}
	repo := newFakeTaskInstancePhotoRepoHandler()
	handler, sm := buildChoreProofTestHandler(t, member, store, exif, repo)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	ct, body := choreProofMultipartUpload(t, csrf, "before", []byte{0xFF, 0xD8, 'x'})
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+household.NewMemberID().String()+"/photos", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/tasks" {
		t.Fatalf("upload: status=%d hx-redirect=%q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	if len(repo.created) != 1 {
		t.Fatalf("created %d photo rows, want 1", len(repo.created))
	}
	if repo.created[0].HouseholdID != member.HouseholdID || repo.created[0].UploadedBy == nil || *repo.created[0].UploadedBy != member.ID {
		t.Fatalf("uploaded photo attribution wrong: %+v", repo.created[0])
	}
	if repo.created[0].Kind != mediadomain.PhotoKindBefore {
		t.Fatalf("Kind = %v, want PhotoKindBefore", repo.created[0].Kind)
	}
}

// TestChoreProofUploadRejectsMissingTimestamp covers AC2 at the handler
// level: a screenshot/EXIF-stripped upload is rejected with a message
// telling the user to take a new photo, and nothing is persisted.
func TestChoreProofUploadRejectsMissingTimestamp(t *testing.T) {
	member := testMember()
	store := &fakeMediaStore{}
	exif := fakeChoreProofExifHandler{taken: nil}
	repo := newFakeTaskInstancePhotoRepoHandler()
	handler, sm := buildChoreProofTestHandler(t, member, store, exif, repo)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	ct, body := choreProofMultipartUpload(t, csrf, "before", []byte{0xFF, 0xD8, 'x'})
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+household.NewMemberID().String()+"/photos", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing-timestamp upload: status=%d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "take a new photo") {
		t.Fatalf("error message = %q, want it to tell the user to take a new photo", rec.Body.String())
	}
	if len(repo.created) != 0 {
		t.Fatal("rejected upload must not persist")
	}
}

func TestChoreProofUploadRejectsInvalidKind(t *testing.T) {
	member := testMember()
	store := &fakeMediaStore{}
	exif := fakeChoreProofExifHandler{}
	repo := newFakeTaskInstancePhotoRepoHandler()
	handler, sm := buildChoreProofTestHandler(t, member, store, exif, repo)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	ct, body := choreProofMultipartUpload(t, csrf, "sideways", []byte{0xFF, 0xD8, 'x'})
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+household.NewMemberID().String()+"/photos", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid-kind upload: status=%d, want 400", rec.Code)
	}
	if len(repo.created) != 0 {
		t.Fatal("rejected upload must not persist")
	}
}

func TestChoreProofUploadRejectsMissingFile(t *testing.T) {
	member := testMember()
	handler, sm := buildChoreProofTestHandler(t, member, &fakeMediaStore{}, fakeChoreProofExifHandler{}, newFakeTaskInstancePhotoRepoHandler())
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("csrf_token", csrf)
	_ = mw.WriteField("kind", "before")
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+household.NewMemberID().String()+"/photos", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing-file upload: status=%d, want 400", rec.Code)
	}
}

func TestChoreProofUploadRejectsBadCSRF(t *testing.T) {
	member := testMember()
	handler, sm := buildChoreProofTestHandler(t, member, &fakeMediaStore{}, fakeChoreProofExifHandler{}, newFakeTaskInstancePhotoRepoHandler())
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	ct, body := choreProofMultipartUpload(t, "wrong-token", "before", []byte{0xFF, 0xD8, 'x'})
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+household.NewMemberID().String()+"/photos", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad-CSRF upload: status=%d, want 403", rec.Code)
	}
}

func TestChoreProofUploadRequiresMember(t *testing.T) {
	handler, _ := buildChoreProofTestHandler(t, testMember(), &fakeMediaStore{}, fakeChoreProofExifHandler{}, newFakeTaskInstancePhotoRepoHandler())

	ct, body := choreProofMultipartUpload(t, "x", "before", []byte{0xFF, 0xD8, 'x'})
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+household.NewMemberID().String()+"/photos", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated upload: status=%d, want 401", rec.Code)
	}
}

// TestChoreProofUploadPreflightRejectsUnknownInstance covers the
// InstanceExists preflight (NES-119 review) at the handler level: an
// unknown/cross-household task instance is rejected with 404 and nothing is
// persisted.
func TestChoreProofUploadPreflightRejectsUnknownInstance(t *testing.T) {
	member := testMember()
	repo := newFakeTaskInstancePhotoRepoHandler()
	repo.instanceExists = false
	handler, sm := buildChoreProofTestHandler(t, member, &fakeMediaStore{}, fakeChoreProofExifHandler{}, repo)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	ct, body := choreProofMultipartUpload(t, csrf, "before", []byte{0xFF, 0xD8, 'x'})
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+household.NewMemberID().String()+"/photos", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown-instance upload: status=%d, want 404", rec.Code)
	}
	if len(repo.created) != 0 {
		t.Fatal("rejected upload must not persist")
	}
}

// TestChoreProofUploadRejectsOversizeRequestBody covers the NES-119 review's
// split of http.MaxBytesError (413) from any other multipart parse failure
// (400): a request body beyond the configured cap is rejected specifically
// as "too large", mirroring TestMediaUploadRejectsOversizeRequestBody's
// coverage of the sibling /photos endpoint.
func TestChoreProofUploadRejectsOversizeRequestBody(t *testing.T) {
	member := testMember()
	handler, sm := buildChoreProofTestHandler(t, member, &fakeMediaStore{}, fakeChoreProofExifHandler{}, newFakeTaskInstancePhotoRepoHandler())
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	oversized := make([]byte, choreProofTestMaxUploadBytes+(1<<20)) // 1 MiB past the configured cap
	ct, body := choreProofMultipartUpload(t, csrf, "before", oversized)
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+household.NewMemberID().String()+"/photos", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload: status=%d, want 413", rec.Code)
	}
}

// TestChoreProofUploadRejectsMalformedMultipartAsBadRequest covers the other
// half of the NES-119 review's split: a small (well within the size cap)
// but structurally malformed multipart body — not an http.MaxBytesError at
// all — is rejected as 400, not 413.
func TestChoreProofUploadRejectsMalformedMultipartAsBadRequest(t *testing.T) {
	member := testMember()
	handler, sm := buildChoreProofTestHandler(t, member, &fakeMediaStore{}, fakeChoreProofExifHandler{}, newFakeTaskInstancePhotoRepoHandler())
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+household.NewMemberID().String()+"/photos", strings.NewReader("this is not a multipart body at all"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=doesnotmatchbody")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed multipart upload: status=%d, want 400", rec.Code)
	}
}

// TestChoreProofRawRedirectsWhenBackendSupportsDirectURL covers NES-132's
// AC1 for the chore-proof route: with an S3-like backend, GET
// /tasks/photos/{id}/raw must 302-redirect to a presigned URL rather than
// streaming a body — mirroring
// TestMediaRawRedirectsWhenBackendSupportsDirectURL's album-path coverage,
// one table over.
func TestChoreProofRawRedirectsWhenBackendSupportsDirectURL(t *testing.T) {
	member := testMember()
	store := &fakeMediaStore{bytes: []byte("the-image-bytes"), directURL: true}
	repo := newFakeTaskInstancePhotoRepoHandler()
	handler, sm := buildChoreProofTestHandler(t, member, store, fakeChoreProofExifHandler{}, repo)
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	owned := mediadomain.NewTaskInstancePhotoID()
	repo.store = map[mediadomain.TaskInstancePhotoID]*mediadomain.TaskInstancePhoto{
		owned: {ID: owned, HouseholdID: member.HouseholdID, StorageRef: "households/hh/chore-photos/aa/x.jpg", ContentType: "image/jpeg", StorageBackend: mediadomain.StorageBackendLocal},
	}
	req := httptest.NewRequest(http.MethodGet, "/tasks/photos/"+owned.String()+"/raw", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("raw with a direct-URL backend: status=%d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "nestova-photos") || !strings.Contains(loc, "X-Amz-Signature") {
		t.Fatalf("Location = %q, want a presigned URL containing the bucket and a signature parameter", loc)
	}
	// The redirect RESPONSE ITSELF (carrying the presigned URL, a
	// short-lived bearer credential) must never be cached — no-store, not
	// just private.
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("redirect Cache-Control = %q, want %q", cc, "private, no-store")
	}
	if strings.Contains(rec.Body.String(), "the-image-bytes") {
		t.Fatal("raw redirected but ALSO streamed bytes through the Go process")
	}

	// Cross-household photo still 404s before any redirect is built.
	foreign := mediadomain.NewTaskInstancePhotoID()
	repo.store[foreign] = &mediadomain.TaskInstancePhoto{ID: foreign, HouseholdID: household.NewHouseholdID(), StorageRef: "households/other/chore-photos/bb/y.jpg", ContentType: "image/jpeg"}
	req = httptest.NewRequest(http.MethodGet, "/tasks/photos/"+foreign.String()+"/raw", nil)
	req.Header.Set("Cookie", cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-household raw with a direct-URL backend: status=%d, want 404", rec.Code)
	}
}
