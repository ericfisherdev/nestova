package domain

import (
	"context"
	"io"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// RandomAccessReader is a read source that also supports offset-based access —
// Read for sequential consumption, ReadAt/Seek for formats (EXIF/TIFF) whose
// internal structure is addressed by absolute byte offsets. Sharing this
// between PhotoStore.Open and ExifReader.TakenAt lets EXIF extraction read
// directly from the stored file instead of first buffering it into a []byte.
type RandomAccessReader interface {
	io.Reader
	io.ReaderAt
	io.Seeker
}

// PhotoReader is what PhotoStore.Open returns: a RandomAccessReader that must
// also be closed. A local-file adapter satisfies this for free (*os.File
// already supports ReadAt/Seek); an object-store adapter added later is free to
// satisfy it however it needs to (e.g. buffering a fetched range), since that
// choice stays internal to the adapter.
type PhotoReader interface {
	RandomAccessReader
	io.Closer
}

// PutResult is the outcome of storing an upload: the ref to persist on the
// Photo plus the facts PhotoStore.Put computes while streaming the bytes to
// storage — ContentHash (sha256, for content-hash dedup), SizeBytes, and the
// server-sniffed ContentType. ContentType is authoritative: it is derived from
// the bytes themselves (see PhotoStore.Put), never from a caller-supplied claim.
type PutResult struct {
	Ref         StorageRef
	ContentHash string
	SizeBytes   int64
	ContentType string
}

// PhotoStore persists and serves photo bytes behind a swappable port (a local
// filesystem adapter first; an object store later). Put streams r to storage —
// it never buffers the whole upload in memory — sniffing the true content type
// from the bytes themselves (a caller-declared type is never trusted), hashing
// and size-capping as it copies. Open streams the bytes back (for serving, and
// for EXIF extraction, which needs the RandomAccessReader half of PhotoReader);
// Delete removes them.
//
// Put error contract: ErrUnsupportedMediaType when the sniffed type is not an
// accepted image format, ErrPhotoTooLarge when r exceeds the configured limit,
// and ErrInvalidPhoto when the bytes do not actually decode as the sniffed
// type. Open returns ErrPhotoNotFound when ref is unknown.
type PhotoStore interface {
	Put(ctx context.Context, householdID household.HouseholdID, r io.Reader) (PutResult, error)
	Open(ctx context.Context, ref StorageRef) (PhotoReader, error)
	Delete(ctx context.Context, ref StorageRef) error
}

// ExifReader extracts the EXIF capture time from a photo's bytes. TakenAt
// returns the capture time normalized to UTC, or nil when the image carries no
// usable EXIF date (a missing tag is not an error — the photo is simply stored
// without a taken_at). r must support random access (see RandomAccessReader)
// because EXIF/TIFF fields are addressed by absolute byte offsets; a
// PhotoStore.Open result satisfies this directly, so no full-file buffering is
// needed just to extract the capture time.
type ExifReader interface {
	TakenAt(r RandomAccessReader) *time.Time
}
