package adapter

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/app"
	"github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/web/components"
)

// photosPath is the canonical page path; mutations redirect back here.
const photosPath = "/photos"

// dateLayout is the YYYY-MM-DD layout of the album filter date inputs.
const dateLayout = "2006-01-02"

// displayDateLayout is the human-readable date layout shown in the UI.
const displayDateLayout = "Jan 2, 2006"

// uploadMemoryLimit bounds the in-memory portion of a multipart parse; larger
// parts spill to temp files. The PhotoStore enforces the real per-upload cap.
const uploadMemoryLimit = 32 << 20

// LayoutFunc wraps page content in the app shell; home.go provides it.
type LayoutFunc func(member *household.Member) func(templ.Component) templ.Component

// WebHandlers serves the /photos UI: album management and photo upload, plus the
// tenant-checked raw-bytes endpoint the viewer and thumbnails load images from.
type WebHandlers struct {
	albums     *app.AlbumService
	photos     *app.PhotoService
	households household.HouseholdRepository
	sm         *scs.SessionManager
	logger     *slog.Logger
}

// NewWebHandlers constructs a WebHandlers, panicking on a nil dependency.
func NewWebHandlers(albums *app.AlbumService, photos *app.PhotoService, households household.HouseholdRepository, sm *scs.SessionManager, logger *slog.Logger) *WebHandlers {
	switch {
	case albums == nil:
		panic("media/adapter: NewWebHandlers requires a non-nil AlbumService")
	case photos == nil:
		panic("media/adapter: NewWebHandlers requires a non-nil PhotoService")
	case households == nil:
		panic("media/adapter: NewWebHandlers requires a non-nil HouseholdRepository")
	case sm == nil:
		panic("media/adapter: NewWebHandlers requires a non-nil session manager")
	case logger == nil:
		panic("media/adapter: NewWebHandlers requires a non-nil logger")
	}
	return &WebHandlers{albums: albums, photos: photos, households: households, sm: sm, logger: logger}
}

// Page handles GET /photos: the album list, the photo grid, and the upload form.
func (h *WebHandlers) Page(layoutFn LayoutFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := authadapter.CurrentMember(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		view, err := h.buildView(r, member)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "photos: build view", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if err := render.Page(r.Context(), w, r, layoutFn(member), components.PhotosPage(view)); err != nil {
			h.logger.ErrorContext(r.Context(), "photos: render page", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}
}

// Upload handles POST /photos: a multipart photo upload.
func (h *WebHandlers) Upload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(uploadMemoryLimit); err != nil {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	if !authadapter.VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	file, headerInfo, err := r.FormFile("photo")
	if err != nil {
		http.Error(w, "a photo file is required", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "could not read the upload", http.StatusBadRequest)
		return
	}
	contentType := headerInfo.Header.Get("Content-Type")
	if _, err := h.photos.Upload(r.Context(), member.HouseholdID, member.ID, data, contentType, r.FormValue("caption")); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, photosPath)
}

// DeletePhoto handles POST /photos/{id}/delete.
func (h *WebHandlers) DeletePhoto(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	id, err := domain.ParsePhotoID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid photo id", http.StatusBadRequest)
		return
	}
	if err := h.photos.Delete(r.Context(), member.HouseholdID, id); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, photosPath)
}

// CreateAlbum handles POST /albums.
func (h *WebHandlers) CreateAlbum(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	in, err := parseAlbumInput(r)
	if err != nil {
		http.Error(w, "invalid album: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := h.albums.Create(r.Context(), member.HouseholdID, in); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, photosPath)
}

// ConfigureAlbum handles POST /albums/{id}.
func (h *WebHandlers) ConfigureAlbum(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseAlbumID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid album id", http.StatusBadRequest)
		return
	}
	in, err := parseAlbumInput(r)
	if err != nil {
		http.Error(w, "invalid album: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.albums.Configure(r.Context(), member.HouseholdID, id, in); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, photosPath)
}

// AddPhoto handles POST /photos/{id}/add-to-album (form: album_id), so the album
// can be chosen from a select without a dynamic form action.
func (h *WebHandlers) AddPhoto(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	photoID, err := domain.ParsePhotoID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid photo id", http.StatusBadRequest)
		return
	}
	albumID, err := domain.ParseAlbumID(strings.TrimSpace(r.FormValue("album_id")))
	if err != nil {
		http.Error(w, "invalid album id", http.StatusBadRequest)
		return
	}
	if err := h.albums.AddPhoto(r.Context(), member.HouseholdID, albumID, photoID); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, photosPath)
}

// RemovePhoto handles POST /albums/{id}/photos/{photoID}/remove.
func (h *WebHandlers) RemovePhoto(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	albumID, err := domain.ParseAlbumID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid album id", http.StatusBadRequest)
		return
	}
	photoID, err := domain.ParsePhotoID(r.PathValue("photoID"))
	if err != nil {
		http.Error(w, "invalid photo id", http.StatusBadRequest)
		return
	}
	if err := h.albums.RemovePhoto(r.Context(), member.HouseholdID, albumID, photoID); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, photosPath)
}

// MovePhoto handles POST /albums/{id}/photos/{photoID}/move (form: direction =
// up|down), shifting a photo one slot within its album.
func (h *WebHandlers) MovePhoto(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	albumID, err := domain.ParseAlbumID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid album id", http.StatusBadRequest)
		return
	}
	photoID, err := domain.ParsePhotoID(r.PathValue("photoID"))
	if err != nil {
		http.Error(w, "invalid photo id", http.StatusBadRequest)
		return
	}
	up := r.FormValue("direction") != "down"
	if err := h.albums.MovePhoto(r.Context(), member.HouseholdID, albumID, photoID, up); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, photosPath)
}

// Raw handles GET /photos/{id}/raw: streams a photo's bytes to its owning
// household only. It is not CSRF-gated (a safe GET) but is tenant-checked.
func (h *WebHandlers) Raw(w http.ResponseWriter, r *http.Request) {
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := domain.ParsePhotoID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid photo id", http.StatusBadRequest)
		return
	}
	rc, contentType, err := h.photos.OpenBytes(r.Context(), member.HouseholdID, id)
	if err != nil {
		if errors.Is(err, domain.ErrPhotoNotFound) {
			http.NotFound(w, r)
			return
		}
		h.logger.ErrorContext(r.Context(), "photos: open bytes", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()
	// Serve with an explicit image content type and forbid MIME sniffing so a
	// crafted upload cannot be reinterpreted as executable content by the browser.
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Private: a photo is household-scoped, so a shared/proxy cache must not store it.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if _, err := io.Copy(w, rc); err != nil {
		h.logger.ErrorContext(r.Context(), "photos: stream bytes", "error", err)
	}
}

func (h *WebHandlers) buildView(r *http.Request, member *household.Member) (components.PhotosView, error) {
	albums, err := h.albums.List(r.Context(), member.HouseholdID)
	if err != nil {
		return components.PhotosView{}, fmt.Errorf("list albums: %w", err)
	}
	photos, err := h.photos.List(r.Context(), member.HouseholdID)
	if err != nil {
		return components.PhotosView{}, fmt.Errorf("list photos: %w", err)
	}
	members, err := h.households.ListMembers(r.Context(), member.HouseholdID)
	if err != nil {
		return components.PhotosView{}, fmt.Errorf("list members: %w", err)
	}

	colorByID := make(map[household.MemberID]string, len(members))
	memberOptions := make([]components.MediaMemberOption, 0, len(members))
	for _, m := range members {
		colorByID[m.ID] = m.Color.String()
		memberOptions = append(memberOptions, components.MediaMemberOption{ID: m.ID.String(), Name: m.DisplayName, Color: m.Color.String()})
	}

	photoViews := make([]components.PhotoView, 0, len(photos))
	for _, p := range photos {
		pv := components.PhotoView{
			ID:      p.ID.String(),
			RawURL:  photosPath + "/" + p.ID.String() + "/raw",
			Caption: p.Caption,
		}
		if p.TakenAt != nil {
			pv.TakenOn = p.TakenAt.Format(displayDateLayout)
		}
		if p.UploadedBy != nil {
			pv.UploaderColor = colorByID[*p.UploadedBy]
		}
		photoViews = append(photoViews, pv)
	}

	albumViews := make([]components.AlbumView, 0, len(albums))
	for _, a := range albums {
		members, err := h.albums.AlbumPhotos(r.Context(), member.HouseholdID, a.ID)
		if err != nil {
			return components.PhotosView{}, fmt.Errorf("album photos: %w", err)
		}
		memberViews := make([]components.AlbumPhotoView, 0, len(members))
		for _, p := range members {
			memberViews = append(memberViews, components.AlbumPhotoView{
				PhotoID: p.ID.String(),
				RawURL:  photosPath + "/" + p.ID.String() + "/raw",
				Caption: p.Caption,
			})
		}
		albumViews = append(albumViews, components.AlbumView{
			ID:              a.ID.String(),
			Name:            a.Name,
			RotationSeconds: a.Rotation.Seconds(),
			ViewURL:         "/album/" + a.ID.String(),
			Photos:          memberViews,
		})
	}

	return components.PhotosView{
		Albums:    albumViews,
		Photos:    photoViews,
		Members:   memberOptions,
		CSRFToken: authadapter.GetCSRFToken(r.Context(), h.sm),
	}, nil
}

// parseAlbumInput parses the album form into a service input. The filter narrows
// by uploader member ids (member_ids) and an optional taken_at range (since/until).
func parseAlbumInput(r *http.Request) (app.AlbumInput, error) {
	rotation, err := domain.NewRotationInterval(atoiDefault(r.FormValue("rotation_seconds"), 0))
	if err != nil {
		return app.AlbumInput{}, err
	}
	var filter domain.AlbumFilter
	for _, raw := range r.Form["member_ids"] {
		id, err := household.ParseMemberID(strings.TrimSpace(raw))
		if err != nil {
			return app.AlbumInput{}, fmt.Errorf("invalid member filter")
		}
		filter.MemberIDs = append(filter.MemberIDs, id)
	}
	if filter.Since, err = parseOptionalDate(r.FormValue("since")); err != nil {
		return app.AlbumInput{}, err
	}
	if filter.Until, err = parseOptionalDate(r.FormValue("until")); err != nil {
		return app.AlbumInput{}, err
	}
	return app.AlbumInput{Name: strings.TrimSpace(r.FormValue("name")), Rotation: rotation, Filter: filter}, nil
}

func parseOptionalDate(v string) (*time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(dateLayout, v)
	if err != nil {
		return nil, fmt.Errorf("invalid date %q", v)
	}
	return &t, nil
}

func atoiDefault(s string, fallback int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	n := fallback
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return fallback
	}
	return n
}

func (h *WebHandlers) beginMutation(w http.ResponseWriter, r *http.Request) (*household.Member, bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, false
	}
	if !authadapter.VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, false
	}
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	return member, true
}

// respondAfterMutation refreshes the page: HX-Redirect for HTMX, a 303 otherwise.
func respondAfterMutation(w http.ResponseWriter, r *http.Request, target string) {
	if render.IsHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleMutationError maps domain errors to HTTP status codes.
func (h *WebHandlers) handleMutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrAlbumNotFound), errors.Is(err, domain.ErrPhotoNotFound),
		errors.Is(err, household.ErrHouseholdNotFound), errors.Is(err, household.ErrMemberNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, domain.ErrUnsupportedMediaType):
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
	case errors.Is(err, domain.ErrPhotoTooLarge):
		http.Error(w, "photo too large", http.StatusRequestEntityTooLarge)
	case errors.Is(err, domain.ErrInvalidAlbum), errors.Is(err, domain.ErrInvalidPhoto):
		http.Error(w, "invalid request", http.StatusBadRequest)
	default:
		h.logger.ErrorContext(r.Context(), "photos: mutation failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
