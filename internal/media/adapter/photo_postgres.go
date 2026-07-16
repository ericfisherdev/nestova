package adapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// PhotoRepository is the pgx-backed domain.PhotoRepository. It persists photo
// metadata only; the bytes live behind the PhotoStore.
type PhotoRepository struct {
	dbtx db.TX
}

var _ domain.PhotoRepository = (*PhotoRepository)(nil)

// NewPhotoRepository constructs the repository with an injected query executor.
func NewPhotoRepository(dbtx db.TX) *PhotoRepository {
	if dbtx == nil {
		panic("media/adapter: NewPhotoRepository requires a non-nil db.TX")
	}
	return &PhotoRepository{dbtx: dbtx}
}

const photoColumns = `SELECT id, household_id, storage_ref, content_sha256, size_bytes, content_type, taken_at, caption, uploaded_by, created_at FROM photo`

// Create inserts a photo and populates its created_at, mapping an unknown
// household to household.ErrHouseholdNotFound, an unknown uploader to
// household.ErrMemberNotFound, and a content hash that collides with another
// household photo (photo_household_content_hash_uniq, 00023) to
// domain.ErrDuplicatePhoto.
func (r *PhotoRepository) Create(ctx context.Context, photo *domain.Photo) error {
	if photo == nil {
		return errors.New("media/adapter: create photo: nil photo")
	}
	const q = `
		INSERT INTO photo (id, household_id, storage_ref, content_sha256, size_bytes, content_type, taken_at, caption, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING created_at`
	err := r.dbtx.QueryRow(ctx, q,
		photo.ID.String(), photo.HouseholdID.String(), photo.StorageRef.String(),
		nullableText(photo.ContentHash), photo.SizeBytes, photo.ContentType,
		photo.TakenAt, photo.Caption, memberArg(photo.UploadedBy),
	).Scan(&photo.CreatedAt)
	if err != nil {
		if isUniqueViolation(err, photoHouseholdContentHashUniq) {
			return domain.ErrDuplicatePhoto
		}
		if mapped := mapFKViolation(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("create photo: %w", err)
	}
	return nil
}

// Get returns the photo, or domain.ErrPhotoNotFound.
func (r *PhotoRepository) Get(ctx context.Context, id domain.PhotoID) (*domain.Photo, error) {
	photo, err := scanPhoto(r.dbtx.QueryRow(ctx, photoColumns+` WHERE id = $1`, id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPhotoNotFound
		}
		return nil, fmt.Errorf("get photo: %w", err)
	}
	return photo, nil
}

// FindByContentHash returns the household's photo carrying the given content
// hash, or domain.ErrPhotoNotFound when none matches — the expected "not a
// duplicate" outcome for a genuinely new upload, not an exceptional one. hash
// must be non-blank; a blank hash can never match (a stored content_sha256 is
// always a 64-character lowercase hex sha256 — see photo_content_sha256_format,
// 00023), so this short-circuits to ErrPhotoNotFound rather than issuing a
// query.
func (r *PhotoRepository) FindByContentHash(ctx context.Context, householdID household.HouseholdID, hash string) (*domain.Photo, error) {
	if strings.TrimSpace(hash) == "" {
		return nil, domain.ErrPhotoNotFound
	}
	photo, err := scanPhoto(r.dbtx.QueryRow(ctx, photoColumns+` WHERE household_id = $1 AND content_sha256 = $2`, householdID.String(), hash))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPhotoNotFound
		}
		return nil, fmt.Errorf("find photo by content hash: %w", err)
	}
	return photo, nil
}

// ListByHousehold returns the household's photos ordered by creation time, or an
// empty slice when none exist.
func (r *PhotoRepository) ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*domain.Photo, error) {
	rows, err := r.dbtx.Query(ctx, photoColumns+` WHERE household_id = $1 ORDER BY created_at`, householdID.String())
	if err != nil {
		return nil, fmt.Errorf("list photos: %w", err)
	}
	defer rows.Close()
	return scanPhotos(rows)
}

// Delete removes the photo (cascading its memberships), returning
// domain.ErrPhotoNotFound when the id is unknown.
func (r *PhotoRepository) Delete(ctx context.Context, id domain.PhotoID) error {
	tag, err := r.dbtx.Exec(ctx, `DELETE FROM photo WHERE id = $1`, id.String())
	if err != nil {
		return fmt.Errorf("delete photo: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrPhotoNotFound
	}
	return nil
}

func scanPhotos(rows pgx.Rows) ([]*domain.Photo, error) {
	photos := make([]*domain.Photo, 0)
	for rows.Next() {
		photo, err := scanPhoto(rows)
		if err != nil {
			return nil, fmt.Errorf("scan photo: %w", err)
		}
		photos = append(photos, photo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan photos: %w", err)
	}
	return photos, nil
}

func scanPhoto(r row) (*domain.Photo, error) {
	var (
		photo          domain.Photo
		idStr, hhStr   string
		storageRef     string
		contentHashPtr *string
		takenAt        *time.Time
		uploadedByStr  *string
	)
	if err := r.Scan(&idStr, &hhStr, &storageRef, &contentHashPtr, &photo.SizeBytes, &photo.ContentType, &takenAt, &photo.Caption, &uploadedByStr, &photo.CreatedAt); err != nil {
		return nil, err
	}
	id, err := domain.ParsePhotoID(idStr)
	if err != nil {
		return nil, fmt.Errorf("parse photo id: %w", err)
	}
	hh, err := household.ParseHouseholdID(hhStr)
	if err != nil {
		return nil, fmt.Errorf("parse household id: %w", err)
	}
	photo.ID = id
	photo.HouseholdID = hh
	photo.StorageRef = domain.StorageRef(storageRef)
	if contentHashPtr != nil {
		photo.ContentHash = *contentHashPtr
	}
	photo.TakenAt = takenAt
	if uploadedByStr != nil {
		memberID, err := household.ParseMemberID(*uploadedByStr)
		if err != nil {
			return nil, fmt.Errorf("parse uploader id: %w", err)
		}
		photo.UploadedBy = &memberID
	}
	return &photo, nil
}
