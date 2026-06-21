package domain

import (
	"context"
	"io"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// PhotoStore persists and serves photo bytes behind a swappable port (a local
// filesystem adapter first; an object store later). Put validates and stores an
// upload, returning the StorageRef recorded on the Photo; Open streams the bytes
// back for serving; Delete removes them.
//
// Put error contract: ErrUnsupportedMediaType when contentType is not an accepted
// image format, ErrPhotoTooLarge when data exceeds the configured limit, and
// ErrInvalidPhoto when the bytes are not a decodable image. Open returns
// ErrPhotoNotFound when ref is unknown.
type PhotoStore interface {
	Put(ctx context.Context, householdID household.HouseholdID, data []byte, contentType string) (StorageRef, error)
	Open(ctx context.Context, ref StorageRef) (io.ReadCloser, error)
	Delete(ctx context.Context, ref StorageRef) error
}

// ExifReader extracts the EXIF capture time from image bytes. TakenAt returns the
// capture time normalized to UTC, or nil when the image carries no usable EXIF
// date (a missing tag is not an error — the photo is simply stored without a
// taken_at).
type ExifReader interface {
	TakenAt(data []byte) *time.Time
}
