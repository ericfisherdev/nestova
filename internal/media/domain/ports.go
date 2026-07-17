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
// filesystem adapter, or an object-store adapter — S3PhotoStore, NES-132).
// Put streams r to storage — it never buffers the whole upload in memory —
// sniffing the true content type from the bytes themselves (a
// caller-declared type is never trusted), hashing and size-capping as it
// copies. class namespaces the resulting StorageRef (see PhotoClass) so
// bytes uploaded for one purpose can never collide with, or be served as,
// another's — the calling context chooses class, never the store. Open
// streams the bytes back (for serving, and for EXIF extraction, which needs
// the RandomAccessReader half of PhotoReader); Delete removes them; URL
// returns a locator for already-stored bytes.
//
// Put error contract: ErrUnsupportedMediaType when the sniffed type is not an
// accepted image format, ErrPhotoTooLarge when r exceeds the configured limit,
// and ErrInvalidPhoto when the bytes do not actually decode as the sniffed
// type. Open and URL return ErrPhotoNotFound when ref is unknown.
type PhotoStore interface {
	Put(ctx context.Context, householdID household.HouseholdID, class PhotoClass, r io.Reader) (PutResult, error)
	Open(ctx context.Context, ref StorageRef) (PhotoReader, error)
	Delete(ctx context.Context, ref StorageRef) error

	// URL returns a locator for ref's stored bytes, or ErrPhotoNotFound when
	// ref is unknown. ttl bounds how long the locator stays valid; a backend
	// that cannot expire what it returns (LocalPhotoStore today) treats ttl as
	// advisory and ignores it. A non-positive ttl asks the backend to apply
	// its own configured default (S3PhotoStore's PresignTTL) rather than the
	// zero-duration URL a literal interpretation would produce.
	//
	// The two backends this port is designed for answer very differently: an
	// object-store adapter (NES-132) returns a presigned URL a client can
	// fetch directly, scoped by ref alone. LocalPhotoStore has no such direct
	// link — every existing serving route (e.g. GET /photos/{id}/raw) is
	// keyed by the caller's own domain id, not by StorageRef, specifically so
	// it can check household ownership before ever touching the store; ref
	// alone never carries enough context to reconstruct that route, and
	// fabricating a plausible-looking-but-unserved path here would be a
	// broken link masquerading as a working one. LocalPhotoStore.URL
	// therefore confirms ref resolves to a stored object and returns ref's
	// own string as a stable (non-navigable) locator — existing view code is
	// untouched and keeps building its own tenant-checked routes without
	// calling URL at all. See SupportsDirectURL for how a caller decides
	// which of these two behaviors to expect without knowing the concrete
	// backend type.
	URL(ctx context.Context, ref StorageRef, ttl time.Duration) (string, error)

	// SupportsDirectURL reports whether URL returns a browser-navigable
	// locator a caller may safely redirect a client to (an object-store
	// backend's presigned GET) as opposed to LocalPhotoStore's
	// non-navigable stable-string locator (see URL's doc). The application
	// layer's raw-serving seam (e.g. PhotoService.RawServe) reads this once,
	// at request time, to decide whether to redirect (SupportsDirectURL
	// true) or Open-and-stream through the Go process (false) — asking the
	// store itself, rather than threading a separately-configured "which
	// backend" flag through every consumer, means the two can never drift
	// out of sync.
	SupportsDirectURL() bool
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
