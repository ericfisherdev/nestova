package domain_test

import (
	"testing"

	"github.com/ericfisherdev/nestova/internal/media/domain"
)

func TestAlbumIDRoundTrip(t *testing.T) {
	id := domain.NewAlbumID()
	got, err := domain.ParseAlbumID(id.String())
	if err != nil {
		t.Fatalf("ParseAlbumID: %v", err)
	}
	if got != id {
		t.Fatalf("round-trip = %s, want %s", got, id)
	}
	if _, err := domain.ParseAlbumID("not-a-uuid"); err == nil {
		t.Fatal("ParseAlbumID(invalid) = nil error, want error")
	}
}

func TestPhotoIDRoundTrip(t *testing.T) {
	id := domain.NewPhotoID()
	got, err := domain.ParsePhotoID(id.String())
	if err != nil {
		t.Fatalf("ParsePhotoID: %v", err)
	}
	if got != id {
		t.Fatalf("round-trip = %s, want %s", got, id)
	}
	if _, err := domain.ParsePhotoID("nope"); err == nil {
		t.Fatal("ParsePhotoID(invalid) = nil error, want error")
	}
}
