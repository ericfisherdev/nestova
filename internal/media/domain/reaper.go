package domain

import (
	"context"
	"time"
)

// ObjectInfo is one stored object as ObjectLister reports it.
type ObjectInfo struct {
	// Key is the object's storage key — directly comparable to a
	// PhotoRepository/TaskInstancePhotoRepository row's StorageRef, since
	// both are opaque strings drawn from the same key scheme (see
	// classKeyPrefix on the adapter side).
	Key StorageRef
	// LastModified is when the backend last wrote this object — the grace
	// window a storage reaper (see ReaperService, media/app) checks before
	// treating an unreferenced object as safe to delete: an object younger
	// than the grace window might be mid-upload (Put has written the bytes
	// but PhotoRepository.Create/TaskInstancePhotoRepository.Create has not
	// yet committed the referencing row) rather than genuinely orphaned.
	LastModified time.Time
}

// ObjectLister lists a PhotoStore backend's raw stored objects, class by
// class, for the storage reaper (NES-132/NES-133): only an object-store
// backend (S3PhotoStore) implements it. LocalPhotoStore does not — a local
// filesystem's own orphaned files are not this reaper's target; NES-132's
// "never delete synchronously" invariant (see PhotoService.Delete's doc)
// applies to both backends, but reclaiming the resulting orphans is, for
// now, an object-store-only concern, since only an object store can be
// listed cheaply without walking a filesystem tree the app process may not
// even have direct access to (a remote MinIO/Garage/R2 bucket).
//
// A PhotoStore backend that also implements ObjectLister opts into being
// reapable; ReaperService (media/app) type-asserts for it rather than this
// being folded into the base PhotoStore interface, keeping PhotoStore itself
// minimal for the (common) case of a caller that only ever needs Put/Open/
// Delete/URL.
type ObjectLister interface {
	// ListObjects returns every stored object under class's namespace,
	// across every household — the reaper operates bucket-wide, not
	// household-scoped, since orphan reclamation is a storage-housekeeping
	// concern, not a tenant-facing one. Returns an empty slice (not an
	// error) when the class has no stored objects yet.
	ListObjects(ctx context.Context, class PhotoClass) ([]ObjectInfo, error)
}
