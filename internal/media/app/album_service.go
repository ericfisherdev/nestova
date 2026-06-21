package app

import (
	"context"
	"errors"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// AlbumInput is the validated, parsed form data for creating or configuring an
// album. The web layer parses raw form values into this; the service builds and
// validates the domain entity.
type AlbumInput struct {
	Name     string
	Rotation domain.RotationInterval
	Filter   domain.AlbumFilter
}

// PlaylistItem is one slide in an album's rotation: the photo to show, its
// caption and capture time, and the uploader (the view layer maps the uploader
// to a member colour). The bytes are fetched separately via the StorageRef.
type PlaylistItem struct {
	PhotoID    domain.PhotoID
	StorageRef domain.StorageRef
	Caption    string
	TakenAt    *time.Time
	UploadedBy *household.MemberID
}

// AlbumService manages albums and their ordered photo membership, and composes
// the filtered rotation playlist.
type AlbumService struct {
	albums      domain.AlbumRepository
	photos      domain.PhotoRepository
	albumPhotos domain.AlbumPhotoRepository
}

// NewAlbumService constructs the service with injected repositories.
func NewAlbumService(albums domain.AlbumRepository, photos domain.PhotoRepository, albumPhotos domain.AlbumPhotoRepository) (*AlbumService, error) {
	switch {
	case albums == nil:
		return nil, errors.New("media/app: NewAlbumService requires a non-nil AlbumRepository")
	case photos == nil:
		return nil, errors.New("media/app: NewAlbumService requires a non-nil PhotoRepository")
	case albumPhotos == nil:
		return nil, errors.New("media/app: NewAlbumService requires a non-nil AlbumPhotoRepository")
	}
	return &AlbumService{albums: albums, photos: photos, albumPhotos: albumPhotos}, nil
}

// Create makes a new album for the household and returns it.
func (s *AlbumService) Create(ctx context.Context, householdID household.HouseholdID, in AlbumInput) (*domain.Album, error) {
	album := &domain.Album{
		ID:          domain.NewAlbumID(),
		HouseholdID: householdID,
		Name:        in.Name,
		Rotation:    in.Rotation,
		Filter:      in.Filter,
	}
	if err := album.Validate(); err != nil {
		return nil, err
	}
	if err := s.albums.Create(ctx, album); err != nil {
		return nil, err
	}
	return album, nil
}

// Configure rewrites an album's name, rotation, and filter (ownership-checked).
func (s *AlbumService) Configure(ctx context.Context, householdID household.HouseholdID, id domain.AlbumID, in AlbumInput) error {
	album, err := s.ownedAlbum(ctx, householdID, id)
	if err != nil {
		return err
	}
	// Validate a candidate copy before mutating the loaded album, so a rejected
	// Configure never leaves the in-memory album in an invalid state.
	candidate := *album
	candidate.Name = in.Name
	candidate.Rotation = in.Rotation
	candidate.Filter = in.Filter
	if err := candidate.Validate(); err != nil {
		return err
	}
	return s.albums.Update(ctx, &candidate)
}

// AddPhoto appends a household photo to one of its albums (both ownership-checked).
func (s *AlbumService) AddPhoto(ctx context.Context, householdID household.HouseholdID, albumID domain.AlbumID, photoID domain.PhotoID) error {
	if _, err := s.ownedAlbum(ctx, householdID, albumID); err != nil {
		return err
	}
	if _, err := s.ownedPhoto(ctx, householdID, photoID); err != nil {
		return err
	}
	return s.albumPhotos.Add(ctx, albumID, photoID)
}

// RemovePhoto drops a photo from one of the household's albums. Both the album
// and the photo are ownership-checked so a cross-household id is normalized to a
// not-found error rather than silently no-opping.
func (s *AlbumService) RemovePhoto(ctx context.Context, householdID household.HouseholdID, albumID domain.AlbumID, photoID domain.PhotoID) error {
	if _, err := s.ownedAlbum(ctx, householdID, albumID); err != nil {
		return err
	}
	if _, err := s.ownedPhoto(ctx, householdID, photoID); err != nil {
		return err
	}
	return s.albumPhotos.Remove(ctx, albumID, photoID)
}

// Reorder sets the album's full photo order. The album and every photo id in
// order are ownership-checked, so a foreign id is rejected as not-found instead
// of silently corrupting the order.
func (s *AlbumService) Reorder(ctx context.Context, householdID household.HouseholdID, albumID domain.AlbumID, order []domain.PhotoID) error {
	if _, err := s.ownedAlbum(ctx, householdID, albumID); err != nil {
		return err
	}
	for _, photoID := range order {
		if _, err := s.ownedPhoto(ctx, householdID, photoID); err != nil {
			return err
		}
	}
	return s.albumPhotos.Reorder(ctx, albumID, order)
}

// List returns the household's albums.
func (s *AlbumService) List(ctx context.Context, householdID household.HouseholdID) ([]*domain.Album, error) {
	return s.albums.ListByHousehold(ctx, householdID)
}

// Playlist returns the album's photos in display order, narrowed by the album's
// filter — the sequence the rotating viewer shows. Ownership-checked.
func (s *AlbumService) Playlist(ctx context.Context, householdID household.HouseholdID, albumID domain.AlbumID) ([]PlaylistItem, error) {
	album, err := s.ownedAlbum(ctx, householdID, albumID)
	if err != nil {
		return nil, err
	}
	photos, err := s.albumPhotos.ListByAlbumOrdered(ctx, albumID)
	if err != nil {
		return nil, err
	}
	items := make([]PlaylistItem, 0, len(photos))
	for _, p := range photos {
		if !album.Filter.Matches(*p) {
			continue
		}
		items = append(items, PlaylistItem{
			PhotoID:    p.ID,
			StorageRef: p.StorageRef,
			Caption:    p.Caption,
			TakenAt:    p.TakenAt,
			UploadedBy: p.UploadedBy,
		})
	}
	return items, nil
}

func (s *AlbumService) ownedAlbum(ctx context.Context, householdID household.HouseholdID, id domain.AlbumID) (*domain.Album, error) {
	album, err := s.albums.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if album.HouseholdID != householdID {
		return nil, domain.ErrAlbumNotFound
	}
	return album, nil
}

func (s *AlbumService) ownedPhoto(ctx context.Context, householdID household.HouseholdID, id domain.PhotoID) (*domain.Photo, error) {
	photo, err := s.photos.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if photo.HouseholdID != householdID {
		return nil, domain.ErrPhotoNotFound
	}
	return photo, nil
}
