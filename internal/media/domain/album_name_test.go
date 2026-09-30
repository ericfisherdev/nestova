package domain_test

import (
	"errors"
	"strings"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

func TestAlbumValidate_NameLength(t *testing.T) {
	t.Parallel()

	rotation, err := domain.NewRotationInterval(10)
	if err != nil {
		t.Fatalf("NewRotationInterval: %v", err)
	}
	tests := []struct {
		name string
		want error
	}{
		{name: strings.Repeat("a", domain.MaxAlbumNameLength), want: nil},
		{name: strings.Repeat("家", domain.MaxAlbumNameLength), want: nil},
		{name: strings.Repeat("a", domain.MaxAlbumNameLength+1), want: domain.ErrAlbumNameTooLong},
		{name: strings.Repeat("家", domain.MaxAlbumNameLength+1), want: domain.ErrAlbumNameTooLong},
		{name: strings.Repeat("a", 10_000), want: domain.ErrAlbumNameTooLong},
	}
	for _, tc := range tests {
		album := domain.Album{ID: domain.NewAlbumID(), HouseholdID: household.NewHouseholdID(), Name: tc.name, Rotation: rotation}
		err := album.Validate()
		if !errors.Is(err, tc.want) {
			t.Errorf("Validate(%d-rune name) = %v, want %v", len([]rune(tc.name)), err, tc.want)
		}
		if tc.want != nil && !errors.Is(err, domain.ErrInvalidAlbum) {
			t.Errorf("Validate(%d-rune name) = %v, want it to wrap ErrInvalidAlbum", len([]rune(tc.name)), err)
		}
	}
}
