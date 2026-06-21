package adapter

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// foreignKeyViolation is the Postgres SQLSTATE the media adapters map to domain
// sentinels.
const foreignKeyViolation = "23503"

// FK constraint names on the media tables (00017). The household FKs are inline
// column references, so Postgres auto-names them <table>_<column>_fkey; the
// uploader and album_photo FKs are the explicitly named composite tenant
// constraints.
const (
	albumHouseholdFK  = "album_household_id_fkey"
	photoHouseholdFK  = "photo_household_id_fkey"
	photoUploaderFK   = "photo_uploader_fk"
	albumPhotoAlbumFK = "album_photo_album_fk"
	albumPhotoPhotoFK = "album_photo_photo_fk"
)

// mapFKViolation maps a media FK violation to its domain sentinel, or nil when
// err is not a recognized FK violation.
func mapFKViolation(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != foreignKeyViolation {
		return nil
	}
	switch pgErr.ConstraintName {
	case albumHouseholdFK, photoHouseholdFK:
		return household.ErrHouseholdNotFound
	case photoUploaderFK:
		return household.ErrMemberNotFound
	case albumPhotoAlbumFK:
		return domain.ErrAlbumNotFound
	case albumPhotoPhotoFK:
		return domain.ErrPhotoNotFound
	default:
		return nil
	}
}

// memberArg renders an optional member id as a nullable text query argument.
func memberArg(id *household.MemberID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

// row is the read surface shared by pgx.Row and pgx.Rows for scan helpers.
type row interface {
	Scan(dest ...any) error
}
