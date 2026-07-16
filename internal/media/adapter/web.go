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

// uploadMemoryLimit is the memory budget ParseMultipartForm is allowed for a
// request's non-file parts (csrf_token, caption) plus any file part smaller
// than this. It is kept deliberately small: any part larger than it spills
// straight to a temp file (mime/multipart's own streaming write), so a real
// photo is written to disk as it arrives rather than held as one big in-memory
// buffer — memory use during an upload stays flat regardless of file size.
const uploadMemoryLimit = 256 << 10

// requestOverheadBytes is the slack added on top of the configured per-photo
// limit (maxUploadBytes, injected via NewWebHandlers) to build the outer
// request-body cap: multipart boundaries/headers plus the small csrf_token
// and caption fields. Generous enough that a legitimate request is never
// rejected by this outer cap before reaching the PhotoStore's own, more
// specific size-limit error.
const requestOverheadBytes = 64 << 10

// uploadResultHeader reports whether Upload created a new photo or matched an
// existing one by content hash. It is informational only — the mutation
// success signal (HX-Redirect/303 via respondAfterMutation) is the same
// either way — read by the client-side upload queue's per-file XHR (NES-124,
// web/static/js/upload-queue.js) to distinguish a dedup no-op from a real
// create without parsing the redirected page.
const uploadResultHeader = "X-Upload-Result"

// uploadResultHeader values.
const (
	uploadResultCreated   = "created"
	uploadResultDuplicate = "duplicate"
)

// LayoutFunc wraps page content in the app shell; home.go provides it.
type LayoutFunc func(member *household.Member) func(templ.Component) templ.Component

// WebHandlers serves the /photos UI: album management and photo upload, plus the
// tenant-checked raw-bytes endpoint the viewer and thumbnails load images from.
type WebHandlers struct {
	albums                *app.AlbumService
	photos                *app.PhotoService
	households            household.HouseholdRepository
	sm                    *scs.SessionManager
	logger                *slog.Logger
	maxUploadBytes        int64
	maxUploadRequestBytes int64
}

// NewWebHandlers constructs a WebHandlers, panicking on a nil dependency or a
// non-positive maxUploadBytes. maxUploadBytes is the operator-configured
// per-photo cap (config.Media.MaxUploadBytes); the outer request-body cap
// derives from it plus requestOverheadBytes, so raising the configured limit
// never leaves it unreachable behind a smaller, hardcoded outer cap.
func NewWebHandlers(albums *app.AlbumService, photos *app.PhotoService, households household.HouseholdRepository, sm *scs.SessionManager, logger *slog.Logger, maxUploadBytes int64) *WebHandlers {
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
	case maxUploadBytes <= 0:
		panic("media/adapter: NewWebHandlers requires a positive maxUploadBytes")
	}
	return &WebHandlers{
		albums: albums, photos: photos, households: households, sm: sm, logger: logger,
		maxUploadBytes:        maxUploadBytes,
		maxUploadRequestBytes: maxUploadBytes + requestOverheadBytes,
	}
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

// Grid handles GET /photos/grid. It rebuilds the photo list and renders just
// the #photo-grid fragment (NES-124): the client-side upload queue
// (web/static/js/upload-queue.js) triggers this exactly once after a whole
// drag-and-drop batch drains, so dropping 50 photos refreshes the grid once —
// not once per file. This is a passive read with no state to change, so it
// is a GET and always succeeds, mirroring tasks/adapter.WebHandlers.Groups.
func (h *WebHandlers) Grid(w http.ResponseWriter, r *http.Request) {
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	view, err := h.buildGridView(r, member)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "photos: build grid view", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := render.Render(r.Context(), w, http.StatusOK, components.PhotoGridFragment(view)); err != nil {
		h.logger.ErrorContext(r.Context(), "photos: render grid", "error", err)
	}
}

// AlbumViewer handles GET /album/{id}: the full-screen rotating slideshow. It is
// a standalone page (not the dashboard shell) built for the entryway display.
func (h *WebHandlers) AlbumViewer(w http.ResponseWriter, r *http.Request) {
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := domain.ParseAlbumID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid album id", http.StatusBadRequest)
		return
	}
	view, err := h.buildViewerView(r, member, id)
	if err != nil {
		if errors.Is(err, domain.ErrAlbumNotFound) {
			http.NotFound(w, r)
			return
		}
		h.logger.ErrorContext(r.Context(), "album viewer: build view", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := render.Render(r.Context(), w, http.StatusOK, components.AlbumViewerPage(view)); err != nil {
		h.logger.ErrorContext(r.Context(), "album viewer: render", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (h *WebHandlers) buildViewerView(r *http.Request, member *household.Member, albumID domain.AlbumID) (components.AlbumViewerView, error) {
	album, err := h.albums.Album(r.Context(), member.HouseholdID, albumID)
	if err != nil {
		return components.AlbumViewerView{}, err
	}
	items, err := h.albums.Playlist(r.Context(), member.HouseholdID, albumID)
	if err != nil {
		return components.AlbumViewerView{}, err
	}
	members, err := h.households.ListMembers(r.Context(), member.HouseholdID)
	if err != nil {
		return components.AlbumViewerView{}, err
	}
	colorByID := make(map[household.MemberID]string, len(members))
	for _, m := range members {
		colorByID[m.ID] = m.Color.String()
	}

	slides := make([]components.SlideView, 0, len(items))
	for _, it := range items {
		slide := components.SlideView{
			RawURL:  photosPath + "/" + it.PhotoID.String() + "/raw",
			Caption: it.Caption,
		}
		if it.UploadedBy != nil {
			slide.UploaderColor = colorByID[*it.UploadedBy]
		}
		slides = append(slides, slide)
	}
	return components.AlbumViewerView{
		AlbumName:       album.Name,
		RotationSeconds: album.Rotation.Seconds(),
		Slides:          slides,
	}, nil
}

// Upload handles POST /photos: a multipart photo upload. The file part is
// streamed straight through to PhotoService/PhotoStore (never buffered whole
// into a []byte here) — the client's declared Content-Type is not even read;
// the store sniffs the true type from the bytes themselves.
func (h *WebHandlers) Upload(w http.ResponseWriter, r *http.Request) {
	// Hard-cap the request body before parsing so a huge upload cannot exhaust
	// memory/disk; the PhotoStore still enforces the real per-photo size limit.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadRequestBytes)
	if err := r.ParseMultipartForm(uploadMemoryLimit); err != nil {
		http.Error(w, "upload too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}
	// ParseMultipartForm may spill large parts to temp files; remove them on exit.
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
	file, _, err := r.FormFile("photo")
	if err != nil {
		http.Error(w, "a photo file is required", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	result, err := h.photos.Upload(r.Context(), member.HouseholdID, member.ID, file, r.FormValue("caption"))
	if err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	if result.Duplicate {
		w.Header().Set(uploadResultHeader, uploadResultDuplicate)
	} else {
		w.Header().Set(uploadResultHeader, uploadResultCreated)
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
	direction := r.FormValue("direction")
	if direction != "up" && direction != "down" {
		http.Error(w, "direction must be up or down", http.StatusBadRequest)
		return
	}
	if err := h.albums.MovePhoto(r.Context(), member.HouseholdID, albumID, photoID, direction == "up"); err != nil {
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

// memberColorByID builds the member-id -> color lookup that a photo's
// uploader-attribution chip (buildPhotoViews) keys off. Shared by buildView
// and buildGridView so the two can never attribute a photo differently.
func memberColorByID(members []*household.Member) map[household.MemberID]string {
	colorByID := make(map[household.MemberID]string, len(members))
	for _, m := range members {
		colorByID[m.ID] = m.Color.String()
	}
	return colorByID
}

// buildPhotoViews projects domain photos into grid rows, attributing each to
// its uploader's color via colorByID. Shared by buildView (the full /photos
// page) and buildGridView (the #photo-grid fragment, NES-124) so the two
// render the exact same photo list/attribution logic rather than a second,
// independently-maintained copy of it.
func buildPhotoViews(photos []*domain.Photo, colorByID map[household.MemberID]string) []components.PhotoView {
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
	return photoViews
}

// buildView loads everything the full /photos page renders: the photo grid,
// the create-album member filter options, and each album's full view
// including its ordered photo membership (h.albums.AlbumPhotos, one query
// per album) for the Albums section's move/remove controls.
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

	memberOptions := make([]components.MediaMemberOption, 0, len(members))
	for _, m := range members {
		memberOptions = append(memberOptions, components.MediaMemberOption{ID: m.ID.String(), Name: m.DisplayName, Color: m.Color.String()})
	}

	albumViews := make([]components.AlbumView, 0, len(albums))
	for _, a := range albums {
		albumMembers, err := h.albums.AlbumPhotos(r.Context(), member.HouseholdID, a.ID)
		if err != nil {
			return components.PhotosView{}, fmt.Errorf("album photos: %w", err)
		}
		memberViews := make([]components.AlbumPhotoView, 0, len(albumMembers))
		for _, p := range albumMembers {
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
		Albums:         albumViews,
		Photos:         buildPhotoViews(photos, memberColorByID(members)),
		Members:        memberOptions,
		CSRFToken:      authadapter.GetCSRFToken(r.Context(), h.sm),
		MaxUploadBytes: h.maxUploadBytes,
	}, nil
}

// buildGridView loads just what PhotoGridFragment renders: the photo grid and
// lightweight album id/name options for photoCard's add-to-album dropdown.
// It deliberately does NOT call h.albums.AlbumPhotos — the grid fragment
// never renders an album's ordered membership (that's the full page's Albums
// section only, in buildView) — so a GET /photos/grid, which the upload
// queue triggers once after every drag-and-drop batch drains, costs a fixed
// three queries (albums, photos, members) regardless of how many albums the
// household has, instead of one extra AlbumPhotos round trip per album.
func (h *WebHandlers) buildGridView(r *http.Request, member *household.Member) (components.PhotoGridView, error) {
	albums, err := h.albums.List(r.Context(), member.HouseholdID)
	if err != nil {
		return components.PhotoGridView{}, fmt.Errorf("list albums: %w", err)
	}
	photos, err := h.photos.List(r.Context(), member.HouseholdID)
	if err != nil {
		return components.PhotoGridView{}, fmt.Errorf("list photos: %w", err)
	}
	members, err := h.households.ListMembers(r.Context(), member.HouseholdID)
	if err != nil {
		return components.PhotoGridView{}, fmt.Errorf("list members: %w", err)
	}

	albumOptions := make([]components.AlbumOption, 0, len(albums))
	for _, a := range albums {
		albumOptions = append(albumOptions, components.AlbumOption{ID: a.ID.String(), Name: a.Name})
	}

	return components.PhotoGridView{
		Photos:    buildPhotoViews(photos, memberColorByID(members)),
		Albums:    albumOptions,
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
		http.Error(w, "unsupported photo type — please upload a JPEG, PNG, or WEBP image", http.StatusUnsupportedMediaType)
	case errors.Is(err, domain.ErrPhotoTooLarge):
		http.Error(w, "photo exceeds the maximum upload size", http.StatusRequestEntityTooLarge)
	case errors.Is(err, domain.ErrInvalidAlbum), errors.Is(err, domain.ErrInvalidPhoto):
		http.Error(w, "invalid request", http.StatusBadRequest)
	default:
		h.logger.ErrorContext(r.Context(), "photos: mutation failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
