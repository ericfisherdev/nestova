package adapter

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/app"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// choreProofRedirectTarget is where a chore-proof upload sends the client
// after a mutation. There is no task-instance detail page today (see
// ChoreProofWebHandlers' doc) — /tasks is the same target every other chore
// mutation (complete/skip/claim, in tasks/adapter.WebHandlers) already
// redirects to, and is where the member almost certainly came from.
const choreProofRedirectTarget = "/tasks"

// choreProofFileField / choreProofKindField are the multipart form field
// names Upload expects, mirroring the album path's "photo" field name
// (WebHandlers.Upload) and the ticket's before/after kind vocabulary.
const (
	choreProofFileField = "photo"
	choreProofKindField = "kind"
)

// ChoreProofWebHandlers serves the chore-proof photo upload endpoint
// (NES-119): POST /tasks/{id}/photos. It is a separate handler type from
// WebHandlers (SRP/ISP) — the album page (list/grid/raw/album management)
// and this single-purpose upload action share no state — and is registered
// directly against the tasks-owned URL prefix from the composition root
// (cmd/server), the same "neither bounded-context adapter imports the
// other" pattern NES-26's onboarding provisioner established: tasks/adapter
// never imports media, and media/adapter never imports tasks.
//
// There is no task-instance detail page in this codebase today (checked:
// tasks/adapter/web.go only ever redirects to /tasks after a mutation; no
// GET /tasks/{id} route exists) — NES-120 is expected to add the capture UX
// (progress states, before/after review) and, implicitly, wherever it lives.
// This ticket therefore ships only the upload endpoint and its tests, no UI.
type ChoreProofWebHandlers struct {
	photos                *app.ChoreProofPhotoService
	sm                    *scs.SessionManager
	logger                *slog.Logger
	maxUploadRequestBytes int64
}

// NewChoreProofWebHandlers constructs a ChoreProofWebHandlers, panicking on a
// nil dependency or a non-positive maxUploadBytes. maxUploadBytes is the same
// operator-configured per-photo cap the album path uses
// (config.Media.MaxUploadBytes); the outer request-body cap derives from it
// plus requestOverheadBytes, mirroring WebHandlers.NewWebHandlers.
func NewChoreProofWebHandlers(photos *app.ChoreProofPhotoService, sm *scs.SessionManager, logger *slog.Logger, maxUploadBytes int64) *ChoreProofWebHandlers {
	switch {
	case photos == nil:
		panic("media/adapter: NewChoreProofWebHandlers requires a non-nil ChoreProofPhotoService")
	case sm == nil:
		panic("media/adapter: NewChoreProofWebHandlers requires a non-nil session manager")
	case logger == nil:
		panic("media/adapter: NewChoreProofWebHandlers requires a non-nil logger")
	case maxUploadBytes <= 0:
		panic("media/adapter: NewChoreProofWebHandlers requires a positive maxUploadBytes")
	}
	return &ChoreProofWebHandlers{
		photos: photos, sm: sm, logger: logger,
		maxUploadRequestBytes: maxUploadBytes + requestOverheadBytes,
	}
}

// Upload handles POST /tasks/{id}/photos: a multipart chore-proof photo
// upload. Any authenticated household member may upload (mirroring
// tasks/adapter.WebHandlers.Complete/Skip/Claim, which are open to any
// member, not just parents) — the QR/kiosk flow's flexibility depends on
// this: a child scanning their own chore's code and photographing it, or a
// sibling documenting work on another member's behalf, must not be blocked
// by an assignee-only or parent-only check.
func (h *ChoreProofWebHandlers) Upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadRequestBytes)
	if err := r.ParseMultipartForm(uploadMemoryLimit); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "upload exceeds the maximum allowed size", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "malformed upload", http.StatusBadRequest)
		}
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	if !authadapter.VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	instanceID, err := domain.ParseTaskInstanceID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid task instance id", http.StatusBadRequest)
		return
	}
	kind, err := domain.ParsePhotoKind(r.FormValue(choreProofKindField))
	if err != nil {
		http.Error(w, "invalid photo kind: must be \"before\" or \"after\"", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile(choreProofFileField)
	if err != nil {
		http.Error(w, "a photo file is required", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	if _, err := h.photos.Upload(r.Context(), member.HouseholdID, member.ID, instanceID, kind, file, time.Now()); err != nil {
		h.handleUploadError(w, r, err)
		return
	}
	respondAfterMutation(w, r, choreProofRedirectTarget)
}

// handleUploadError maps chore-proof domain errors to HTTP status codes and
// user-facing messages.
func (h *ChoreProofWebHandlers) handleUploadError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrTaskInstanceNotFound), errors.Is(err, household.ErrMemberNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, domain.ErrUnsupportedMediaType):
		http.Error(w, "unsupported photo type — please take a new photo with your camera (JPEG). If your phone saves HEIC photos, switch it to \"Most Compatible\"/JPEG in Camera settings first.", http.StatusUnsupportedMediaType)
	case errors.Is(err, domain.ErrPhotoTooLarge):
		http.Error(w, "photo exceeds the maximum upload size", http.StatusRequestEntityTooLarge)
	case errors.Is(err, domain.ErrPhotoMissingTimestamp):
		http.Error(w, "we couldn't find a camera timestamp on that photo — please take a new photo instead of uploading a saved or screenshotted one", http.StatusUnprocessableEntity)
	case errors.Is(err, domain.ErrPhotoStale):
		http.Error(w, "that photo's timestamp is too old — please take a fresh photo now", http.StatusUnprocessableEntity)
	case errors.Is(err, domain.ErrAfterPrecedesBefore):
		http.Error(w, "the after photo's timestamp is earlier than the before photo — please take a new after photo once the chore is done", http.StatusUnprocessableEntity)
	case errors.Is(err, domain.ErrInvalidTaskInstancePhoto), errors.Is(err, domain.ErrInvalidPhoto):
		http.Error(w, "invalid request", http.StatusBadRequest)
	default:
		h.logger.ErrorContext(r.Context(), "chore proof photo: upload failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
