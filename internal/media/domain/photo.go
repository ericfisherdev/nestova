package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Photo errors.
var (
	// ErrPhotoNotFound is returned when a photo does not exist (or belongs to
	// another household).
	ErrPhotoNotFound = errors.New("media: photo not found")
	// ErrInvalidPhoto is returned by Photo.Validate for a malformed photo.
	ErrInvalidPhoto = errors.New("media: invalid photo")
	// ErrUnsupportedMediaType is returned when an upload's content type is not an
	// accepted image format. The web layer maps it to 415.
	ErrUnsupportedMediaType = errors.New("media: unsupported media type")
	// ErrPhotoTooLarge is returned when an upload exceeds the configured size
	// limit. The web layer maps it to 413.
	ErrPhotoTooLarge = errors.New("media: photo exceeds the maximum size")
)

// StorageRef is an opaque key identifying a photo's bytes in the PhotoStore. The
// bytes are never stored in the database.
type StorageRef string

// String returns the ref's string value.
func (r StorageRef) String() string { return string(r) }

// Photo is one household image. StorageRef points at the bytes behind the
// PhotoStore; TakenAt is the EXIF capture time (UTC) when the upload carried one;
// UploadedBy is the member who added it, nilled (not deleted) if that member is
// removed so the photo survives.
type Photo struct {
	ID          PhotoID
	HouseholdID household.HouseholdID
	StorageRef  StorageRef
	TakenAt     *time.Time
	Caption     string
	UploadedBy  *household.MemberID
	CreatedAt   time.Time
}

// Validate reports whether the photo is well-formed, wrapping ErrInvalidPhoto.
func (p Photo) Validate() error {
	if strings.TrimSpace(p.StorageRef.String()) == "" {
		return fmt.Errorf("%w: storage ref must not be blank", ErrInvalidPhoto)
	}
	return nil
}

// PhotoRepository persists photo metadata (not the bytes). Get returns
// ErrPhotoNotFound for an unknown id; a Create with an unknown HouseholdID
// returns household.ErrHouseholdNotFound and an unknown UploadedBy returns
// household.ErrMemberNotFound (both mapped from the tenant FK violations by the
// adapter). ListByHousehold returns an empty slice (not an error) when none match.
type PhotoRepository interface {
	Create(ctx context.Context, photo *Photo) error
	Get(ctx context.Context, id PhotoID) (*Photo, error)
	ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*Photo, error)
	Delete(ctx context.Context, id PhotoID) error
}
