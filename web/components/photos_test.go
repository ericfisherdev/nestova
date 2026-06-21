package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

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
		Members:   []components.MediaMemberOption{{ID: "mem-1", Name: "Alex", Color: "clay"}},
		CSRFToken: "tok-xyz",
	}
}

func TestPhotosPageRendersUploadAlbumsAndGrid(t *testing.T) {
	out := renderString(t, components.PhotosPage(photosView()))

	// Multipart upload form posting to /photos with CSRF.
	if !strings.Contains(out, `hx-post="/photos"`) || !strings.Contains(out, `multipart/form-data`) {
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
	// The upload form is always present.
	if !strings.Contains(out, `hx-post="/photos"`) {
		t.Errorf("empty state missing upload form")
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
