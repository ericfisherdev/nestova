package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

// testMaxUploadBytes mirrors the .env.example default (25 MiB) so assertions
// on the rendered MB label and byte count stay in sync with a realistic value.
const testMaxUploadBytes = 26214400

func photosView() components.PhotosView {
	return components.PhotosView{
		Albums: []components.AlbumView{{
			ID: "alb-1", Name: "Family", RotationSeconds: 8, ViewURL: "/album/alb-1",
			Photos: []components.AlbumPhotoView{
				{PhotoID: "ph-1", RawURL: "/photos/ph-1/raw", Caption: "one"},
				{PhotoID: "ph-2", RawURL: "/photos/ph-2/raw", Caption: "two"},
			},
		}},
		Photos: []components.PhotoView{
			{ID: "ph-1", RawURL: "/photos/ph-1/raw", Caption: "Beach", TakenOn: "Jul 4, 2026", UploaderColor: "clay"},
			{ID: "ph-2", RawURL: "/photos/ph-2/raw"},
		},
		Members:        []components.MediaMemberOption{{ID: "mem-1", Name: "Alex", Color: "clay"}},
		CSRFToken:      "tok-xyz",
		MaxUploadBytes: testMaxUploadBytes,
	}
}

func TestPhotosPageRendersUploadAlbumsAndGrid(t *testing.T) {
	out := renderString(t, components.PhotosPage(photosView()))

	// The multipart upload form carries CSRF and the multi-file input the
	// upload queue (NES-124) reads from.
	if !strings.Contains(out, `enctype="multipart/form-data"`) {
		t.Errorf("missing multipart upload form: %q", out)
	}
	if !strings.Contains(out, `value="tok-xyz"`) {
		t.Errorf("missing csrf token: %q", out)
	}
	// Create-album form.
	if !strings.Contains(out, `hx-post="/albums"`) {
		t.Errorf("missing create-album form")
	}
	// Album row with its view link, move, and remove actions.
	if !strings.Contains(out, "/album/alb-1") {
		t.Errorf("missing album view link")
	}
	if !strings.Contains(out, `hx-post="/albums/alb-1/photos/ph-1/move"`) ||
		!strings.Contains(out, `hx-post="/albums/alb-1/photos/ph-1/remove"`) {
		t.Errorf("missing album photo move/remove actions: %q", out)
	}
	// Photo grid: image src, delete, add-to-album, uploader color.
	if !strings.Contains(out, `src="/photos/ph-1/raw"`) {
		t.Errorf("missing photo image")
	}
	if !strings.Contains(out, `hx-post="/photos/ph-1/delete"`) ||
		!strings.Contains(out, `hx-post="/photos/ph-1/add-to-album"`) {
		t.Errorf("missing photo delete/add actions: %q", out)
	}
	if !strings.Contains(out, "bg-member-clay-solid") {
		t.Errorf("missing uploader color chip")
	}
}

// ---------------------------------------------------------------------------
// uploadDropzone (NES-124)
// ---------------------------------------------------------------------------

// TestUploadDropzoneMarkup covers the pieces web/static/js/upload-queue.js
// depends on existing exactly as it expects: the data attributes it reads its
// config from, the multi-file input, and the drag event wiring.
func TestUploadDropzoneMarkup(t *testing.T) {
	out := renderString(t, components.PhotosPage(photosView()))

	if !strings.Contains(out, `id="upload-dropzone"`) {
		t.Errorf("missing #upload-dropzone container: %q", out)
	}
	if !strings.Contains(out, `data-upload-url="/photos"`) {
		t.Errorf("missing data-upload-url pointing at the unchanged single-file endpoint: %q", out)
	}
	if !strings.Contains(out, `data-max-bytes="26214400"`) {
		t.Errorf("missing data-max-bytes carrying MaxUploadBytes: %q", out)
	}
	if !strings.Contains(out, `data-accept="image/jpeg,image/png,image/webp"`) {
		t.Errorf("missing data-accept for the client-side type pre-check: %q", out)
	}
	if !strings.Contains(out, `x-data="uploadQueue()"`) {
		t.Errorf("missing uploadQueue() Alpine component registration: %q", out)
	}
	if !strings.Contains(out, `@dragover.prevent`) || !strings.Contains(out, `@dragleave.prevent`) || !strings.Contains(out, `@drop.prevent`) {
		t.Errorf("missing drag event handlers: %q", out)
	}
	// The click-to-browse / mobile fallback (AC5): a real multi-file input,
	// wired to enqueue on change rather than a native form submit.
	if !strings.Contains(out, `id="photo-file"`) || !strings.Contains(out, "multiple") {
		t.Errorf("missing multi-file input: %q", out)
	}
	if !strings.Contains(out, `@change="enqueueFiles($event.target.files)`) {
		t.Errorf("file input must enqueue on change: %q", out)
	}
	// Retry control and per-file progress/status markup for the queue list.
	if !strings.Contains(out, `@click="retry(item.id)"`) {
		t.Errorf("missing retry button wiring: %q", out)
	}
	if !strings.Contains(out, `x-text="item.progress"`) && !strings.Contains(out, `:value="item.progress"`) {
		t.Errorf("missing per-file progress binding: %q", out)
	}
	if !strings.Contains(out, `data-testid="upload-summary"`) {
		t.Errorf("missing batch summary line element: %q", out)
	}
}

// TestUploadDropzoneFileInputHasVisibleFocusIndicator covers the reviewed
// a11y gap: the file input is sr-only (visually hidden but still in the tab
// order), so a sighted keyboard user tabbing to it would see no focus
// indicator at all unless something else shows one. The fix is real HTML
// semantics — the input nested inside its own <label>, not an ARIA role
// hack — with a focus-within ring on the label so focusing the input (by
// Tab, not just by click) visibly highlights "choose files".
func TestUploadDropzoneFileInputHasVisibleFocusIndicator(t *testing.T) {
	out := renderString(t, components.PhotosPage(photosView()))

	labelStart := strings.Index(out, "focus-within:ring-2")
	if labelStart == -1 {
		t.Fatalf("missing focus-within visible-focus ring on the file-input label: %q", out)
	}
	labelEnd := strings.Index(out[labelStart:], "</label>")
	if labelEnd == -1 {
		t.Fatalf("focus-within label never closes: %q", out)
	}
	between := out[labelStart : labelStart+labelEnd]
	if !strings.Contains(between, `id="photo-file"`) {
		t.Errorf("file input must be nested inside the focus-within label (real semantics, not role hacks), so :focus-within actually tracks the input's own focus state: %q", between)
	}
}

func TestUploadDropzoneMaxBytesLabel(t *testing.T) {
	view := photosView()
	view.MaxUploadBytes = 10 << 20 // 10 MiB, distinct from the package default
	out := renderString(t, components.PhotosPage(view))

	if !strings.Contains(out, `data-max-bytes="10485760"`) {
		t.Errorf("data-max-bytes did not track MaxUploadBytes: %q", out)
	}
	if !strings.Contains(out, "up to 10 MB each") {
		t.Errorf("human-readable size label did not track MaxUploadBytes: %q", out)
	}
}

func TestPhotosPageEmptyStates(t *testing.T) {
	view := photosView()
	view.Albums = nil
	view.Photos = nil
	out := renderString(t, components.PhotosPage(view))
	if !strings.Contains(out, "No albums yet") {
		t.Errorf("missing empty album state")
	}
	if !strings.Contains(out, "No photos yet") {
		t.Errorf("missing empty photo state")
	}
	// The upload dropzone is always present.
	if !strings.Contains(out, `id="upload-dropzone"`) {
		t.Errorf("empty state missing upload dropzone")
	}
}

func TestPhotosPageAlbumConfigureAndMoveControls(t *testing.T) {
	out := renderString(t, components.PhotosPage(photosView()))
	// Each album exposes a configure form posting to /albums/{id}.
	if !strings.Contains(out, `hx-post="/albums/alb-1"`) {
		t.Errorf("missing album configure form: %q", out)
	}
	// Configure pre-fills the album's current name (not reset to a default).
	if !strings.Contains(out, `value="Family"`) {
		t.Errorf("configure form did not pre-fill the album name: %q", out)
	}
	// Move controls render a disabled button at the ends (the first photo cannot
	// move up), so reordering never runs off the boundary.
	if !strings.Contains(out, "disabled") {
		t.Errorf("expected a disabled move button at an album boundary: %q", out)
	}
	// Icon-only and unlabeled controls carry accessible names.
	if !strings.Contains(out, `aria-label="Move up"`) || !strings.Contains(out, `aria-label="Add to album"`) {
		t.Errorf("missing accessible names on move/add controls: %q", out)
	}
}

// ---------------------------------------------------------------------------
// PhotoGridFragment (NES-124)
// ---------------------------------------------------------------------------

// TestPhotoGridFragment_RendersContainerAndPhotos verifies that
// PhotoGridFragment wraps its photo cards in the stable id="photo-grid"
// container the upload queue's post-drain refresh targets, and that the
// hx-trigger wiring points at GET /photos/grid.
func TestPhotoGridFragment_RendersContainerAndPhotos(t *testing.T) {
	view := components.PhotoGridView{
		Photos: []components.PhotoView{
			{ID: "ph-1", RawURL: "/photos/ph-1/raw", Caption: "Beach"},
		},
		Albums:    []components.AlbumOption{{ID: "alb-1", Name: "Family"}},
		CSRFToken: "tok-xyz",
	}
	out := renderString(t, components.PhotoGridFragment(view))

	if !strings.Contains(out, `id="photo-grid"`) {
		t.Errorf("fragment missing the stable #photo-grid container id: %q", out)
	}
	if !strings.Contains(out, `hx-trigger="photos-uploaded"`) {
		t.Errorf("fragment missing the post-drain refresh trigger: %q", out)
	}
	if !strings.Contains(out, `hx-get="/photos/grid"`) || !strings.Contains(out, `hx-target="this"`) || !strings.Contains(out, `hx-swap="outerHTML"`) {
		t.Errorf("fragment missing hx-get/hx-target/hx-swap wiring: %q", out)
	}
	if !strings.Contains(out, `src="/photos/ph-1/raw"`) {
		t.Errorf("fragment missing photo image: %q", out)
	}
	if !strings.Contains(out, "Family") {
		t.Errorf("fragment missing add-to-album option built from Albums: %q", out)
	}
}

// TestPhotoGridFragment_Empty verifies that PhotoGridFragment renders the
// "no photos yet" empty state, still wrapped in id="photo-grid", the same
// shape GET /photos/grid returns after every photo has been deleted.
func TestPhotoGridFragment_Empty(t *testing.T) {
	out := renderString(t, components.PhotoGridFragment(components.PhotoGridView{}))

	if !strings.Contains(out, `id="photo-grid"`) {
		t.Errorf("empty fragment missing the #photo-grid container: %q", out)
	}
	if !strings.Contains(out, "No photos yet") {
		t.Errorf("empty fragment missing empty-state message: %q", out)
	}
}
