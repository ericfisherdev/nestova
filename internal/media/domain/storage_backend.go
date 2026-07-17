package domain

import (
	"fmt"
	"strings"
)

// StorageBackend records which domain.PhotoStore implementation persisted a
// photo's bytes — a repository-level fact (see PhotoRepository/
// TaskInstancePhotoRepository's Create doc), not something a caller chooses
// per photo: the composition root selects one backend, app-wide, at startup
// (NES-132), and every repository instance is constructed bound to that
// same value, which Create stamps onto every row it writes.
type StorageBackend string

// StorageBackend values, mirroring the storage_backend CHECK constraint
// widened by migration 00032.
const (
	StorageBackendLocal StorageBackend = "local"
	StorageBackendS3    StorageBackend = "s3"
)

// Valid reports whether b is a known StorageBackend.
func (b StorageBackend) Valid() bool {
	switch b {
	case StorageBackendLocal, StorageBackendS3:
		return true
	default:
		return false
	}
}

// String returns b's storage/column spelling.
func (b StorageBackend) String() string { return string(b) }

// ParseStorageBackend parses a persisted storage_backend column value,
// rejecting anything the CHECK constraint should never have let through —
// a defensive boundary check (mirroring ParsePhotoKind's identical
// precedent) rather than trusting the database blindly.
func ParseStorageBackend(s string) (StorageBackend, error) {
	b := StorageBackend(strings.ToLower(strings.TrimSpace(s)))
	if !b.Valid() {
		return "", fmt.Errorf("media: unknown storage backend %q", s)
	}
	return b, nil
}
