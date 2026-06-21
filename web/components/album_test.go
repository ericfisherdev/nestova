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
