package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// AlbumID uniquely identifies a photo album.
type AlbumID uuid.UUID

// NewAlbumID returns a new time-ordered (UUIDv7) album id, which gives better
// B-tree index locality than random v4 ids. uuid.NewV7 only errors if the crypto
// random source is unavailable, the same failure under which uuid.New panics, so
// Must is appropriate here.
func NewAlbumID() AlbumID { return AlbumID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id AlbumID) String() string { return uuid.UUID(id).String() }

// ParseAlbumID parses a canonical UUID string into an AlbumID.
func ParseAlbumID(s string) (AlbumID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return AlbumID{}, fmt.Errorf("parse album id: %w", err)
	}
	return AlbumID(u), nil
}

// PhotoID uniquely identifies a photo.
type PhotoID uuid.UUID

// NewPhotoID returns a new time-ordered (UUIDv7) photo id.
func NewPhotoID() PhotoID { return PhotoID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id PhotoID) String() string { return uuid.UUID(id).String() }

// ParsePhotoID parses a canonical UUID string into a PhotoID.
func ParsePhotoID(s string) (PhotoID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return PhotoID{}, fmt.Errorf("parse photo id: %w", err)
	}
	return PhotoID(u), nil
}
