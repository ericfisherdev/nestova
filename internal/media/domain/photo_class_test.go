package domain_test

import (
	"testing"

	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// TestPhotoClassZeroValueInvalid guards the deliberate design that an
// uninitialized PhotoClass is rejected rather than defaulting to the album
// namespace.
func TestPhotoClassZeroValueInvalid(t *testing.T) {
	var zero domain.PhotoClass
	if zero.Valid() {
		t.Error("zero-value PhotoClass.Valid() = true, want false")
	}
	if zero != domain.PhotoClassUnspecified {
		t.Errorf("zero value = %v, want PhotoClassUnspecified", zero)
	}
}

// TestPhotoClassValid covers the known classes and an out-of-range value.
func TestPhotoClassValid(t *testing.T) {
	cases := []struct {
		name  string
		class domain.PhotoClass
		want  bool
	}{
		{"album", domain.PhotoClassAlbum, true},
		{"chore proof", domain.PhotoClassChoreProof, true},
		{"reward image", domain.PhotoClassRewardImage, true},
		{"unspecified", domain.PhotoClassUnspecified, false},
		{"out of range", domain.PhotoClass(99), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.class.Valid(); got != tc.want {
				t.Errorf("PhotoClass(%v).Valid() = %v, want %v", tc.class, got, tc.want)
			}
		})
	}
}
