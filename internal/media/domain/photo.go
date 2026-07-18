package domain

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Accepted image content types — the upload accept-list. Both Photo.Validate
// and the adapter's storage-extension mapping (LocalPhotoStore's
// acceptedTypes) key off these constants, so there is a single source of
// truth for "what image type does the upload path accept."
const (
	ContentTypeJPEG = "image/jpeg"
	ContentTypePNG  = "image/png"
	ContentTypeWebP = "image/webp"
)

// acceptedContentTypes is the set Photo.Validate checks ContentType against.
var acceptedContentTypes = map[string]struct{}{
	ContentTypeJPEG: {},
	ContentTypePNG:  {},
	ContentTypeWebP: {},
}

// contentHashPattern matches a hex-encoded sha256 sum: exactly 64 lowercase
// hex characters, the shape PhotoStore.Put always produces.
var contentHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

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
	// ErrDuplicatePhoto is returned by PhotoRepository.Create when a photo with
	// the same content hash already exists for the household (the
	// photo_household_content_hash_uniq index added in 00023). PhotoService
	// resolves it by fetching and returning the existing photo instead of
	// surfacing an error, so callers never see it directly.
	ErrDuplicatePhoto = errors.New("media: duplicate photo content")
)

// StorageRef is an opaque key identifying a photo's bytes in the PhotoStore. The
// bytes are never stored in the database.
type StorageRef string

// String returns the ref's string value.
func (r StorageRef) String() string { return string(r) }

// Photo is one household image. StorageRef points at the bytes behind the
// PhotoStore; ContentHash is the hex sha256 of those bytes (computed once,
// while streaming the upload to storage) and is what content-hash dedup keys
// on — empty for a photo uploaded before NES-123, which never matches a
// duplicate check. SizeBytes and ContentType are the other server-verified
// upload facts, recorded for a future NES-124 queue UI (zero/empty for a
// pre-NES-123 photo, same as ContentHash). TakenAt is the EXIF capture time
// (UTC) when the upload carried one; UploadedBy is the member who added it,
// nilled (not deleted) if that member is removed so the photo survives.
// StorageBackend (NES-132) is populated by PhotoRepository.Create from the
// repository's own configured backend, never by the caller — see that
// method's doc — so it is the zero value on a Photo the caller is still
// building, exactly like CreatedAt.
type Photo struct {
	ID             PhotoID
	HouseholdID    household.HouseholdID
	StorageRef     StorageRef
	ContentHash    string
	SizeBytes      int64
	ContentType    string
	TakenAt        *time.Time
	Caption        string
	UploadedBy     *household.MemberID
	CreatedAt      time.Time
	StorageBackend StorageBackend
}

// Validate reports whether the photo is well-formed, wrapping ErrInvalidPhoto.
// ContentHash, SizeBytes, and ContentType are all required in their canonical
// server-verified shape because every photo constructed by
// PhotoService.Upload has them (PhotoStore.Put always computes them) — a
// violation here signals a PhotoStore implementation bug, not a legitimate
// legacy photo (those are read back from storage, not built fresh through
// Validate, and may carry a zero/blank value for a field that predates it).
func (p Photo) Validate() error {
	if strings.TrimSpace(p.StorageRef.String()) == "" {
		return fmt.Errorf("%w: storage ref must not be blank", ErrInvalidPhoto)
	}
	if !contentHashPattern.MatchString(p.ContentHash) {
		return fmt.Errorf("%w: content hash must be a 64-character lowercase hex sha256, got %q", ErrInvalidPhoto, p.ContentHash)
	}
	if p.SizeBytes <= 0 {
		return fmt.Errorf("%w: size bytes must be positive, got %d", ErrInvalidPhoto, p.SizeBytes)
	}
	if _, ok := acceptedContentTypes[p.ContentType]; !ok {
		return fmt.Errorf("%w: content type %q is not accepted", ErrInvalidPhoto, p.ContentType)
	}
	return nil
}

// PhotoRepository persists photo metadata (not the bytes). Get returns
// ErrPhotoNotFound for an unknown id; a Create with an unknown HouseholdID
// returns household.ErrHouseholdNotFound, an unknown UploadedBy returns
// household.ErrMemberNotFound, and a content hash that collides with another
// household photo returns ErrDuplicatePhoto (all mapped from the tenant/unique
// constraint violations by the adapter). ListByHousehold returns an empty slice
// (not an error) when none match. FindByContentHash returns ErrPhotoNotFound
// when no household photo carries that hash — the expected "not a duplicate"
// outcome, not an exceptional one.
//
// Every implementation is constructed bound to ONE StorageBackend (NES-132),
// matching the composition root's single-backend-per-deployment selection
// (see StorageBackend's doc) — Create stamps that value onto photo.
// StorageBackend itself, ignoring whatever the caller may have left on the
// struct, so the column always reflects which backend genuinely wrote the
// bytes, never the column's DEFAULT by omission.
type PhotoRepository interface {
	Create(ctx context.Context, photo *Photo) error
	Get(ctx context.Context, id PhotoID) (*Photo, error)
	FindByContentHash(ctx context.Context, householdID household.HouseholdID, hash string) (*Photo, error)
	ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*Photo, error)
	Delete(ctx context.Context, id PhotoID) error

	// ListAllStorageRefs returns the StorageRef of every photo row stamped
	// with backend, across every household — the storage reaper's
	// (NES-132/133, ReaperService in media/app) source of truth for "which
	// album-class objects of THIS backend are still referenced." backend is
	// explicit, not implicitly bound to the repository's own configured
	// write backend (see the type doc): a repository instance serves READS
	// across every backend rows may be stamped with, mid-NES-133-migration
	// mixed state included — a query that ignored backend would let a
	// content-identical LOCAL row shield a genuine S3 orphan forever, since
	// StorageRef is content-addressed and therefore IDENTICAL across
	// backends for the same bytes. Bucket-wide, not household-scoped,
	// mirroring ObjectLister.ListObjects' identical scope; returns an empty
	// slice (not an error) when there are no matching photos.
	ListAllStorageRefs(ctx context.Context, backend StorageBackend) ([]StorageRef, error)

	// ExistsByStorageRef reports whether any photo row STAMPED WITH backend
	// currently references ref, across every household — a targeted,
	// single-ref query the storage reaper (ReaperService.sweepClass) runs
	// immediately before deleting an apparently-orphaned object, closing
	// the bulk of the TOCTOU window between the bulk ListAllStorageRefs
	// snapshot and the delete: a row referencing ref that commits in that
	// window (e.g. a restore re-inserting it) is caught by THIS check even
	// though it postdates the snapshot. backend is explicit for the same
	// content-addressed-collision reason ListAllStorageRefs' doc explains —
	// a local-backed row with the same ref must never protect a genuine S3
	// orphan (or vice versa). See ReaperService's doc for the residual,
	// deliberately-accepted window this does not close, and the operator
	// contract that makes it acceptable.
	ExistsByStorageRef(ctx context.Context, ref StorageRef, backend StorageBackend) (bool, error)
}
