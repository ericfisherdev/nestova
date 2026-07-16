package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// PhotoService handles uploading and deleting photos: it streams the bytes
// behind the PhotoStore, dedups by content hash, captures the EXIF date, and
// persists the photo metadata.
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

// UploadResult is the outcome of PhotoService.Upload: the photo that now
// represents the upload, and whether it was a re-drop of bytes the household
// already has (Duplicate) rather than a newly created row.
type UploadResult struct {
	Photo     *domain.Photo
	Duplicate bool
}

// Upload streams r to storage, dedups by content hash, captures the EXIF date,
// and persists the photo attributed to uploaderID.
//
// Stored objects are immutable and content-addressed, so the SAME StorageRef
// can legitimately be shared by more than one Photo row (identical bytes
// uploaded more than once — the common case Upload is optimizing for). That
// sharing means the upload path never synchronously deletes stored.Ref on a
// failure after Put succeeds: doing so could destroy bytes another photo row
// (this household's existing match, or a concurrent upload that is about to
// win the race below) still references, and there is no cheap, race-free way
// from here to prove nothing else points at it. A failure after Put therefore
// may leave an orphaned object behind — that is safe, not a leak: it has no
// row referencing it, and NES-132/133's planned storage verify/reaper finds
// and reclaims exactly this class of object after a grace window. Contrast
// with Delete, which removes a row this service instance just confirmed is
// the only owner and is expected to clean up its bytes.
//
// If a photo with the same content hash already exists for householdID, Upload
// returns that existing photo with Duplicate set — Put's write above was a
// harmless overwrite (content-addressed), so a re-drop of the same photo is a
// no-op. A duplicate detected by a concurrent upload racing this one (caught
// as domain.ErrDuplicatePhoto from the repository's unique index) resolves
// the same way.
//
// It returns the storage layer's validation errors (ErrUnsupportedMediaType,
// ErrPhotoTooLarge, ErrInvalidPhoto) unchanged.
func (s *PhotoService) Upload(ctx context.Context, householdID household.HouseholdID, uploaderID household.MemberID, r io.Reader, caption string) (UploadResult, error) {
	stored, err := s.store.Put(ctx, householdID, r)
	if err != nil {
		return UploadResult{}, err
	}

	if existing, err := s.photos.FindByContentHash(ctx, householdID, stored.ContentHash); err == nil {
		return UploadResult{Photo: existing, Duplicate: true}, nil
	} else if !errors.Is(err, domain.ErrPhotoNotFound) {
		return UploadResult{}, fmt.Errorf("check duplicate photo: %w", err)
	}

	taken, err := s.takenAt(ctx, stored.Ref)
	if err != nil {
		return UploadResult{}, err
	}

	uploader := uploaderID
	photo := &domain.Photo{
		ID:          domain.NewPhotoID(),
		HouseholdID: householdID,
		StorageRef:  stored.Ref,
		ContentHash: stored.ContentHash,
		SizeBytes:   stored.SizeBytes,
		ContentType: stored.ContentType,
		TakenAt:     taken,
		Caption:     strings.TrimSpace(caption),
		UploadedBy:  &uploader,
	}
	if err := photo.Validate(); err != nil {
		return UploadResult{}, err
	}
	if err := s.photos.Create(ctx, photo); err != nil {
		if errors.Is(err, domain.ErrDuplicatePhoto) {
			// Lost a race with a concurrent upload of the same bytes: fetch and
			// return the winner's row instead of surfacing an error.
			existing, findErr := s.photos.FindByContentHash(ctx, householdID, stored.ContentHash)
			if findErr != nil {
				return UploadResult{}, fmt.Errorf("resolve concurrent duplicate: %w", findErr)
			}
			return UploadResult{Photo: existing, Duplicate: true}, nil
		}
		return UploadResult{}, err
	}
	return UploadResult{Photo: photo}, nil
}

// takenAt reopens the just-stored bytes to extract the EXIF capture time.
// PhotoStore.Open returns a domain.PhotoReader, which the ExifReader consumes
// directly (via random access into the file) — no separate buffering step is
// needed to feed it.
func (s *PhotoService) takenAt(ctx context.Context, ref domain.StorageRef) (*time.Time, error) {
	rc, err := s.store.Open(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("reopen stored photo for exif: %w", err)
	}
	defer func() { _ = rc.Close() }()
	return s.exif.TakenAt(rc), nil
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

// OpenBytes streams a household photo's stored bytes after verifying ownership,
// returning domain.ErrPhotoNotFound for an unknown or cross-household id. It also
// returns the image content type (derived from the stored extension, which was
// set from the validated format at upload) so the web layer can serve the bytes
// with an explicit Content-Type instead of letting the browser sniff them. The
// web layer uses it to serve a photo only to its owning household.
func (s *PhotoService) OpenBytes(ctx context.Context, householdID household.HouseholdID, id domain.PhotoID) (io.ReadCloser, string, error) {
	photo, err := s.ownedPhoto(ctx, householdID, id)
	if err != nil {
		return nil, "", err
	}
	rc, err := s.store.Open(ctx, photo.StorageRef)
	if err != nil {
		return nil, "", err
	}
	return rc, contentTypeForRef(photo.StorageRef), nil
}

// contentTypeForRef maps a stored ref's extension to its image content type,
// falling back to application/octet-stream for an unexpected extension.
func contentTypeForRef(ref domain.StorageRef) string {
	switch strings.ToLower(filepath.Ext(ref.String())) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
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
