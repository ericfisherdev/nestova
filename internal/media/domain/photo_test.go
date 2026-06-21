package domain_test

import (
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

func TestPhotoValidate(t *testing.T) {
	ok := domain.Photo{
		ID:          domain.NewPhotoID(),
		HouseholdID: household.NewHouseholdID(),
		StorageRef:  domain.StorageRef("hh/ab/abc123.jpg"),
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid photo rejected: %v", err)
	}
	for _, ref := range []domain.StorageRef{"", "   "} {
		p := ok
		p.StorageRef = ref
		if !errors.Is(p.Validate(), domain.ErrInvalidPhoto) {
			t.Fatalf("blank storage ref %q accepted", ref)
		}
	}
}
