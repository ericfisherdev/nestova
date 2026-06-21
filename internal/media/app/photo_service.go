package app

import (
	"context"
	"errors"
	"strings"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// PhotoService handles uploading and deleting photos: it stores the bytes behind
// the PhotoStore, captures the EXIF date, and persists the photo metadata.
type PhotoService struct {
	store  domain.PhotoStore
	exif   domain.ExifReader
	photos domain.PhotoRepository
}

// NewPhotoService constructs the service with injected dependencies.
func NewPhotoService(store domain.PhotoStore, exif domain.ExifReader, photos domain.PhotoRepository) (*PhotoService, error) {
	switch {
	case store == nil:
		return nil, errors.New("media/app: NewPhotoService requires a non-nil PhotoStore")
	case exif == nil:
		return nil, errors.New("media/app: NewPhotoService requires a non-nil ExifReader")
	case photos == nil:
		return nil, errors.New("media/app: NewPhotoService requires a non-nil PhotoRepository")
	}
	return &PhotoService{store: store, exif: exif, photos: photos}, nil
}

// Upload validates and stores the bytes, captures the EXIF date, and persists the
// photo attributed to uploaderID. It returns the storage layer's validation
// errors (ErrUnsupportedMediaType, ErrPhotoTooLarge, ErrInvalidPhoto) unchanged.
// If persistence fails after the bytes are stored, the bytes are cleaned up.
func (s *PhotoService) Upload(ctx context.Context, householdID household.HouseholdID, uploaderID household.MemberID, data []byte, contentType, caption string) (*domain.Photo, error) {
	ref, err := s.store.Put(ctx, householdID, data, contentType)
	if err != nil {
		return nil, err
	}
	uploader := uploaderID
	photo := &domain.Photo{
		ID:          domain.NewPhotoID(),
		HouseholdID: householdID,
		StorageRef:  ref,
		TakenAt:     s.exif.TakenAt(data),
		Caption:     strings.TrimSpace(caption),
		UploadedBy:  &uploader,
	}
	if err := photo.Validate(); err != nil {
		s.cleanupBytes(ctx, ref)
		return nil, err
	}
	if err := s.photos.Create(ctx, photo); err != nil {
		// Roll back the stored bytes so a failed insert does not orphan a file.
		s.cleanupBytes(ctx, ref)
		return nil, err
	}
	return photo, nil
}

// Delete removes the photo (verifying it belongs to householdID) and its stored
// bytes. It returns domain.ErrPhotoNotFound for an unknown or cross-household id.
func (s *PhotoService) Delete(ctx context.Context, householdID household.HouseholdID, id domain.PhotoID) error {
	photo, err := s.ownedPhoto(ctx, householdID, id)
	if err != nil {
		return err
	}
	if err := s.photos.Delete(ctx, id); err != nil {
		return err
	}
	// The row (and its album memberships, via cascade) is gone; remove the bytes
	// best-effort so a storage hiccup does not resurrect the metadata.
	s.cleanupBytes(ctx, photo.StorageRef)
	return nil
}

// cleanupBytes deletes stored bytes best-effort during rollback/cleanup. It uses
// a context detached from cancellation so the cleanup still runs when the request
// context is already canceled or timed out (often the very reason cleanup is
// needed), avoiding an orphaned file.
func (s *PhotoService) cleanupBytes(ctx context.Context, ref domain.StorageRef) {
	_ = s.store.Delete(context.WithoutCancel(ctx), ref)
}

// List returns the household's photos.
func (s *PhotoService) List(ctx context.Context, householdID household.HouseholdID) ([]*domain.Photo, error) {
	return s.photos.ListByHousehold(ctx, householdID)
}

// ownedPhoto fetches a photo and confirms it belongs to householdID, returning
// domain.ErrPhotoNotFound otherwise so a tenant cannot probe another household.
func (s *PhotoService) ownedPhoto(ctx context.Context, householdID household.HouseholdID, id domain.PhotoID) (*domain.Photo, error) {
	photo, err := s.photos.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if photo.HouseholdID != householdID {
		return nil, domain.ErrPhotoNotFound
	}
	return photo, nil
}
