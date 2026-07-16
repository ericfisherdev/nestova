package adapter

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// Postgres SQLSTATE codes the media adapters react to.
const (
	foreignKeyViolation = "23503"
	uniqueViolation     = "23505"
)

// albumPhotoPositionUniq is the UNIQUE (album_id, position) constraint; a
// violation signals two concurrent inserts raced on the next position.
const albumPhotoPositionUniq = "album_photo_position_uniq"

// photoHouseholdContentHashUniq is the partial UNIQUE (household_id,
// content_sha256) index added in 00023; a violation signals a concurrent
// upload of the same bytes won the race to insert first.
const photoHouseholdContentHashUniq = "photo_household_content_hash_uniq"

// isUniqueViolation reports whether err is a unique-constraint violation on the
// named constraint.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}

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

// nullableText renders s as a nullable text query argument, mapping a blank
// string to SQL NULL. Used for photo.content_sha256, which is NULL for a
// legacy (pre-NES-123) photo rather than an empty string — matching the
// photo_content_sha256_format CHECK, which rejects a stored value that is not
// a 64-character lowercase hex sha256.
func nullableText(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

// row is the read surface shared by pgx.Row and pgx.Rows for scan helpers.
type row interface {
	Scan(dest ...any) error
}
