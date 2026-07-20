package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

func TestAlbumViewerPageRendersSlidesAndScripts(t *testing.T) {
	view := components.AlbumViewerView{
		AlbumName:       "Family",
		RotationSeconds: 6,
		Slides: []components.SlideView{
			{RawURL: "/photos/ph-1/raw", Caption: "Beach", UploaderColor: "clay"},
			{RawURL: "/photos/ph-2/raw", Caption: ""},
		},
	}
	out := renderString(t, components.AlbumViewerPage(view))

	// Standalone page that loads GSAP, Alpine, and the viewer glue.
	for _, want := range []string{"/static/js/gsap.min.js", "/static/js/alpine.min.js", "/static/js/album.js", "/static/css/app.css"} {
		if !strings.Contains(out, want) {
			t.Errorf("viewer page missing %q", want)
		}
	}
	// album.js registers Alpine.data('albumViewer') via an 'alpine:init'
	// listener, so it must appear BEFORE alpine.min.js — loaded after,
	// the listener registers after the event already fired and the
	// component silently never exists (NES-147).
	// src-attribute matches, not bare paths — the head comment mentions
	// the script names too.
	albumIdx := strings.Index(out, `src="/static/js/album.js"`)
	alpineIdx := strings.Index(out, `src="/static/js/alpine.min.js"`)
	if albumIdx == -1 || alpineIdx == -1 {
		t.Fatalf("missing album.js or alpine.min.js script tag in viewer page: %q", out)
	}
	if albumIdx > alpineIdx {
		t.Errorf("album.js must load before alpine.min.js, got order: %q", out)
	}
	// The Alpine component + rotation cadence.
	if !strings.Contains(out, `x-data="albumViewer"`) || !strings.Contains(out, `data-rotation-seconds="6"`) {
		t.Errorf("missing viewer hooks: %q", out)
	}
	// Slides carry their image and caption/colour data for the JS.
	if !strings.Contains(out, `src="/photos/ph-1/raw"`) || !strings.Contains(out, `data-caption="Beach"`) || !strings.Contains(out, `data-color="clay"`) {
		t.Errorf("missing slide data: %q", out)
	}
}

func TestAlbumViewerPageEmptyState(t *testing.T) {
	out := renderString(t, components.AlbumViewerPage(components.AlbumViewerView{AlbumName: "Empty", RotationSeconds: 8}))
	if !strings.Contains(out, "No photos yet") || !strings.Contains(out, `data-testid="album-empty"`) {
		t.Errorf("missing empty viewer state: %q", out)
	}
}

func TestAlbumViewerSlideHasImageDimensionsClass(t *testing.T) {
	view := components.AlbumViewerView{
		AlbumName:       "X",
		RotationSeconds: 5,
		Slides:          []components.SlideView{{RawURL: "/photos/p/raw", Caption: "c"}},
	}
	out := renderString(t, components.AlbumViewerPage(view))
	// Each slide image fills the frame (object-cover) for the entryway display.
	if !strings.Contains(out, "object-cover") {
		t.Errorf("slide image missing object-cover: %q", out)
	}
}
