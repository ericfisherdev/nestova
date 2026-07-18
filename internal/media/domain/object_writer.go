package domain

import (
	"context"
	"io"
)

// ObjectExister reports whether an object already exists at a given
// StorageRef without downloading it (an S3 HeadObject, for S3PhotoStore) —
// NES-133's storage migrator's idempotency check before uploading a
// content-addressed object a DIFFERENT row (same household+class+hash — see
// PhotoClass's dedup note) may have already migrated there. Only an
// object-store backend implements this; see ObjectLister's identical
// asymmetric-implementation rationale (LocalPhotoStore does not, since the
// migrator never checks existence against the local backend).
type ObjectExister interface {
	ObjectExists(ctx context.Context, ref StorageRef) (bool, error)
}

// RawObjectWriter uploads bytes the caller has ALREADY validated (content
// sniffed, decoded, hashed, size-capped — exactly what PhotoStore.Put's own
// validateAndStage did once already, at the photo's original local upload)
// directly to ref, verbatim, with no second validation pass. NES-133's
// storage migrator is the only intended caller: re-running Put's full
// validation pipeline a second time for every migrated photo would repeat
// work Put's contract does not require repeating, and — more fundamentally —
// Put always computes its OWN content-addressed key from the bytes it
// streams, which the migrator cannot use directly, since the object must
// land at a SPECIFIC key (the class-namespaced key the migrator itself
// derives from the row's household/class/content-hash — see
// mediaadapter.BuildStorageKey), not wherever Put's internal hashing would
// place it. Only an object-store backend implements this, mirroring
// ObjectExister's identical asymmetry.
type RawObjectWriter interface {
	PutAt(ctx context.Context, ref StorageRef, contentType string, r io.Reader) error
}
